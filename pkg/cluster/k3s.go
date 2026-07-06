package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

// k3sKubeconfig is where the k3s server writes its admin kubeconfig
// inside the VM; the counterpart of adminConf on the kubeadm path.
const k3sKubeconfig = "/etc/rancher/k3s/k3s.yaml"

// k3sServerArgs builds the k3s server command line for the given node.
// The rancher/k3s image entrypoint execs whatever follows the image, so
// these run k3s directly as PID 1 (no systemd in the VM).
func k3sServerArgs(cfg Config, nodeName string) []string {
	args := []string{
		"server",
		// flannel is disabled outright: even with host-gw (the node
		// kernel has no VXLAN), flannel bridges pods onto cni0, and
		// without br_netfilter same-node pod->ClusterIP return
		// traffic bypasses iptables un-DNAT and times out. kiac
		// applies kindnet (PTP, routed) right after the API answers.
		"--flannel-backend=none",
		// k3s's network-policy controller programs iptables against
		// the flannel bridge; without flannel it only logs errors.
		"--disable-network-policy",
		// The container name is also the VM hostname, so it is a
		// stable SAN; the apiserver cert already includes the node IP
		// that mergeKubeconfig points the host's kubeconfig at.
		"--tls-san", nodeName,
		// Explicit so the Kubernetes node name always matches the
		// container name, which get/status/chaos/delete key on.
		"--node-name", nodeName,
		// k3s's bundled Traefik ingress would fight kiac's --gateway
		// Traefik for ports 80/443. servicelb (klipper) stays on: it
		// backs type LoadBalancer with node IPs natively.
		"--disable=traefik",
	}
	// k3s bundles metrics-server, local-path storage, and servicelb, so
	// kiac's own addon installs are skipped entirely for this distro and
	// the --no-* flags map onto k3s --disable switches instead.
	if cfg.NoMetrics {
		args = append(args, "--disable=metrics-server")
	}
	if cfg.NoStorage {
		args = append(args, "--disable=local-storage")
	}
	if cfg.NoLB {
		args = append(args, "--disable=servicelb")
	}
	return args
}

// k3sAgentArgs builds the k3s agent command line for one worker.
func k3sAgentArgs(nodeName string) []string {
	return []string{"agent", "--node-name", nodeName}
}

// k3sBoot wraps a k3s command line in a /bin/sh preamble that relinks
// the image's iptables to the legacy xtables backend before exec'ing
// k3s as PID 1. The image's default symlinks point at the nf_tables
// build, and the node kernel has no CONFIG_NF_TABLES, so kube-proxy
// exits ("Could not fetch rule set generation id: Invalid argument")
// and, k3s being PID 1, takes the whole VM down with it. The legacy
// backend (CONFIG_IP_NF_IPTABLES_LEGACY=y) is fully supported. Both
// variants ship in the image under /bin/aux.
func k3sBoot(k3sArgs []string) (entrypoint string, args []string) {
	cmd := "for t in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do ln -sf xtables-legacy-multi /bin/aux/$t; done; exec k3s"
	for _, a := range k3sArgs {
		cmd += " " + shQuote(a)
	}
	return "/bin/sh", []string{"-c", cmd}
}

// shQuote single-quotes s for a POSIX shell command line.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// k3sAgentEnv points an agent at the server. K3S_URL/K3S_TOKEN are the
// documented join mechanism; the agent retries until the server answers.
func k3sAgentEnv(serverIP, token string) []string {
	return []string{
		"K3S_URL=https://" + serverIP + ":6443",
		"K3S_TOKEN=" + token,
	}
}

// ensureK3sCNIPlugins installs the upstream CNI plugin binaries kindnet
// needs into /opt/cni/bin of a node VM. kubelet resolves plugins there,
// and with the bundled flannel disabled k3s leaves the directory empty.
// k3s's own multicall binary is no substitute: it does not implement
// ptp, the plugin kindnet's conflist is built around (and bridge would
// reintroduce the br_netfilter breakage kindnet exists to avoid).
func (m *Manager) ensureK3sCNIPlugins(node string) error {
	archive, err := ensureCNIPluginsArchive()
	if err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return m.rt.ExecStdin(node, f, "/bin/sh", "-c",
		"mkdir -p /opt/cni/bin && tar -xz -C /opt/cni/bin ./loopback ./ptp ./host-local ./portmap ./bandwidth")
}

