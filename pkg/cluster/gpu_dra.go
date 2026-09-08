package cluster

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

const gpuAgentNodePath = "/usr/local/libexec/kiac/kiac-gpu-agent"

func gpuAgentBinary() ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(gpuAgentLinuxARM64Gzip))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (m *Manager) installGPUDRA(cp string, cfg Config, gpuNodes []string) error {
	if err := requireDRAKubernetesVersion(cfg.K8sVersion); err != nil {
		return err
	}
	if err := ui.Step("Installing Apple GPU DRA agent", func() error {
		if err := m.ensureNVIDIARuntimeClass(cp, cfg.Distro); err != nil {
			return err
		}
		binary, err := gpuAgentBinary()
		if err != nil {
			return fmt.Errorf("reading embedded GPU agent: %w", err)
		}
		if err := inParallel(len(gpuNodes), func(i int) error {
			return m.rt.ExecStdin(gpuNodes[i], bytes.NewReader(binary), "sh", "-euc", `
install -d -m 0755 /usr/local/libexec/kiac /var/lib/kubelet/plugins /var/lib/kubelet/plugins_registry /etc/cdi
tmp="$(mktemp /usr/local/libexec/kiac/kiac-gpu-agent.XXXXXX)"
cat > "$tmp"
chmod 0755 "$tmp"
mv "$tmp" /usr/local/libexec/kiac/kiac-gpu-agent
`)
		}); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, cfg.Distro, "delete", "daemonset", "kiac-gpu-device-plugin", "-n", "kube-system", "--ignore-not-found"); err != nil {
			return err
		}
		if err := m.gpuKubectlStdin(cp, cfg.Distro, strings.NewReader(gpuDRAManifest), "apply", "-f", "-"); err != nil {
			return err
		}
		_, err = m.gpuKubectl(cp, cfg.Distro, "rollout", "status", "daemonset/kiac-gpu-dra",
			"-n", "kube-system", fmt.Sprintf("--timeout=%ds", int(cfg.WaitTimeout.Seconds())))
		return err
	}); err != nil {
		return err
	}
	if err := ui.Step("Waiting for DRA GPU inventory", func() error {
		return m.waitGPUDRA(cp, cfg.Distro, gpuNodes, cfg.WaitTimeout)
	}); err != nil {
		return err
	}
	ui.Infof("DRA driver gpu.kiac.dev maps %s to real Venus devices; CUDA/NVML are not emulated", gpuResourceName)
	return nil
}

func requireDRAKubernetesVersion(version string) error {
	major, minor, ok := kubernetesMajorMinor(version)
	if !ok || major != 1 || minor < 36 {
		return fmt.Errorf("--gpu-resource-driver dra requires Kubernetes 1.36 or newer; got %q", version)
	}
	return nil
}

func kubernetesMajorMinor(version string) (int, int, bool) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

type resourceSliceList struct {
	Items []struct {
		Spec struct {
			Driver   string `json:"driver"`
			NodeName string `json:"nodeName"`
			Devices  []struct {
				Name       string `json:"name"`
				Attributes map[string]struct {
					String *string `json:"string"`
				} `json:"attributes"`
			} `json:"devices"`
		} `json:"spec"`
	} `json:"items"`
}

func (m *Manager) waitGPUDRA(cp, distro string, gpuNodes []string, timeout time.Duration) error {
	want := make(map[string]bool, len(gpuNodes))
	for _, node := range gpuNodes {
		want[node] = true
	}
	deadline := time.Now().Add(timeout)
	last := "no ResourceSlices returned"
	for time.Now().Before(deadline) {
		out, err := m.gpuKubectl(cp, distro, "get", "resourceslices.resource.k8s.io", "-o", "json")
		if err == nil {
			var list resourceSliceList
			if err := json.Unmarshal([]byte(out), &list); err == nil {
				ready := map[string]bool{}
				for _, item := range list.Items {
					if item.Spec.Driver == "gpu.kiac.dev" && want[item.Spec.NodeName] && len(item.Spec.Devices) == 1 && item.Spec.Devices[0].Name == "venus-0" {
						ready[item.Spec.NodeName] = true
					}
				}
				if len(ready) == len(want) {
					return nil
				}
				last = fmt.Sprintf("%d/%d GPU nodes published a Venus ResourceSlice", len(ready), len(want))
			} else {
				last = err.Error()
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("DRA inventory did not become ready in %s: %s", timeout, last)
}
