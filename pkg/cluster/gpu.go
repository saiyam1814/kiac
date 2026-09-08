package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

const (
	gpuResourceDomain = "kiac.dev"
	gpuResourceName   = gpuResourceDomain + "/gpu"
)

// SupportedGPUResourceDrivers returns the Kubernetes allocation backends
// accepted by --gpu-resource-driver.
func SupportedGPUResourceDrivers() []string { return []string{"device-plugin", "dra"} }

func gpuConfigForDistro(cfg Config, distro string) Config {
	cfg.Distro = distro
	if cfg.GPUDriver == "" {
		cfg.GPUDriver = "device-plugin"
	}
	return cfg
}

func (m *Manager) installGPUResources(cp string, cfg Config, gpuNodes []string) error {
	if len(gpuNodes) == 0 {
		return nil
	}
	if err := ui.Step("Discovering real Apple GPU nodes", func() error {
		for _, node := range gpuNodes {
			state, err := m.krunkit.State(node)
			if err != nil {
				return err
			}
			if !state.GPU || state.GPUMemoryMiB <= 0 {
				return fmt.Errorf("GPU node %s has no recorded Venus memory window", node)
			}
			if _, err := m.rt.Exec(node, "test", "-c", "/dev/dri/renderD128"); err != nil {
				return fmt.Errorf("GPU node %s has no Venus render device: %w", node, err)
			}
			labels := []string{
				gpuResourceDomain + "/gpu.present=true",
				gpuResourceDomain + "/gpu.product=apple-silicon",
				gpuResourceDomain + "/gpu.memory=" + strconv.Itoa(state.GPUMemoryMiB),
				gpuResourceDomain + "/gpu.count=1",
				gpuResourceDomain + "/gpu.api=venus",
			}
			args := append([]string{"label", "node", node}, labels...)
			args = append(args, "--overwrite")
			if _, err := m.gpuKubectl(cp, cfg.Distro, args...); err != nil {
				return err
			}
			if _, err := m.gpuKubectl(cp, cfg.Distro, "annotate", "node", node,
				gpuResourceDomain+"/gpu.description=Virtio-GPU Venus backed by the host Apple GPU", "--overwrite"); err != nil {
				return err
			}
			if _, err := m.gpuKubectl(cp, cfg.Distro, "taint", "node", node,
				gpuResourceDomain+"/gpu=true:NoSchedule", "--overwrite"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if cfg.GPUDriver == "dra" {
		return m.installGPUDRA(cp, cfg, gpuNodes)
	}
	if err := ui.Step("Installing Apple GPU device plugin", func() error {
		if err := m.ensureNVIDIARuntimeClass(cp, cfg.Distro); err != nil {
			return err
		}
		if err := m.gpuKubectlStdin(cp, cfg.Distro, strings.NewReader(gpuDevicePluginManifest), "apply", "-f", "-"); err != nil {
			return err
		}
		_, err := m.gpuKubectl(cp, cfg.Distro, "rollout", "status", "daemonset/kiac-gpu-device-plugin",
			"-n", "kube-system", fmt.Sprintf("--timeout=%ds", int(cfg.WaitTimeout.Seconds())))
		return err
	}); err != nil {
		return err
	}

	if err := ui.Step("Waiting for schedulable GPU capacity", func() error {
		return m.waitGPUCapacity(cp, cfg.Distro, gpuNodes, cfg.WaitTimeout)
	}); err != nil {
		return err
	}
	ui.Infof("resource %s is ready on %d node(s); CUDA/NVML are not emulated", gpuResourceName, len(gpuNodes))
	return nil
}

func (m *Manager) ensureNVIDIARuntimeClass(cp, distro string) error {
	handler, err := m.gpuKubectl(cp, distro, "get", "runtimeclass", "nvidia", "--ignore-not-found", "-o", "jsonpath={.handler}")
	if err != nil {
		return err
	}
	if err := validateNVIDIARuntimeHandler(strings.TrimSpace(handler), distro); err != nil {
		return err
	}
	switch strings.TrimSpace(handler) {
	case "":
		return m.gpuKubectlStdin(cp, distro, strings.NewReader(gpuRuntimeClassManifest), "apply", "-f", "-")
	default:
		return nil
	}
}

func validateNVIDIARuntimeHandler(handler, distro string) error {
	switch handler {
	case "", "runc":
		return nil
	case "nvidia":
		if distro == "k3s" {
			return nil
		}
		return fmt.Errorf("RuntimeClass nvidia already uses unsupported immutable handler %q for %s", handler, distro)
	default:
		return fmt.Errorf("RuntimeClass nvidia already uses unsupported immutable handler %q", handler)
	}
}

type gpuCapacityNodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Capacity map[string]string `json:"capacity"`
		} `json:"status"`
	} `json:"items"`
}

func (m *Manager) waitGPUCapacity(cp, distro string, gpuNodes []string, timeout time.Duration) error {
	want := make(map[string]bool, len(gpuNodes))
	for _, node := range gpuNodes {
		want[node] = true
	}
	deadline := time.Now().Add(timeout)
	last := "no node capacity returned"
	for time.Now().Before(deadline) {
		out, err := m.gpuKubectl(cp, distro, "get", "nodes", "-l", gpuResourceDomain+"/gpu.present=true", "-o", "json")
		if err == nil {
			var list gpuCapacityNodeList
			if err := json.Unmarshal([]byte(out), &list); err == nil {
				ready := 0
				for _, node := range list.Items {
					if want[node.Metadata.Name] && node.Status.Capacity[gpuResourceName] == "1" {
						ready++
					}
				}
				if ready == len(want) {
					return nil
				}
				last = fmt.Sprintf("%d/%d GPU nodes advertise %s", ready, len(want), gpuResourceName)
			} else {
				last = err.Error()
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("GPU capacity did not register in %s: %s", timeout, last)
}

// waitInstalledGPUResources follows the allocation driver already installed
// in the cluster. DRA deliberately does not add kiac.dev/gpu to Node capacity,
// so resume must wait for its DaemonSet and ResourceSlices instead.
func (m *Manager) waitInstalledGPUResources(cp, distro string, gpuNodes []string, timeout time.Duration) error {
	dra, err := m.gpuKubectl(cp, distro, "get", "daemonset", "kiac-gpu-dra", "-n", "kube-system", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(dra) != "" {
		if _, err := m.gpuKubectl(cp, distro, "rollout", "status", "daemonset/kiac-gpu-dra", "-n", "kube-system", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
			return err
		}
		return m.waitGPUDRA(cp, distro, gpuNodes, timeout)
	}
	plugin, err := m.gpuKubectl(cp, distro, "get", "daemonset", "kiac-gpu-device-plugin", "-n", "kube-system", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(plugin) == "" {
		return fmt.Errorf("cluster has GPU nodes but no kiac GPU resource driver")
	}
	if _, err := m.gpuKubectl(cp, distro, "rollout", "status", "daemonset/kiac-gpu-device-plugin", "-n", "kube-system", fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))); err != nil {
		return err
	}
	return m.waitGPUCapacity(cp, distro, gpuNodes, timeout)
}

// reconcileInstalledGPUResources upgrades the selected in-cluster driver to
// the manifests and embedded agent shipped by the running Kiac binary. Driver
// selection remains stable: resume never switches device-plugin and DRA.
func (m *Manager) reconcileInstalledGPUResources(cp, distro string, gpuNodes []string, timeout time.Duration) error {
	timeoutArg := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	dra, err := m.gpuKubectl(cp, distro, "get", "daemonset", "kiac-gpu-dra", "-n", "kube-system", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(dra) != "" {
		if err := m.ensureNVIDIARuntimeClass(cp, distro); err != nil {
			return err
		}
		if err := inParallel(len(gpuNodes), func(i int) error {
			return m.uploadGPUAgent(gpuNodes[i])
		}); err != nil {
			return err
		}
		if err := m.gpuKubectlStdin(cp, distro, strings.NewReader(gpuDRAManifest), "apply", "-f", "-"); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "rollout", "restart", "daemonset/kiac-gpu-dra", "-n", "kube-system"); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "rollout", "status", "daemonset/kiac-gpu-dra", "-n", "kube-system", timeoutArg); err != nil {
			return err
		}
	} else {
		plugin, err := m.gpuKubectl(cp, distro, "get", "daemonset", "kiac-gpu-device-plugin", "-n", "kube-system", "--ignore-not-found", "-o", "name")
		if err != nil {
			return err
		}
		if strings.TrimSpace(plugin) == "" {
			return fmt.Errorf("cluster has GPU nodes but no kiac GPU resource driver")
		}
		if err := m.ensureNVIDIARuntimeClass(cp, distro); err != nil {
			return err
		}
		if err := m.gpuKubectlStdin(cp, distro, strings.NewReader(gpuDevicePluginManifest), "apply", "-f", "-"); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "rollout", "restart", "daemonset/kiac-gpu-device-plugin", "-n", "kube-system"); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "rollout", "status", "daemonset/kiac-gpu-device-plugin", "-n", "kube-system", timeoutArg); err != nil {
			return err
		}
	}

	compat, err := m.gpuKubectl(cp, distro, "get", "deployment", gpuCompatName, "-n", "kube-system", "--ignore-not-found", "-o", "name")
	if err != nil {
		return err
	}
	if strings.TrimSpace(compat) != "" {
		if err := m.uploadGPUAgent(cp); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "rollout", "restart", "deployment/"+gpuCompatName, "-n", "kube-system"); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, distro, "rollout", "status", "deployment/"+gpuCompatName, "-n", "kube-system", timeoutArg); err != nil {
			return err
		}
	}
	return m.waitInstalledGPUResources(cp, distro, gpuNodes, timeout)
}

func (m *Manager) gpuKubectl(cp, distro string, args ...string) (string, error) {
	base := []string{"kubectl"}
	if distro == "kubeadm" {
		base = append(base, "--kubeconfig", adminConf)
	}
	return m.rt.Exec(cp, append(base, args...)...)
}

func (m *Manager) gpuKubectlStdin(cp, distro string, input io.Reader, args ...string) error {
	base := []string{"kubectl"}
	if distro == "kubeadm" {
		base = append(base, "--kubeconfig", adminConf)
	}
	return m.rt.ExecStdin(cp, input, append(base, args...)...)
}
