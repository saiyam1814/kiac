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
	"gopkg.in/yaml.v3"
)

const (
	k3sResumeLauncherPath = "/usr/local/bin/k3s"
	k3sResumeServerURL    = "/etc/kiac/k3s-server-url"
	k3sEdgeProxyMarker    = "/etc/kiac/edge-proxy-server-url"
)

// k3sResumeLauncher is intentionally transparent for every command except an
// agent boot. /usr/local/bin precedes /bin in rancher/k3s's PATH, while the
// kubectl/crictl/ctr multicall symlinks still point directly at /bin/k3s.
const k3sResumeLauncher = `#!/bin/sh
# Installed by kiac: refresh the k3s agent endpoint after vmnet changes it.
if [ "${1:-}" = "agent" ] && [ -r ` + k3sResumeServerURL + ` ]; then
  K3S_URL="$(cat ` + k3sResumeServerURL + `)"
  export K3S_URL
fi
exec /bin/k3s "$@"
`

const installK3sResumeHookScript = `url="$1"
case "$url" in https://*:6443) ;; *) echo "invalid k3s server URL: $url" >&2; exit 1;; esac
install -d -m 0700 /etc/kiac
install -d -m 0755 /usr/local/bin
if [ -e ` + k3sResumeLauncherPath + ` ] && ! grep -q 'Installed by kiac: refresh the k3s agent endpoint' ` + k3sResumeLauncherPath + `; then
  target="$(readlink -f ` + k3sResumeLauncherPath + ` 2>/dev/null || true)"
  if [ "$target" != /bin/k3s ]; then
    echo "` + k3sResumeLauncherPath + ` already exists and is not kiac-managed" >&2
    exit 1
  fi
fi
launcher="$(mktemp ` + k3sResumeLauncherPath + `.XXXXXX)"
cat > "$launcher"
chmod 0755 "$launcher"
mv "$launcher" ` + k3sResumeLauncherPath + `
urltmp="$(mktemp ` + k3sResumeServerURL + `.XXXXXX)"
printf '%s\n' "$url" > "$urltmp"
chmod 0600 "$urltmp"
mv "$urltmp" ` + k3sResumeServerURL + `
`

