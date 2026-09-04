package cluster

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

const gpuResourceDomain = "kiac.dev"

// GPUResourceName is the extended resource advertised by the GPU alpha.
const GPUResourceName = gpuResourceDomain + "/gpu"

const (
	gpuNamespace       = "kiac-gpu-system"
	gpuDevicePlugin    = "kiac-gpu-device-plugin"
	gpuLabelPresent    = gpuResourceDomain + "/gpu.present"
	gpuLabelProduct    = gpuResourceDomain + "/gpu.product"
	gpuLabelMemory     = gpuResourceDomain + "/gpu.memory"
	gpuLabelCount      = gpuResourceDomain + "/gpu.count"
	gpuLabelAPI        = gpuResourceDomain + "/gpu.api"
	gpuMockDevicePath  = "/dev/kiac-mock-gpu"
	gpuRuntimeClass    = "kiac-gpu"
	defaultGPUWaitTime = 5 * time.Minute
)

// installGPUMock exposes one synthetic extended resource on every current
// node. It proves chart scheduling and runtime contracts on ordinary kiac
// clusters; /dev/null is mounted at gpuMockDevicePath and no GPU is exposed.
func (m *Manager) installGPUMock(cp string, cfg Config, kubeconfig string) error {
	manifest := strings.Join(gpuMockManifests(), "\n---\n")
	if err := ui.Step("Installing mock GPU scheduling", func() error {
		return m.rt.ExecStdin(cp, strings.NewReader(manifest),
			"kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	}); err != nil {
		return err
	}

	if err := ui.Step("Labeling mock GPU nodes", func() error {
		_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", kubeconfig,
			"label", "nodes", "--all",
			gpuLabelPresent+"=true",
			gpuLabelProduct+"=Mock",
			gpuLabelMemory+"=0",
			gpuLabelCount+"=1",
			gpuLabelAPI+"=mock",
			"--overwrite")
		return err
	}); err != nil {
		return err
	}

	timeout := cfg.WaitTimeout
	if timeout <= 0 {
		timeout = defaultGPUWaitTime
	}
	if err := ui.Step("Waiting for mock GPU capacity", func() error {
		started := time.Now()
		if _, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", kubeconfig,
			"rollout", "status", "daemonset/"+gpuDevicePlugin, "-n", gpuNamespace,
			"--timeout="+timeout.String()); err != nil {
			return err
		}
		remaining := timeout - time.Since(started)
		if remaining <= 0 {
			return fmt.Errorf("mock GPU device plugin became ready after the %s timeout", timeout)
		}
		return m.waitForGPUMockCapacity(cp, kubeconfig, 1+cfg.Workers, remaining)
	}); err != nil {
		return err
	}

	ui.Infof("Mock GPU scheduling ready: %s=1 on %d node(s); no hardware acceleration is enabled", GPUResourceName, 1+cfg.Workers)
	ui.Hintf("kubectl get nodes -l %s=true -o custom-columns=NAME:.metadata.name,GPU:.status.capacity.%s,API:.metadata.labels.%s", gpuLabelPresent, escapedJSONPathKey(GPUResourceName), escapedJSONPathKey(gpuLabelAPI))
	return nil
}

func (m *Manager) installGPU(cp string, cfg Config) error {
	return m.installGPUMock(cp, cfg, adminConf)
}

func (m *Manager) installGPUK3s(cp string, cfg Config) error {
	return m.installGPUMock(cp, cfg, k3sKubeconfig)
}

func (m *Manager) waitForGPUMockCapacity(cp, kubeconfig string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "no labeled nodes reported capacity"
	for {
		out, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", kubeconfig,
			"get", "nodes", "-l", gpuLabelPresent+"=true", "-o", "json")
		if err == nil {
			var nodes kubeNodeList
			if err := json.Unmarshal([]byte(out), &nodes); err != nil {
				last = "cannot parse node capacity: " + err.Error()
			} else {
				ready := 0
				for _, node := range nodes.Items {
					if positiveResourceQuantity(node.Status.Capacity[GPUResourceName]) {
						ready++
					}
				}
				last = fmt.Sprintf("%d/%d node(s) advertise %s", ready, want, GPUResourceName)
				if len(nodes.Items) == want && ready == want {
					return nil
				}
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mock GPU capacity was not ready within %s: %s; check: kubectl get pods -n %s -o wide", timeout, compact(last), gpuNamespace)
		}
		time.Sleep(2 * time.Second)
	}
}

func positiveResourceQuantity(value string) bool {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return err == nil && n > 0
}

// escapedJSONPathKey turns a qualified key into kubectl's custom-columns key
// syntax, where dots in map keys must be escaped but the slash is literal.
func escapedJSONPathKey(key string) string {
	return strings.ReplaceAll(key, ".", `\.`)
}
