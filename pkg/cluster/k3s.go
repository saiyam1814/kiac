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
		// Traefik for ports 80/443. Its bundled servicelb advertises
		// every node IP for each LoadBalancer, including nodes that must
		// forward large uploads across vmnet; kiac-lb is lighter and
		// assigns an endpoint-local node IP instead.
		"--disable=traefik",
		"--disable=servicelb",
	}
	// Dual-stack: give k3s both pod and Service CIDRs (v4 primary). The
	// node --node-ip is set at boot in k3sBoot, once the VM knows its own
	// addresses. Only "dual" reaches here; ipv6-only k3s is rejected in
	// CreateK3s because it needs pre-boot cert SAN handling the kubeadm
	// path covers instead.
	if cfg.family() == DualStack {
		args = append(args,
			"--cluster-cidr="+cfg.family().podCIDR(k3sPodCIDRv4, k3sPodCIDRv6),
			"--service-cidr="+cfg.family().serviceCIDR(k3sServiceCIDRv4, k3sServiceCIDRv6),
		)
	}
	// k3s bundles metrics-server and local-path storage, so kiac's own
	// addon installs are skipped for those parts and the --no-* flags map
	// onto k3s --disable switches instead. LoadBalancer support uses the
	// same tiny kiac-lb controller as the kubeadm path unless --no-lb is set.
	if cfg.NoMetrics {
		args = append(args, "--disable=metrics-server")
	}
	if cfg.NoStorage {
		args = append(args, "--disable=local-storage")
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
func k3sBoot(cfg Config, k3sArgs []string) (entrypoint string, args []string) {
	cmd := senderOffloadFix + "; " +
		"for t in iptables iptables-save iptables-restore ip6tables ip6tables-save ip6tables-restore; do ln -sf xtables-legacy-multi /bin/aux/$t; done; " +
		"if [ -x " + kiacLBScriptPath + " ]; then " +
		"mkdir -p /var/log /var/run; " +
		"if ! kill -0 \"$(cat " + kiacLBK3sSupervisorPID + " 2>/dev/null)\" 2>/dev/null; then " +
		"nohup sh -c 'while :; do KUBECONFIG=" + k3sKubeconfig + " " + kiacLBScriptPath + " >>" + kiacLBK3sLogPath + " 2>&1; sleep 1; done' >/dev/null 2>&1 & echo $! > " + kiacLBK3sSupervisorPID + "; " +
		"fi; fi; " +
		"if [ -x " + edgeProxyNodePath + " ] && [ -f " + edgeProxyKubeconfigPath + " ] && [ -f " + edgeProxyTokenPath + " ]; then " +
		"if ! kill -0 \"$(cat " + edgeProxySupervisorPID + " 2>/dev/null)\" 2>/dev/null; then " +
		"nohup sh -c 'while :; do " + edgeProxyNodePath + " --kubeconfig " + edgeProxyKubeconfigPath + " --token-file " + edgeProxyTokenPath + " >>" + edgeProxyLogPath + " 2>&1; sleep 1; done' >/dev/null 2>&1 & echo $! > " + edgeProxySupervisorPID + "; " +
		"fi; fi; "
	// Dual-stack: k3s needs --node-ip with both families, but the VM only
	// learns its addresses after it boots, so compute them here (mirroring
	// runtime.IP/IPv6's default-route source lookup) and append the flag
	// to the k3s exec. sysctl enables v6 forwarding, matching the kubeadm
	// path's post-boot step; k3s is PID 1 so there is no separate hook.
	nodeIPExpr := ""
	if cfg.family() == DualStack {
		// k3s runs as PID 1, so this preamble executes before SLAAC has
		// assigned the vmnet global IPv6 (it waits on a router
		// advertisement). Two things matter here:
		//
		//  1. accept_ra=2 on the primary interface BEFORE enabling
		//     forwarding. Enabling net.ipv6 forwarding turns off the
		//     default accept_ra=1, so the kernel would ignore the router
		//     advertisement and never get a v6 address; =2 keeps accepting
		//     RAs even with forwarding on.
		//  2. Poll up to ~30s for the address, so --node-ip never gets a
		//     trailing-comma half value. The scope-global scan is the same
		//     fallback runtime.IPv6 uses when the default route lags.
		cmd += ipv6BootPrep +
			`KIAC_V4="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<NF;i++) if ($i=="src") print $(i+1)}' | head -n1)"; ` +
			`KIAC_V6=""; KIAC_I=0; while [ "$KIAC_I" -lt 45 ]; do ` +
			// scope-global scan first: it lists only real global addresses,
			// never ::1 (which route-get returns before the v6 default
			// route exists) or fe80:: link-local. The grep -v is a
			// belt-and-braces filter for either sneaking through.
			`KIAC_V6="$(ip -6 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -v '^fe80' | grep -v '^::1$' | head -n1)"; ` +
			`[ -n "$KIAC_V6" ] && break; sleep 1; KIAC_I=$((KIAC_I+1)); done; `
		nodeIPExpr = ` --node-ip="$KIAC_V4,$KIAC_V6"`
	}
	cmd += "exec k3s"
	for _, a := range k3sArgs {
		cmd += " " + shQuote(a)
	}
	cmd += nodeIPExpr
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

// k3sServerRunOpts carries the common VM knobs into the server VM. Keep
// this in one place so flags like --kernel cannot silently apply to the
// kubeadm path only.
func k3sServerRunOpts(cfg Config, nodeName, token string, dns []string) runtime.RunOpts {
	entry, bootArgs := k3sBoot(cfg, k3sServerArgs(cfg, nodeName))
	return runtime.RunOpts{
		Name:       nodeName,
		Image:      cfg.Image,
		CPUs:       cfg.CPUs,
		Memory:     cfg.CPMemory,
		Env:        []string{"K3S_TOKEN=" + token},
		Entrypoint: entry,
		Kernel:     cfg.Kernel,
		Args:       bootArgs,
		DNS:        dns,
	}
}

// k3sAgentRunOpts mirrors k3sServerRunOpts for workers.
func k3sAgentRunOpts(cfg Config, nodeName string, env []string, dns []string) runtime.RunOpts {
	entry, bootArgs := k3sBoot(cfg, k3sAgentArgs(nodeName))
	return runtime.RunOpts{
		Name:       nodeName,
		Image:      cfg.Image,
		CPUs:       cfg.CPUs,
		Memory:     cfg.Memory,
		Env:        env,
		Entrypoint: entry,
		Kernel:     cfg.Kernel,
		Args:       bootArgs,
		DNS:        dns,
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
	nodes := []string{cp}
	for i := 1; i <= cfg.Workers; i++ {
		nodes = append(nodes, worker(cfg.Name, i))
	}
	// Same hostname limit as Create: node name == container name == VM
	// hostname, and Linux caps hostnames at 63 chars.
	if len(cp) > maxNodeNameLen {
		return fmt.Errorf("cluster name %q is too long: node name %q is %d chars, over the %d-char limit; use a name of %d chars or fewer",
			cfg.Name, cp, len(cp), maxNodeNameLen, maxNodeNameLen-len("kiac--control-plane"))
	}
	if cfg.Gateway && cfg.NoLB {
		return fmt.Errorf("--gateway needs the built-in LoadBalancer; drop --no-lb")
	}
	if err := validateIPFamily(cfg); err != nil {
		return err
	}
	if err := validateDNS(cfg); err != nil {
		return err
	}
	if cfg.family() == IPv6 {
		return fmt.Errorf("--ip-family ipv6 is not supported on --distro k3s yet (it needs pre-boot apiserver cert SANs the kubeadm path handles); use --distro kubeadm for IPv6-only, or --ip-family dual on k3s")
	}

	existing, err := m.rt.List(prefix(cfg.Name))
	if err == nil && len(existing) > 0 {
		return fmt.Errorf("cluster %q already exists; delete it first with: kiac delete cluster --name %s", cfg.Name, cfg.Name)
	}

	if err := ui.Step("Preflight checks", func() error {
		if err := m.preflight(); err != nil {
			return err
		}
		return m.preflightIPv6Network(cfg)
	}); err != nil {
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

	dns := nodeDNS(cfg)

	if err := ui.Step("Booting k3s server VM", func() error {
		if err := m.rt.RunDetached(k3sServerRunOpts(cfg, cp, token, dns)); err != nil {
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
		return m.k3sKubectlStdin(cp, strings.NewReader(k3sKindnetManifestFor(cfg.family())),
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
			workers := make([]string, 0, cfg.Workers)
			for i := 1; i <= cfg.Workers; i++ {
				w := worker(cfg.Name, i)
				workers = append(workers, w)
				if err := m.rt.RunDetached(k3sAgentRunOpts(cfg, w, env, dns)); err != nil {
					return err
				}
			}
			// Pod sandboxes on the agents need the standard CNI plugin
			// names resolvable too; the helper's poll rides out each
			// agent's k3s self-extraction.
			if err := inParallel(cfg.Workers, func(i int) error {
				return m.ensureK3sCNIPlugins(worker(cfg.Name, i+1))
			}); err != nil {
				return err
			}
			// The persisted VM command keeps the original K3S_URL in its
			// environment. Install a transparent launcher now so resume can
			// replace only that endpoint after vmnet assigns the server a new
			// address, without rebuilding the worker VM.
			return m.installK3sResumeHooks(workers, serverIP)
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
	// with a kiac.io/lb-primary nodeSelector, remain schedulable. kiac-lb
	// also prefers a node hosting the Service endpoint before falling back
	// to this primary label.
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
		if err := m.installKiacLBK3s(cp); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if !cfg.NoEdgeProxy {
		if err := m.installEdgeProxyK3s(cp, cfg, nodes); err != nil {
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

	// See Create: vmnet can leave the control-plane VM unreachable from
	// the Mac even though the API is healthy inside it. Probe before
	// suggesting kubectl.
	reachable := false
	_ = ui.Step("Checking API server reachability from your Mac", func() error {
		reachable = hostReachAPI(serverIP, 10*time.Second)
		if !reachable {
			return fmt.Errorf("host cannot reach %s:6443", serverIP)
		}
		return nil
	})

	ui.Successf("k3s cluster %q is ready in %s. Every node is its own lightweight VM.",
		cfg.Name, time.Since(start).Round(time.Second))
	ui.Infof("context kiac-%s merged into %s", cfg.Name, kubeconfigPath)
	ui.Infof("k3s bundles local-path storage and metrics-server; kiac-lb provides LoadBalancer IPs")
	if reachable {
		ui.Hintf("kubectl get nodes")
		if !cfg.NoMetrics {
			ui.Hintf("kubectl top nodes        # native metrics, give it ~60s to scrape")
		}
	} else {
		warnAPIUnreachable(cp, cfg.Name, serverIP)
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
// kiac-lb assigning Grafana its LoadBalancer IP.
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
		grafanaIP, err = m.waitK3sLoadBalancerIP(cp, obsNamespace, "grafana", 90*time.Second)
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
		addr, err = m.waitK3sLoadBalancerIP(cp, "kiac-gateway", "traefik", 90*time.Second)
		return err
	}); err != nil {
		return err
	}

	ui.Infof("Gateway API ready: http://%s (GatewayClass traefik, Gateway kiac-gateway/kiac)", addr)
	ui.Hintf(`attach routes with: spec.parentRefs: [{name: kiac, namespace: kiac-gateway}] in any HTTPRoute`)
	return nil
}

// waitK3sLoadBalancerIP polls a Service until kiac-lb publishes a
// LoadBalancer ingress IP for it. The primary preference is kept for
// older upgraded clusters that may still have multiple servicelb IPs in
// status, but new k3s clusters use kiac-lb and normally publish one IP.
func (m *Manager) waitK3sLoadBalancerIP(cp, namespace, svc string, timeout time.Duration) (string, error) {
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
			if first != "" {
				return first, nil
			}
			return "", fmt.Errorf("service %s/%s never got a LoadBalancer IP (is kiac-lb running?); check: kubectl get svc -n %s %s", namespace, svc, namespace, svc)
		}
		time.Sleep(3 * time.Second)
	}
}