// resumeK3s restores a k3s server first, then points each agent at its current
// vmnet address. Existing clusters are migrated in place by installing the
// launcher after their first boot; a second worker boot is needed only when
// the live process still carries the old K3S_URL.
func (m *Manager) resumeK3s(name string, infos []runtime.Info, waitTimeout time.Duration, started time.Time) error {
	krunkit, err := krunkitOnlyCluster(infos)
	if err != nil {
		return err
	}
	if krunkit {
		return m.resumeK3sSystemd(name, infos, waitTimeout, started)
	}

	cp, workers, err := orderNodes(name, infos)
	if err != nil {
		return err
	}
	if err := ui.Step("Preflight checks", func() error { return m.preflight(false) }); err != nil {
		return err
	}

	running := map[string]bool{}
	nodesRestarted := false
	for _, info := range infos {
		running[info.Name] = strings.EqualFold(info.Status, "running")
		if !running[info.Name] {
			nodesRestarted = true
		}
	}
	if err := ui.Step("Booting k3s server VM", func() error {
		if !running[cp] {
			if err := m.rt.Start(cp); err != nil {
				return err
			}
		}
		return m.waitK3sAPI(cp, waitTimeout)
	}); err != nil {
		return err
	}

	serverIP, err := m.rt.IP(cp)
	if err != nil {
		return err
	}
	if parsed := net.ParseIP(serverIP); parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("k3s server address %q is not IPv4", serverIP)
	}
	targetURL := "https://" + serverIP + ":6443"

	if len(workers) > 0 {
		var agentRestarted atomic.Bool
		if err := ui.Step(fmt.Sprintf("Reconnecting %d k3s agent(s)", len(workers)), func() error {
			return inParallel(len(workers), func(i int) error {
				node := workers[i]
				if !running[node] {
					if err := m.rt.Start(node); err != nil {
						return err
					}
				}
				if err := m.waitK3sGuest(node, waitTimeout); err != nil {
					return err
				}
				liveURL, err := m.k3sAgentLiveURL(node)
				if err != nil {
					return err
				}
				if err := m.installK3sResumeHook(node, targetURL); err != nil {
					return err
				}
				if normalizeServerURL(liveURL) == targetURL {
					return nil
				}
				agentRestarted.Store(true)
				if err := m.rt.Stop(node); err != nil {
					return err
				}
				if err := m.rt.Start(node); err != nil {
					return err
				}
				if err := m.waitK3sGuest(node, waitTimeout); err != nil {
					return err
				}
				liveURL, err = m.k3sAgentLiveURL(node)
				if err != nil {
					return err
				}
				if normalizeServerURL(liveURL) != targetURL {
					return fmt.Errorf("k3s agent %s restarted with server %q, want %q", node, liveURL, targetURL)
				}
				return nil
			})
		}); err != nil {
			return err
		}
		nodesRestarted = nodesRestarted || agentRestarted.Load()
	}

	nodes := append([]string{cp}, workers...)
	if err := ui.Step("Waiting for current k3s node addresses", func() error {
		return m.waitK3sNodeAddresses(cp, nodes, waitTimeout)
	}); err != nil {
		return err
	}
	if nodesRestarted {
		if err := ui.Step("Refreshing node-addressed pods", func() error {
			return m.refreshK3sNodePods(cp, waitTimeout)
		}); err != nil {
			return err
		}
	}

	if err := ui.Step("Healing k3s networking helpers", func() error {
		return m.healK3sEdgeProxy(nodes, targetURL)
	}); err != nil {
		return err
	}

	var kubeconfigPath string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", k3sKubeconfig)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(name, raw, serverIP)
		return err
	}); err != nil {
		return err
	}

	reachable := false
	_ = ui.Step("Checking API server reachability from your Mac", func() error {
		reachable = hostReachAPI(serverIP, 10*time.Second)
		if !reachable {
			return fmt.Errorf("host cannot reach %s:6443", serverIP)
		}
		return nil
	})

	ui.Successf("k3s cluster %q resumed in %s.", name, time.Since(started).Round(time.Second))
	ui.Infof("context kiac-%s updated in %s", name, kubeconfigPath)
	if reachable {
		ui.Hintf("kubectl get nodes")
	} else {
		warnGPUAPIUnreachable(name, serverIP)
	}
	return nil
}

// krunkitOnlyCluster identifies the Fedora/systemd lifecycle. GPU clusters use
// krunkit for every node because apple/container's vmnet and vmnet-helper do
// not provide reliable bidirectional routing between their private networks.
func krunkitOnlyCluster(infos []runtime.Info) (bool, error) {
	krunkit := 0
	for _, info := range infos {
		if info.Backend == runtime.BackendKrunkit {
			krunkit++
		}
	}
	if krunkit == 0 {
		return false, nil
	}
	if krunkit != len(infos) {
		return false, fmt.Errorf("cluster mixes apple/container and krunkit nodes; recreate it so every GPU-cluster node uses krunkit")
	}
	return true, nil
}