// k3sToken generates the shared cluster secret agents present to join.
func k3sToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating k3s token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CreateK3s boots a k3s cluster: the same VM-per-node layout and
// container names as Create, but each VM runs the rancher/k3s image as
// PID 1 (single binary, sqlite datastore, no systemd), so the kubeadm
// path's systemd-based readiness and addon installs do not apply here.
func (m *Manager) CreateK3s(cfg Config) error {
	start := time.Now()
	if !ValidName(cfg.Name) {
		return fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", cfg.Name)
	}
	cp := ControlPlane(cfg.Name)
	// Same hostname limit as Create: node name == container name == VM
	// hostname, and Linux caps hostnames at 63 chars.
	if len(cp) > maxNodeNameLen {
		return fmt.Errorf("cluster name %q is too long: node name %q is %d chars, over the %d-char limit; use a name of %d chars or fewer",
			cfg.Name, cp, len(cp), maxNodeNameLen, maxNodeNameLen-len("kiac--control-plane"))
	}
	if cfg.Gateway && cfg.NoLB {
		return fmt.Errorf("--gateway needs the built-in LoadBalancer; drop --no-lb")
	}

	existing, err := m.rt.List(prefix(cfg.Name))
	if err == nil && len(existing) > 0 {
		return fmt.Errorf("cluster %q already exists; delete it first with: kiac delete cluster --name %s", cfg.Name, cfg.Name)
	}

	if err := ui.Step("Preflight checks", func() error { return m.preflight() }); err != nil {
		return err
	}

	if err := ui.Step(fmt.Sprintf("Pulling k3s image %s", shortImage(cfg.Image)), func() error {
		return m.rt.ImagePull(cfg.Image)
	}); err != nil {
		return err
	}

	token, err := k3sToken()
	if err != nil {
		return err
	}

	if err := ui.Step("Booting k3s server VM", func() error {
		entry, bootArgs := k3sBoot(k3sServerArgs(cfg, cp))
		if err := m.rt.RunDetached(runtime.RunOpts{
			Name:       cp,
			Image:      cfg.Image,
			CPUs:       cfg.CPUs,
			Memory:     cfg.CPMemory,
			Env:        []string{"K3S_TOKEN=" + token},
			Entrypoint: entry,
			Args:       bootArgs,
		}); err != nil {
			return err
		}
		// WaitReady polls systemd/containerd and cannot work here (k3s
		// is PID 1); the apiserver answering IS readiness for k3s.
		return m.waitK3sAPI(cp, cfg.WaitTimeout)
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if err := ui.Step("Installing CNI (kindnet)", func() error {
		if err := m.ensureK3sCNIPlugins(cp); err != nil {
			return err
		}
		return m.k3sKubectlStdin(cp, strings.NewReader(k3sKindnetManifest),
			"apply", "-f", "-")
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	serverIP, err := m.rt.IP(cp)
	if err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if cfg.Workers > 0 {
		if err := ui.Step(fmt.Sprintf("Joining %d k3s agent(s)", cfg.Workers), func() error {
			env := k3sAgentEnv(serverIP, token)
			for i := 1; i <= cfg.Workers; i++ {
				w := worker(cfg.Name, i)
				entry, bootArgs := k3sBoot(k3sAgentArgs(w))
				if err := m.rt.RunDetached(runtime.RunOpts{
					Name:       w,
					Image:      cfg.Image,
					CPUs:       cfg.CPUs,
					Memory:     cfg.Memory,
					Env:        env,
					Entrypoint: entry,
					Args:       bootArgs,
				}); err != nil {
					return err
				}
			}
			// Pod sandboxes on the agents need the standard CNI plugin
			// names resolvable too; the helper's poll rides out each
			// agent's k3s self-extraction.
			return inParallel(cfg.Workers, func(i int) error {
				return m.ensureK3sCNIPlugins(worker(cfg.Name, i+1))
			})
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := ui.Step("Waiting for nodes to be Ready", func() error {
		return m.waitK3sNodes(cp, 1+cfg.Workers, cfg.WaitTimeout)
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	// Same primary rule as configureLBPool on the kubeadm path: the
	// first worker (or the server on single-node clusters) is labeled so
	// bundled manifests like Grafana and Traefik, which pin their pods
	// with a kiac.io/lb-primary nodeSelector, remain schedulable. The
	// metallb.io/address-pool annotation those manifests carry is simply
	// ignored by servicelb.
	if !cfg.NoLB {
		primary := cp
		if cfg.Workers > 0 {
			primary = worker(cfg.Name, 1)
		}
		if err := ui.Step("Labeling primary LoadBalancer node", func() error {
			_, err := m.k3sKubectl(cp, "label", "node", primary, "kiac.io/lb-primary=true", "--overwrite")
			return err
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	// Optional addons apply through k3s kubectl; failures degrade the
	// cluster rather than tearing it down, matching the kubeadm path.
	if cfg.Observability {
		if err := m.installObservabilityK3s(cp); err != nil {
			ui.Infof("observability stack not installed: %v", err)
		}
	}
	if cfg.Gateway {
		if err := m.installGatewayK3s(cp); err != nil {
			ui.Infof("gateway stack not installed: %v", err)
		}
	}

	var kubeconfigPath string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", k3sKubeconfig)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(cfg.Name, raw, serverIP)
		return err
	}); err != nil {
		return err
	}

	ui.Successf("k3s cluster %q is ready in %s. Every node is its own lightweight VM.",
		cfg.Name, time.Since(start).Round(time.Second))
	ui.Infof("context kiac-%s merged into %s", cfg.Name, kubeconfigPath)
	ui.Infof("k3s bundles local-path storage, servicelb, and metrics-server; kiac's MetalLB is not used")
	ui.Hintf("kubectl get nodes")
	if !cfg.NoMetrics {
		ui.Hintf("kubectl top nodes        # native metrics, give it ~60s to scrape")
	}
	return nil
}

// k3sKubectl runs kubectl inside the server VM via the /bin/kubectl
// multicall symlink (running as root it reads /etc/rancher/k3s/k3s.yaml
// automatically). `k3s kubectl` is NOT used: under `container exec`,
// vminitd's argv handling makes the k3s multicall binary resolve the
// subcommand as argv0 and it fails with `unknown command "kubectl"`.
func (m *Manager) k3sKubectl(cp string, args ...string) (string, error) {
	return m.rt.Exec(cp, append([]string{"kubectl"}, args...)...)
}

// k3sKubectlStdin is k3sKubectl with a manifest piped to stdin.
func (m *Manager) k3sKubectlStdin(cp string, r io.Reader, args ...string) error {
	return m.rt.ExecStdin(cp, r, append([]string{"kubectl"}, args...)...)
}

// waitK3sAPI polls until the apiserver inside the server VM answers a
// kubectl call, i.e. k3s finished bootstrapping its datastore and certs.
func (m *Manager) waitK3sAPI(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := m.k3sKubectl(name, "get", "nodes", "--no-headers")
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("k3s apiserver on %s did not answer in %s: %w", name, timeout, lastErr)
}

// waitK3sNodes polls `k3s kubectl get nodes` on the server until want
// nodes are registered and every one reports Ready.
func (m *Manager) waitK3sNodes(cp string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := m.k3sKubectl(cp, "get", "nodes", "--no-headers")
		if err == nil {
			last = out
			if k3sNodesReady(out, want) {
				return nil
			}
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("nodes not Ready in %s; last state:\n%s", timeout, strings.TrimSpace(last))
}

// k3sNodesReady parses `kubectl get nodes --no-headers` output and
// reports whether at least want nodes exist and every listed node has
// Ready among its status conditions (NotReady and SchedulingDisabled
// variants are handled by exact comma-part matching).
func k3sNodesReady(out string, want int) bool {
	nodes := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		nodes++
		ready := false
		for _, part := range strings.Split(fields[1], ",") {
			if part == "Ready" {
				ready = true
			}
		}
		if !ready {
			return false
		}
	}
	return nodes >= want
}

// installObservabilityK3s mirrors installObservability for the k3s
// distro: same embedded manifests, applied through k3s kubectl, with
// servicelb (not MetalLB) assigning Grafana its LoadBalancer IP.
func (m *Manager) installObservabilityK3s(cp string) error {
	if err := ui.Step("Installing observability (Prometheus + Grafana)", func() error {
		manifest := strings.Join(observabilityManifests(), "\n---\n")
		return m.k3sKubectlStdin(cp, strings.NewReader(manifest), "apply", "-f", "-")
	}); err != nil {
		return err
	}

	var grafanaIP string
	if err := ui.Step("Waiting for Grafana LoadBalancer IP", func() error {
		var err error
		// servicelb first pulls its klipper-lb pod image inside the VM,
		// so the window is wider than the kubeadm path's 90s.
		grafanaIP, err = m.waitK3sSvcLBIP(cp, obsNamespace, "grafana", 120*time.Second)
		return err
	}); err != nil {
		return err
	}

	ui.Infof("Grafana: http://%s:3000 (anonymous admin, local-only)", grafanaIP)
	ui.Hintf("open http://%s:3000        # dashboards: Cluster Overview, Nodes", grafanaIP)
	return nil
}

// installGatewayK3s mirrors installGateway for the k3s distro. The k3s
// bundled Traefik ingress is disabled at server start (see
// k3sServerArgs), so kiac's Traefik owns ports 80/443 on the LB.
func (m *Manager) installGatewayK3s(cp string) error {
	if err := ui.Step("Installing Gateway API CRDs", func() error {
		// Server-side apply for the same reason as the kubeadm path:
		// the HTTPRoute CRD exceeds the client-side annotation cap.
		if err := m.k3sKubectlStdin(cp, strings.NewReader(gatewayCRDsManifest),
			"apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
			return err
		}
		_, err := m.k3sKubectl(cp, "wait", "--for", "condition=Established",
			"crd/httproutes.gateway.networking.k8s.io", "--timeout=60s")
		return err
	}); err != nil {
		return err
	}

	if err := ui.Step("Installing Traefik (GatewayClass traefik)", func() error {
		if err := m.k3sKubectlStdin(cp, strings.NewReader(gatewayTraefikManifest), "apply", "-f", "-"); err != nil {
			return err
		}
		return m.k3sKubectlStdin(cp, strings.NewReader(gatewayDefaultManifest), "apply", "-f", "-")
	}); err != nil {
		return err
	}

	var addr string
	if err := ui.Step("Waiting for Gateway address", func() error {
		var err error
		addr, err = m.waitK3sSvcLBIP(cp, "kiac-gateway", "traefik", 120*time.Second)
		return err
	}); err != nil {
		return err
	}

	ui.Infof("Gateway API ready: http://%s (GatewayClass traefik, Gateway kiac-gateway/kiac)", addr)
	ui.Hintf(`attach routes with: spec.parentRefs: [{name: kiac, namespace: kiac-gateway}] in any HTTPRoute`)
	return nil
}

// waitK3sSvcLBIP polls a Service until servicelb publishes a
// LoadBalancer ingress IP for it. servicelb (klipper) advertises EVERY
// node's IP; the addon pods are pinned to the kiac.io/lb-primary node,
// and only that node's IP delivers pod-locally (a frame entering
// another node is NATed across vmnet's slow forwarding path, ~100x
// worse for bulk transfers), so the primary's IP is preferred over
// ingress[0] when it is among the advertised set.
func (m *Manager) waitK3sSvcLBIP(cp, namespace, svc string, timeout time.Duration) (string, error) {
	primaryOut, _ := m.k3sKubectl(cp, "get", "nodes",
		"-l", "kiac.io/lb-primary=true",
		"-o", "jsonpath={.items[0].status.addresses[?(@.type==\"InternalIP\")].address}")
	// Dual-stack nodes list an IPv6 InternalIP too; the vmnet addresses
	// kiac hands out are IPv4, so match on the first IPv4-looking entry.
	primaryIP := ""
	for _, a := range strings.Fields(primaryOut) {
		if strings.Count(a, ".") == 3 {
			primaryIP = a
			break
		}
	}
	deadline := time.Now().Add(timeout)
	first := ""
	for {
		out, err := m.k3sKubectl(cp, "get", "svc", "-n", namespace, svc,
			"-o", "jsonpath={range .status.loadBalancer.ingress[*]}{.ip}{\" \"}{end}")
		if err == nil {
			for _, ip := range strings.Fields(out) {
				if primaryIP != "" && ip == primaryIP {
					return ip, nil
				}
				if first == "" {
					first = ip
				}
			}
		}
		if time.Now().After(deadline) {
			// servicelb publishes node IPs one at a time as its
			// per-node pods go ready; only fall back to whichever
			// came first once the primary has had its full window.
			if first != "" {
				return first, nil
			}
			return "", fmt.Errorf("service %s/%s never got a LoadBalancer IP (servicelb disabled?); check: kubectl get svc -n %s %s", namespace, svc, namespace, svc)
		}
		time.Sleep(3 * time.Second)
	}
}
