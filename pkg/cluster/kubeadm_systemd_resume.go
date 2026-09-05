package cluster

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

// resumeKubeadmSystemd recovers kubeadm clusters whose Fedora VMs are
// persistent krunkit processes. It mirrors the mature apple/container heal,
// but uses SSH readiness and Fedora's ordinary systemd units.
func (m *Manager) resumeKubeadmSystemd(name string, infos []runtime.Info, waitTimeout time.Duration, started time.Time) error {
	cp, regularWorkers, gpuWorkers, err := orderNodeGroups(name, infos)
	if err != nil {
		return err
	}
	workers := append(append([]string(nil), regularWorkers...), gpuWorkers...)
	nodes := append([]string{cp}, workers...)
	if err := ui.Step("Preflight checks for Apple GPU VMs", m.krunkit.Preflight); err != nil {
		return err
	}

	running := make(map[string]bool, len(infos))
	var changed atomic.Bool
	for _, info := range infos {
		running[info.Name] = strings.EqualFold(info.Status, "running")
		if !running[info.Name] {
			changed.Store(true)
		}
	}
	if err := ui.Step(fmt.Sprintf("Booting %d kubeadm node VM(s)", len(nodes)), func() error {
		for _, node := range nodes {
			if !running[node] {
				if err := m.rt.Start(node); err != nil {
					return err
				}
			}
		}
		return inParallel(len(nodes), func(i int) error {
			node := nodes[i]
			if err := m.rt.WaitReady(node, waitTimeout); err != nil {
				return err
			}
			if _, err := m.rt.Exec(node, "sh", "-euc", "swapoff -a; "+senderOffloadFix); err != nil {
				return err
			}
			nodeIP, err := m.rt.IP(node)
			if err != nil {
				return err
			}
			updated, err := m.setKubeletSystemdNodeIP(node, nodeIP)
			if updated {
				changed.Store(true)
			}
			return err
		})
	}); err != nil {
		return err
	}

	newIP, err := m.rt.IP(cp)
	if err != nil {
		return err
	}
	if net.ParseIP(newIP).To4() == nil {
		return fmt.Errorf("kubeadm control-plane address %q is not IPv4", newIP)
	}
	adminRaw, err := m.rt.Exec(cp, "cat", adminConf)
	if err != nil {
		return err
	}
	oldIP, err := apiServerIP(adminRaw)
	if err != nil {
		return err
	}
	if oldIP != newIP {
		changed.Store(true)
		ui.Infof("control-plane IP changed %s -> %s; healing kubeadm state", oldIP, newIP)
		if err := ui.Step("Healing control-plane files", func() error {
			_, err := m.rt.Exec(cp, "sh", "-euc", healControlPlaneScript(oldIP, newIP))
			return err
		}); err != nil {
			return err
		}
	} else if _, err := m.rt.Exec(cp, "systemctl", "is-active", "--quiet", "kubelet.service"); err != nil {
		if _, err := m.rt.Exec(cp, "systemctl", "restart", "kubelet.service"); err != nil {
			return err
		}
	}
	if err := ui.Step("Waiting for the API server", func() error {
		return m.waitAPIServer(cp, waitTimeout)
	}); err != nil {
		return err
	}

	if len(workers) > 0 {
		if err := ui.Step("Pointing worker kubelets at the control plane", func() error {
			return inParallel(len(workers), func(i int) error {
				_, err := m.rt.Exec(workers[i], "sh", "-euc", healWorkerScript(newIP))
				return err
			})
		}); err != nil {
			return err
		}
	}
	if err := ui.Step("Re-pointing kube-proxy and cluster-info", func() error {
		var lastErr error
		for attempt := 0; attempt < 6; attempt++ {
			if _, lastErr = m.rt.Exec(cp, "sh", "-euc", healConfigMapsScript(newIP)); lastErr == nil {
				return nil
			}
			time.Sleep(5 * time.Second)
		}
		return lastErr
	}); err != nil {
		return err
	}
	if err := ui.Step("Waiting for current kubeadm node addresses", func() error {
		return m.waitKubeadmNodeAddresses(cp, nodes, waitTimeout)
	}); err != nil {
		return err
	}
	if changed.Load() {
		if err := ui.Step("Refreshing node-addressed pods", func() error {
			return m.refreshKubeadmSystemdPods(cp, waitTimeout)
		}); err != nil {
			return err
		}
	}
	if len(gpuWorkers) > 0 {
		if err := ui.Step("Reconciling GPU resource driver", func() error {
			return m.reconcileInstalledGPUResources(cp, "kubeadm", gpuWorkers, waitTimeout)
		}); err != nil {
			return err
		}
	}
	if err := ui.Step("Waiting for built-in addons", func() error {
		return m.waitSystemdManagedAddons(cp, "kubeadm", waitTimeout)
	}); err != nil {
		return err
	}
	if err := ui.Step("Healing kubeadm networking helpers", func() error {
		if err := m.healEdgeProxySystemd(cp, name, adminConf, nodes); err != nil {
			return err
		}
		if _, err := m.rt.Exec(cp, "test", "-x", kiacLBScriptPath); err == nil {
			_, err = m.rt.Exec(cp, "systemctl", "restart", "kiac-lb.service")
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	var kubeconfigPath string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", adminConf)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(name, raw, newIP)
		return err
	}); err != nil {
		return err
	}
	reachable := false
	_ = ui.Step("Checking API server reachability from your Mac", func() error {
		reachable = hostReachAPI(newIP, 10*time.Second)
		if !reachable {
			return fmt.Errorf("host cannot reach %s:6443", newIP)
		}
		return nil
	})

	ui.Successf("kubeadm cluster %q resumed in %s.", name, time.Since(started).Round(time.Second))
	ui.Infof("context kiac-%s updated in %s", name, kubeconfigPath)
	if reachable {
		ui.Hintf("kubectl get nodes")
	} else {
		warnGPUAPIUnreachable(name, newIP)
	}
	return nil
}

func (m *Manager) setKubeletSystemdNodeIP(node, nodeIP string) (bool, error) {
	if net.ParseIP(nodeIP).To4() == nil {
		return false, fmt.Errorf("kubeadm node %s has invalid IPv4 address %q", node, nodeIP)
	}
	desired := "KUBELET_EXTRA_ARGS=--fail-swap-on=false --node-ip=" + nodeIP
	current, err := m.rt.Exec(node, "sh", "-c", "cat /etc/sysconfig/kubelet 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) == desired {
		return false, nil
	}
	if err := m.rt.ExecStdin(node, strings.NewReader(desired+"\n"), "sh", "-euc", `
tmp="$(mktemp /etc/sysconfig/kubelet.XXXXXX)"
cat > "$tmp"
chmod 0600 "$tmp"
mv "$tmp" /etc/sysconfig/kubelet
systemctl restart kubelet.service
`); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) waitKubeadmNodeAddresses(cp string, nodes []string, timeout time.Duration) error {
	expected := make(map[string]string, len(nodes))
	for _, node := range nodes {
		ip, err := m.rt.IP(node)
		if err != nil {
			return err
		}
		expected[node] = ip
	}
	deadline := time.Now().Add(timeout)
	last := "no node state returned"
	for time.Now().Before(deadline) {
		out, err := m.diagnosticKubectl(cp, "kubeadm", 15*time.Second, "get", "nodes", "-o", "json")
		if err == nil {
			var state k3sNodeStateList
			if err := json.Unmarshal([]byte(out), &state); err == nil {
				if ok, detail := k3sNodeAddressesCurrent(state, expected); ok {
					return nil
				} else {
					last = detail
				}
			} else {
				last = err.Error()
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("kubeadm nodes did not register their current addresses in %s: %s", timeout, last)
}

func (m *Manager) refreshKubeadmSystemdPods(cp string, timeout time.Duration) error {
	timeoutArg := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	datapathFound := false
	for _, daemonSet := range []string{"kindnet", "cilium"} {
		name, err := m.gpuKubectl(cp, "kubeadm", "-n", "kube-system", "get", "daemonset", daemonSet, "--ignore-not-found", "-o", "name")
		if err != nil {
			return err
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		datapathFound = true
		if _, err := m.gpuKubectl(cp, "kubeadm", "-n", "kube-system", "rollout", "restart", "daemonset/"+daemonSet); err != nil {
			return err
		}
		if _, err := m.gpuKubectl(cp, "kubeadm", "-n", "kube-system", "rollout", "status", "daemonset/"+daemonSet, timeoutArg); err != nil {
			return err
		}
	}
	if !datapathFound {
		return fmt.Errorf("cannot refresh kubeadm node addresses: neither kindnet nor Cilium is installed")
	}
	return m.refreshOptionalNodeExporter(cp, "kubeadm", timeout)
}