// resumeK3sSystemd reconciles persistent Fedora VMs. K3s is a systemd service
// here, not PID 1, and both node-ip and the agent server endpoint live in the
// YAML config. Rewriting the structured config before restarting avoids
// carrying a stale vmnet-helper lease into kubelet or Flannel.
func (m *Manager) resumeK3sSystemd(name string, infos []runtime.Info, waitTimeout time.Duration, started time.Time) error {
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
	var nodesChanged atomic.Bool
	for _, info := range infos {
		running[info.Name] = strings.EqualFold(info.Status, "running")
		if !running[info.Name] {
			nodesChanged.Store(true)
		}
	}

	if err := ui.Step("Booting K3s server VM", func() error {
		if !running[cp] {
			if err := m.rt.Start(cp); err != nil {
				return err
			}
		}
		return m.rt.WaitReady(cp, waitTimeout)
	}); err != nil {
		return err
	}
	serverIP, err := m.rt.IP(cp)
	if err != nil {
		return err
	}
	if parsed := net.ParseIP(serverIP); parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("K3s server address %q is not IPv4", serverIP)
	}
	if err := ui.Step("Reconciling K3s server address", func() error {
		changed, err := m.updateK3sSystemdConfig(cp, "server", serverIP, serverIP)
		if err != nil {
			return err
		}
		unitChanged, err := m.ensureK3sGPUUnit(cp, "server")
		if err != nil {
			return err
		}
		if changed || unitChanged {
			nodesChanged.Store(true)
		}
		return m.ensureK3sSystemdService(cp, changed || unitChanged || !running[cp])
	}); err != nil {
		return err
	}
	if err := ui.Step("Waiting for the K3s API", func() error {
		return m.waitK3sAPI(cp, waitTimeout)
	}); err != nil {
		return err
	}

	if len(workers) > 0 {
		if err := ui.Step(fmt.Sprintf("Reconciling %d K3s agent(s)", len(workers)), func() error {
			return inParallel(len(workers), func(i int) error {
				node := workers[i]
				if !running[node] {
					if err := m.rt.Start(node); err != nil {
						return err
					}
				}
				if err := m.rt.WaitReady(node, waitTimeout); err != nil {
					return err
				}
				nodeIP, err := m.rt.IP(node)
				if err != nil {
					return err
				}
				changed, err := m.updateK3sSystemdConfig(node, "agent", nodeIP, serverIP)
				if err != nil {
					return err
				}
				unitChanged, err := m.ensureK3sGPUUnit(node, "agent")
				if err != nil {
					return err
				}
				if changed || unitChanged {
					nodesChanged.Store(true)
				}
				return m.ensureK3sSystemdService(node, changed || unitChanged || !running[node])
			})
		}); err != nil {
			return err
		}
	}

	if err := ui.Step("Waiting for current K3s node addresses", func() error {
		return m.waitK3sNodeAddresses(cp, nodes, waitTimeout)
	}); err != nil {
		return err
	}
	if nodesChanged.Load() {
		if err := ui.Step("Refreshing node-addressed pods", func() error {
			return m.refreshOptionalNodeExporter(cp, "k3s", waitTimeout)
		}); err != nil {
			return err
		}
	}
	if len(gpuWorkers) > 0 {
		if err := ui.Step("Reconciling GPU resource driver", func() error {
			return m.reconcileInstalledGPUResources(cp, "k3s", gpuWorkers, waitTimeout)
		}); err != nil {
			return err
		}
	}
	if err := ui.Step("Waiting for built-in addons", func() error {
		return m.waitSystemdManagedAddons(cp, "k3s", waitTimeout)
	}); err != nil {
		return err
	}

	if err := ui.Step("Healing K3s networking helpers", func() error {
		if err := m.healEdgeProxySystemd(cp, name, k3sKubeconfig, nodes); err != nil {
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
		raw, err := m.rt.Exec(cp, "cat", k3sKubeconfig)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(name, raw, serverIP)
		return err
	}); err != nil {
		return err
	}

	reachable := false
	_ = ui.Step("Checking API server reachability from your Mac", func() error {
		reachable = hostReachAPI(serverIP, 10*time.Second)
		if !reachable {
			return fmt.Errorf("host cannot reach %s:6443", serverIP)
		}
		return nil
	})

	ui.Successf("K3s cluster %q resumed in %s.", name, time.Since(started).Round(time.Second))
	ui.Infof("context kiac-%s updated in %s", name, kubeconfigPath)
	if reachable {
		ui.Hintf("kubectl get nodes")
	} else {
		warnAPIUnreachable(cp, name, serverIP)
	}
	return nil
}

func (m *Manager) updateK3sSystemdConfig(node, role, nodeIP, serverIP string) (bool, error) {
	raw, err := m.rt.Exec(node, "cat", "/etc/rancher/k3s/config.yaml")
	if err != nil {
		return false, err
	}
	config := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
		return false, fmt.Errorf("parsing K3s config on %s: %w", node, err)
	}
	changed := setK3sConfigString(config, "node-ip", nodeIP)
	switch role {
	case "server":
		changed = setK3sConfigString(config, "advertise-address", serverIP) || changed
		wantSANs := []string{node, serverIP}
		if !stringListEqual(config["tls-san"], wantSANs) {
			config["tls-san"] = wantSANs
			changed = true
		}
	case "agent":
		changed = setK3sConfigString(config, "server", "https://"+net.JoinHostPort(serverIP, "6443")) || changed
	default:
		return false, fmt.Errorf("invalid K3s role %q", role)
	}
	if !changed {
		return false, nil
	}
	updated, err := yaml.Marshal(config)
	if err != nil {
		return false, err
	}
	if err := m.rt.ExecStdin(node, strings.NewReader(string(updated)), "sh", "-euc", `
tmp="$(mktemp /etc/rancher/k3s/config.yaml.XXXXXX)"
cat > "$tmp"
chmod 0600 "$tmp"
mv "$tmp" /etc/rancher/k3s/config.yaml
`); err != nil {
		return false, err
	}
	return true, nil
}

func setK3sConfigString(config map[string]any, key, value string) bool {
	if current, ok := config[key].(string); ok && current == value {
		return false
	}
	config[key] = value
	return true
}

func stringListEqual(value any, want []string) bool {
	var got []string
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			text, ok := item.(string)
			if !ok {
				return false
			}
			got = append(got, text)
		}
	case []string:
		got = values
	default:
		return false
	}
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func (m *Manager) ensureK3sSystemdService(node string, restart bool) error {
	if restart {
		_, err := m.rt.Exec(node, "systemctl", "--no-block", "restart", "k3s.service")
		return err
	}
	if _, err := m.rt.Exec(node, "systemctl", "is-active", "--quiet", "k3s.service"); err == nil {
		return nil
	}
	_, err := m.rt.Exec(node, "systemctl", "--no-block", "start", "k3s.service")
	return err
}

// healK3sEdgeProxySystemd refreshes only the API credential file. The
// per-cluster tunnel token deliberately stays untouched so restarted nodes can
// still authenticate proxy-to-proxy traffic with their existing peers.
func (m *Manager) healEdgeProxySystemd(cp, name, sourceKubeconfig string, nodes []string) error {
	installed := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if _, err := m.rt.Exec(node, "sh", "-c", "test -x "+edgeProxyNodePath+" -a -f "+edgeProxyKubeconfigPath+" -a -f "+edgeProxyTokenPath); err == nil {
			installed = append(installed, node)
		}
	}
	if len(installed) == 0 {
		return nil
	}
	kubeconfig, err := m.edgeProxyKubeconfig(cp, name, sourceKubeconfig)
	if err != nil {
		return err
	}
	if err := inParallel(len(installed), func(i int) error {
		node := installed[i]
		if err := m.rt.ExecStdin(node, strings.NewReader(kubeconfig), "sh", "-euc", `
tmp="$(mktemp `+edgeProxyKubeconfigPath+`.XXXXXX)"
cat > "$tmp"
chmod 0600 "$tmp"
mv "$tmp" `+edgeProxyKubeconfigPath+`
`); err != nil {
			return err
		}
		_, err := m.rt.Exec(node, "systemctl", "restart", "kiac-edge-proxy.service")
		return err
	}); err != nil {
		return err
	}
	return m.waitEdgeProxyRules(installed)
}

type k3sNodeStateList struct {
	Items []k3sNodeState `json:"items"`
}

type k3sNodeState struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Addresses  []k3sNodeAddress   `json:"addresses"`
		Conditions []k3sNodeCondition `json:"conditions"`
	} `json:"status"`
}

type k3sNodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

type k3sNodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// waitK3sNodeAddresses is stricter than an ordinary Ready wait. A Node can
// remain Ready briefly after its VM restarts, while its InternalIP still
// names the previous vmnet address. Waiting for the current runtime address
// prevents hostNetwork pods and LoadBalancer status from capturing stale IPs.
func (m *Manager) waitK3sNodeAddresses(cp string, nodes []string, timeout time.Duration) error {
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
		out, err := m.k3sKubectl(cp, "get", "nodes", "-o", "json")
		if err == nil {
			var state k3sNodeStateList
			if err := json.Unmarshal([]byte(out), &state); err == nil {
				if ok, detail := k3sNodeAddressesCurrent(state, expected); ok {
					return nil
				} else {
					last = detail
				}
			} else {
				last = "cannot parse Kubernetes node state: " + err.Error()
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("k3s nodes did not register their current addresses in %s: %s", timeout, last)
}

func k3sNodeAddressesCurrent(state k3sNodeStateList, expected map[string]string) (bool, string) {
	observed := make(map[string]string, len(state.Items))
	ready := make(map[string]bool, len(state.Items))
	for _, node := range state.Items {
		for _, address := range node.Status.Addresses {
			parsed := net.ParseIP(address.Address)
			if address.Type == "InternalIP" && parsed != nil {
				observed[node.Metadata.Name] = address.Address
				if parsed.To4() != nil {
					break
				}
			}
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready[node.Metadata.Name] = true
			}
		}
	}
	for node, want := range expected {
		if !ready[node] {
			return false, fmt.Sprintf("node %s is not Ready", node)
		}
		if got := observed[node]; got != want {
			return false, fmt.Sprintf("node %s InternalIP is %q, waiting for %q", node, got, want)
		}
	}
	return true, ""
}

// refreshK3sNodePods recreates kiac's hostNetwork DaemonSet pods after vmnet
// changes node addresses. Kubelet updates the Node InternalIP, but an existing
// hostNetwork Pod object retains its old PodIP; recreating kindnet (and the
// optional node-exporter) keeps API-visible addresses honest.
func (m *Manager) refreshK3sNodePods(cp string, timeout time.Duration) error {
	timeoutArg := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	if _, err := m.k3sKubectl(cp, "-n", "kube-system", "rollout", "restart", "daemonset/kindnet"); err != nil {
		return err
	}
	if _, err := m.k3sKubectl(cp, "-n", "kube-system", "rollout", "status", "daemonset/kindnet", timeoutArg); err != nil {
		return err
	}
	return m.refreshOptionalNodeExporter(cp, "k3s", timeout)
}

func (m *Manager) refreshOptionalNodeExporter(cp, distro string, timeout time.Duration) error {
	out, err := m.gpuKubectl(cp, distro, "-n", obsNamespace, "get", "daemonset", "node-exporter", "--ignore-not-found", "-o", "name")
	if err != nil || strings.TrimSpace(out) == "" {
		return err
	}
	if _, err := m.gpuKubectl(cp, distro, "-n", obsNamespace, "rollout", "restart", "daemonset/node-exporter"); err != nil {
		return err
	}
	timeoutArg := fmt.Sprintf("--timeout=%ds", int(timeout.Seconds()))
	_, err = m.gpuKubectl(cp, distro, "-n", obsNamespace, "rollout", "status", "daemonset/node-exporter", timeoutArg)
	return err
}

func (m *Manager) installK3sResumeHooks(nodes []string, serverIP string) error {
	if parsed := net.ParseIP(serverIP); parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("k3s server address %q is not IPv4", serverIP)
	}
	targetURL := "https://" + serverIP + ":6443"
	return inParallel(len(nodes), func(i int) error {
		return m.installK3sResumeHook(nodes[i], targetURL)
	})
}

func (m *Manager) installK3sResumeHook(node, targetURL string) error {
	return m.rt.ExecStdin(node, strings.NewReader(k3sResumeLauncher),
		"sh", "-euc", installK3sResumeHookScript, "sh", targetURL)
}

func (m *Manager) waitK3sGuest(node string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := m.rt.Exec(node, "sh", "-c", `[ "$(readlink -f /proc/1/exe 2>/dev/null)" = /bin/k3s ]`); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("k3s node %s did not become reachable in %s: %w", node, timeout, lastErr)
}

func (m *Manager) k3sAgentLiveURL(node string) (string, error) {
	out, err := m.rt.Exec(node, "sh", "-c",
		`tr '\000' '\n' < /proc/1/environ | sed -n 's/^K3S_URL=//p' | head -n1`)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func normalizeServerURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func (m *Manager) healK3sEdgeProxy(nodes []string, targetURL string) error {
	var installed []string
	for _, node := range nodes {
		if _, err := m.rt.Exec(node, "sh", "-c", "test -x "+edgeProxyNodePath+" -a -f "+edgeProxyKubeconfigPath); err == nil {
			installed = append(installed, node)
		}
	}
	if len(installed) == 0 {
		return nil
	}
	if err := inParallel(len(installed), func(i int) error {
		node := installed[i]
		if _, err := m.rt.Exec(node, "sh", "-euc", healK3sEdgeProxyScript, "sh", targetURL); err != nil {
			return err
		}
		_, err := m.rt.Exec(node, "sh", "-euc", edgeProxySupervisorScript)
		return err
	}); err != nil {
		return err
	}
	if err := m.waitEdgeProxyRules(installed); err != nil {
		return err
	}
	return inParallel(len(installed), func(i int) error {
		_, err := m.rt.Exec(installed[i], "sh", "-euc", `printf '%s\n' "$1" > `+k3sEdgeProxyMarker+`; chmod 0600 `+k3sEdgeProxyMarker, "sh", targetURL)
		return err
	})
}

const healK3sEdgeProxyScript = `url="$1"
if [ "$(cat ` + k3sEdgeProxyMarker + ` 2>/dev/null || true)" = "$url" ]; then
  PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux:$PATH"
  IPT="$(command -v iptables-legacy || command -v iptables)"
  found=
  for p in /proc/[0-9]*; do
    [ "$(readlink "$p/exe" 2>/dev/null)" = "` + edgeProxyNodePath + `" ] && found=1 && break
  done
  if [ -n "$found" ] && "$IPT" -w -t nat -C PREROUTING -j KIAC-EDGE 2>/dev/null && "$IPT" -w -t nat -C OUTPUT -j KIAC-EDGE-OUTPUT 2>/dev/null; then
    exit 0
  fi
fi
tmp="$(mktemp ` + edgeProxyKubeconfigPath + `.XXXXXX)"
sed 's#^\([[:space:]]*server:[[:space:]]*\).*$#\1'"$url"'#' ` + edgeProxyKubeconfigPath + ` > "$tmp"
grep -Fq "server: $url" "$tmp"
chmod 0600 "$tmp"
mv "$tmp" ` + edgeProxyKubeconfigPath + `
if [ -r ` + edgeProxySupervisorPID + ` ]; then
  old="$(cat ` + edgeProxySupervisorPID + ` 2>/dev/null || true)"
  [ -z "$old" ] || kill "$old" 2>/dev/null || true
fi
rm -f ` + edgeProxySupervisorPID + `
for p in /proc/[0-9]*; do
  if [ "$(readlink "$p/exe" 2>/dev/null)" = "` + edgeProxyNodePath + `" ]; then
    kill "${p##*/}" 2>/dev/null || true
  fi
done
`
