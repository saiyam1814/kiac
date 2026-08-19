// Package cluster turns apple/container lightweight VMs into a Kubernetes
// cluster: one VM per node, kubeadm inside, kindnet CNI, metrics-server on
// by default.
package cluster

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

const adminConf = "/etc/kubernetes/admin.conf"

// IPFamily selects the address families a cluster's pods, Services, and
// nodes use. Anything but IPv4 needs the kiac full kernel: the runtime's
// default kernel ships no IPv6 netfilter, so kube-proxy silently programs
// only IPv4 rules and IPv6 Services never connect (issue #10).
type IPFamily string

const (
	// IPv4 is today's behavior: single-stack IPv4, stock kernel.
	IPv4 IPFamily = "ipv4"
	// DualStack runs IPv4-primary with IPv6 secondary: pods and nodes get
	// both, Services default to IPv4 and opt into IPv6 via ipFamilies.
	DualStack IPFamily = "dual"
	// IPv6 runs IPv6-primary at the Kubernetes layer (pod/Service/node
	// InternalIP all IPv6). Nodes keep their vmnet IPv4 for image pulls
	// and other host egress; this is not a link-level IPv6-only node.
	IPv6 IPFamily = "ipv6"
)

// Dual-stack and IPv6 CIDRs, kept in one place so kubeadm init, the k3s
// server args, and the kindnet POD_SUBNET template always agree. The
// IPv6 ranges follow kind's own dual-stack defaults (ULA space, no
// relation to any real routable prefix). kubeadm and k3s use different
// IPv4 ranges, so each distro has its own pair.
const (
	kubeadmPodCIDRv4     = "10.244.0.0/16"
	kubeadmPodCIDRv6     = "fd00:10:244::/56"
	kubeadmServiceCIDRv4 = "10.96.0.0/12"
	kubeadmServiceCIDRv6 = "fd00:10:96::/112"

	k3sPodCIDRv4     = "10.42.0.0/16"
	k3sPodCIDRv6     = "fd00:10:42::/56"
	k3sServiceCIDRv4 = "10.43.0.0/16"
	k3sServiceCIDRv6 = "fd00:10:43::/112"
)

// podCIDR returns the pod network CIDR string for the family, ordered
// primary-first: IPv4 for dual (v4-primary), IPv6 for ipv6-only.
func (f IPFamily) podCIDR(v4, v6 string) string {
	switch f {
	case DualStack:
		return v4 + "," + v6
	case IPv6:
		return v6
	default:
		return v4
	}
}

// serviceCIDR mirrors podCIDR for the Service network.
func (f IPFamily) serviceCIDR(v4, v6 string) string { return f.podCIDR(v4, v6) }

// ipv6BootPrep readies a node's networking for IPv6 before anything reads
// its address. accept_ra=2 on the primary interface must be set BEFORE
// forwarding: enabling IPv6 forwarding turns off the default accept_ra=1,
// so without =2 the kernel ignores the vmnet router advertisement and the
// node never acquires (or later loses, at RA-lifetime expiry) its global
// v6 address. Safe to run on any node; a no-op where already set.
const ipv6BootPrep = `KIAC_IF="$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}')"; ` +
	`[ -n "$KIAC_IF" ] && sysctl -w "net.ipv6.conf.$KIAC_IF.accept_ra=2" >/dev/null 2>&1; ` +
	`sysctl -w net.ipv6.conf.all.forwarding=1 >/dev/null 2>&1 || true; `

// vmnet mishandles TSO/GSO-deferred TCP streams between sibling VMs once
// the receiving node forwards them across a veth into a pod - the same
// pathology as issue #8, but VM-to-VM, where the edge proxy cannot sit.
// A pod pulling a large body from the API server (kubectl's ~6.5MB
// OpenAPI download inside a job, for example) crawls at KB/s and times
// out. Disabling segmentation offload on the sending side keeps those
// transfers at line rate. Offloads are runtime state, so this runs on
// every boot and resume. Best-effort: a node without ethtool still
// works, just slower.
const senderOffloadFix = "command -v ethtool >/dev/null 2>&1 && ethtool -K eth0 tso off gso off || true"

// WantsIPv6 reports whether the family carries IPv6 traffic at all.
func (f IPFamily) WantsIPv6() bool { return f == DualStack || f == IPv6 }

// Valid reports whether f is a supported family value.
func (f IPFamily) Valid() bool {
	switch f {
	case IPv4, DualStack, IPv6:
		return true
	default:
		return false
	}
}

// Config describes a cluster to create.
type Config struct {
	Name          string
	Workers       int
	Image         string
	CPUs          string
	Memory        string // worker VMs; measured idle usage is ~400Mi, so 2G default
	CPMemory      string // control plane; etcd+apiserver (and all addons on single-node) need headroom
	CNI           string
	Kernel        string   // resolved kernel Image path; empty = runtime default
	IPFamily      IPFamily // ipv4 (default), dual, or ipv6; non-ipv4 requires the full kernel
	DNS           []string // node VM nameservers; empty = runtime default resolv.conf (see nodeDNS)
	NoMetrics     bool
	NoStorage     bool
	NoLB          bool
	NoEdgeProxy   bool
	Observability bool
	Gateway       bool
	WaitTimeout   time.Duration
}

// family returns the configured IP family, defaulting an empty value to
// IPv4 so a zero Config behaves exactly as before this feature existed.
func (c Config) family() IPFamily {
	if c.IPFamily == "" {
		return IPv4
	}
	return c.IPFamily
}

// Manager orchestrates cluster lifecycle on top of the container runtime.
type Manager struct {
	rt *runtime.Client
}

func NewManager() *Manager { return &Manager{rt: runtime.New()} }

func (m *Manager) Runtime() *runtime.Client { return m.rt }

// ValidName reports whether a cluster name is safe for container names,
// kubeconfig entries, and the UI: lowercase letters, digits, dashes.
func ValidName(s string) bool {
	for _, r := range s {
		if !(r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return s != ""
}

// maxNodeNameLen is the Linux hostname limit; a node's container name is
// also its hostname and its kubeadm node name, so all three stay in sync
// only while the name fits here.
const maxNodeNameLen = 63

func prefix(name string) string { return "kiac-" + name + "-" }

// ControlPlane returns the control-plane container name for a cluster.
func ControlPlane(name string) string { return prefix(name) + "control-plane" }

func worker(name string, i int) string { return fmt.Sprintf("%sworker-%d", prefix(name), i) }

// Create boots the node VMs and brings up Kubernetes. Progress is
// reported through ui.Step so the caller stays a thin cobra shim.
func (m *Manager) Create(cfg Config) error {
	start := time.Now()
	if !ValidName(cfg.Name) {
		return fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", cfg.Name)
	}
	cp := ControlPlane(cfg.Name)
	// A node's name is also its VM hostname, capped at 63 chars by Linux.
	// Past that the VM fails to boot (or the hostname is truncated and the
	// kubeadm node name no longer matches the VM), so reject it up front.
	if len(cp) > maxNodeNameLen {
		return fmt.Errorf("cluster name %q is too long: node name %q is %d chars, over the %d-char limit; use a name of %d chars or fewer",
			cfg.Name, cp, len(cp), maxNodeNameLen, maxNodeNameLen-len("kiac--control-plane"))
	}

	existing, err := m.rt.List(prefix(cfg.Name))
	if err == nil && len(existing) > 0 {
		return fmt.Errorf("cluster %q already exists; delete it first with: kiac delete cluster --name %s", cfg.Name, cfg.Name)
	}

	if err := validateIPFamily(cfg); err != nil {
		return err
	}
	if err := validateDNS(cfg); err != nil {
		return err
	}

	// Fail cilium prerequisites before any VM boots, not minutes later
	// when the CNI step runs.
	if cfg.CNI == "cilium" {
		if cfg.Kernel == "" {
			return fmt.Errorf("--cni cilium needs the full node kernel: add --kernel full (or a --kernel path)")
		}
		if _, err := exec.LookPath("cilium"); err != nil {
			return fmt.Errorf("--cni cilium drives the official installer, which is not on PATH; install it with: brew install cilium-cli")
		}
	}

	if err := ui.Step("Preflight checks", func() error {
		if err := m.preflight(); err != nil {
			return err
		}
		return m.preflightIPv6Network(cfg)
	}); err != nil {
		return err
	}

	if err := ui.Step(fmt.Sprintf("Pulling node image %s", shortImage(cfg.Image)), func() error {
		return m.rt.ImagePull(cfg.Image)
	}); err != nil {
		return err
	}

	nodes := []string{cp}
	for i := 1; i <= cfg.Workers; i++ {
		nodes = append(nodes, worker(cfg.Name, i))
	}

	dns := nodeDNS(cfg)

	if err := ui.Step(fmt.Sprintf("Booting %d node VM(s)", len(nodes)), func() error {
		for _, n := range nodes {
			mem := cfg.Memory
			if n == cp && cfg.CPMemory != "" {
				mem = cfg.CPMemory
			}
			if err := m.rt.RunDetached(runtime.RunOpts{Name: n, Image: cfg.Image, CPUs: cfg.CPUs, Memory: mem, Kernel: cfg.Kernel, DNS: dns}); err != nil {
				return err
			}
		}
		// Each node boots independently, so readiness polling, the sysctl,
		// and the dual-stack kubelet pin run concurrently across nodes.
		return inParallel(len(nodes), func(i int) error {
			n := nodes[i]
			if err := m.rt.WaitReady(n, cfg.WaitTimeout); err != nil {
				return err
			}
			if _, err := m.rt.Exec(n, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
				return err
			}
			if _, err := m.rt.Exec(n, "sh", "-c", senderOffloadFix); err != nil {
				return err
			}
			if cfg.family().WantsIPv6() {
				if _, err := m.rt.Exec(n, "sh", "-c", ipv6BootPrep); err != nil {
					return err
				}
				return m.pinKubeletNodeIP(n, cfg.family())
			}
			return nil
		})
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if err := ui.Step("Initializing Kubernetes control plane", func() error {
		args := []string{"init",
			"--pod-network-cidr=" + cfg.family().podCIDR(kubeadmPodCIDRv4, kubeadmPodCIDRv6),
			"--node-name", cp,
			"--ignore-preflight-errors=all"}
		if cfg.family().WantsIPv6() {
			args = append(args, "--service-cidr="+cfg.family().serviceCIDR(kubeadmServiceCIDRv4, kubeadmServiceCIDRv6))
		}
		if cfg.family() == IPv6 {
			// IPv6-only: kubeadm's default advertise-address detection
			// picks the v4 default-route source, which conflicts with a
			// v6-only service CIDR. Pin it to the node's v6 so the
			// apiserver serves and certs itself on the family the cluster
			// actually uses; the host then connects over v6 too.
			v6, err := m.waitNodeIPv6(cp, 45*time.Second)
			if err != nil {
				return err
			}
			args = append(args, "--apiserver-advertise-address="+v6)
		}
		_, err := m.rt.Exec(cp, append([]string{"kubeadm"}, args...)...)
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if cfg.Workers > 0 {
		if err := ui.Step(fmt.Sprintf("Joining %d worker(s)", cfg.Workers), func() error {
			joinCmd, err := m.rt.Exec(cp, "kubeadm", "token", "create", "--print-join-command")
			if err != nil {
				return err
			}
			join := strings.Fields(lastNonEmptyLine(joinCmd))
			if len(join) == 0 || join[0] != "kubeadm" {
				return fmt.Errorf("unexpected join command: %q", joinCmd)
			}
			join = append(join, "--ignore-preflight-errors=all")
			// Joins from distinct nodes against one control plane are
			// safe to run concurrently.
			return inParallel(cfg.Workers, func(i int) error {
				w := worker(cfg.Name, i+1)
				// Fresh slice per worker so each gets its own --node-name
				// without aliasing the shared base via append.
				args := append(append([]string{}, join...), "--node-name", w)
				_, err := m.rt.Exec(w, args...)
				return err
			})
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := m.installCNI(cp, cfg); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if cfg.Workers == 0 {
		if err := ui.Step("Untainting control plane for workloads", func() error {
			_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
				"taint", "nodes", "--all", "node-role.kubernetes.io/control-plane-")
			return err
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	// The three default addons apply independent manifests against the
	// same apiserver, so they install concurrently under one step.
	type addon struct {
		name  string // step title fragment and error prefix
		apply func() error
	}
	var addons []addon
	if !cfg.NoStorage {
		addons = append(addons, addon{"storage", func() error {
			_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
				"apply", "-f", "/kind/manifests/default-storage.yaml")
			return err
		}})
	}
	if !cfg.NoMetrics {
		addons = append(addons, addon{"metrics-server", func() error {
			return m.rt.ExecStdin(cp, strings.NewReader(metricsServerManifest),
				"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
		}})
	}
	if len(addons) > 0 {
		var names []string
		for _, a := range addons {
			names = append(names, a.name)
		}
		if err := ui.Step(fmt.Sprintf("Installing addons (%s)", strings.Join(names, ", ")), func() error {
			return inParallel(len(addons), func(i int) error {
				if err := addons[i].apply(); err != nil {
					return fmt.Errorf("%s: %w", addons[i].name, err)
				}
				return nil
			})
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	// kiac-lb is not a pod: it is a tiny systemd service inside the
	// control-plane VM driving the node's own kubectl, so it needs no
	// image pulls, RBAC, or webhook waits and assigns EXTERNAL-IPs
	// within seconds of enable --now.
	if !cfg.NoLB {
		if err := m.installKiacLB(cp); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if cfg.CNI == "none" {
		ui.Infof("nodes stay NotReady until you install a CNI; skipping readiness wait")
	} else if err := ui.Step("Waiting for nodes to be Ready", func() error {
		_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
			"wait", "--for=condition=Ready", "nodes", "--all",
			fmt.Sprintf("--timeout=%ds", int(cfg.WaitTimeout.Seconds())))
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if cfg.CNI != "none" {
		// A missing label degrades addon placement (Grafana/Traefik pods
		// pin to it with a nodeSelector, see labelLBPrimary), it does not
		// break the cluster, so this never triggers teardown.
		if err := ui.Step("Labeling LoadBalancer primary node", func() error {
			return m.labelLBPrimary(cp, cfg, nodes)
		}); err != nil {
			ui.Infof("LB primary node not labeled: %v", err)
			ui.Infof("the cluster works; label manually with: kubectl label node <node> kiac.io/lb-primary=true")
		}
	}

	if !cfg.NoEdgeProxy && cfg.CNI != "none" {
		if err := m.installEdgeProxy(cp, cfg, nodes); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	// Optional addons ride after the primary label: both expose Services
	// of type LoadBalancer and want an EXTERNAL-IP assignable immediately.
	// Failures degrade the cluster rather than tearing it down.
	if cfg.Observability && cfg.CNI != "none" {
		if err := m.installObservability(cp, cfg); err != nil {
			ui.Infof("observability stack not installed: %v", err)
		}
	}
	if cfg.Gateway && cfg.CNI != "none" {
		if err := m.installGateway(cp, cfg); err != nil {
			ui.Infof("gateway stack not installed: %v", err)
		}
	}

	var kubeconfigPath, serverIP string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", adminConf)
		if err != nil {
			return err
		}
		serverIP, err = m.serverIP(cp, cfg.family())
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(cfg.Name, raw, serverIP)
		return err
	}); err != nil {
		return err
	}

	// The API endpoint is healthy inside the VM, but vmnet may not have
	// made the control-plane VM host-reachable; probe before handing the
	// user a kubectl hint that would only error with "no route to host".
	reachable := false
	_ = ui.Step("Checking API server reachability from your Mac", func() error {
		reachable = hostReachAPI(serverIP, 10*time.Second)
		if !reachable {
			return fmt.Errorf("host cannot reach %s:6443", serverIP)
		}
		return nil
	})

	ui.Successf("Cluster %q is ready in %s. Every node is its own lightweight VM.",
		cfg.Name, time.Since(start).Round(time.Second))
	ui.Infof("context kiac-%s merged into %s", cfg.Name, kubeconfigPath)
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

// serverIP returns the control-plane address the host kubeconfig should
// target for the family: the vmnet IPv4 for ipv4/dual (the host reaches
// the VM over v4, and the apiserver advertises v4), the vmnet IPv6 for
// ipv6-only (where the apiserver advertises and certs itself on v6).
func (m *Manager) serverIP(cp string, family IPFamily) (string, error) {
	if family == IPv6 {
		return m.waitNodeIPv6(cp, 45*time.Second)
	}
	return m.rt.IP(cp)
}

// pinKubeletNodeIP writes the node's own addresses into
// /etc/default/kubelet as --node-ip before kubeadm ever starts kubelet,
// so the node registers the InternalIPs we intend instead of relying on
// kubelet's single-family auto-detection (which picks one address and,
// on dual-stack, often the wrong family). The kindest/node kubelet
// systemd drop-in sources /etc/default/kubelet last via
// KUBELET_EXTRA_ARGS, so this overrides kubeadm's own flags. For dual
// the value is "v4,v6" (v4 primary); for ipv6 it is the v6 alone.
func (m *Manager) pinKubeletNodeIP(node string, family IPFamily) error {
	nodeIP, err := m.nodeIPArg(node, family)
	if err != nil {
		return err
	}
	_, err = m.rt.Exec(node, "sh", "-c",
		fmt.Sprintf("printf 'KUBELET_EXTRA_ARGS=--node-ip=%s\\n' > /etc/default/kubelet", nodeIP))
	return err
}

// nodeIPArg resolves the --node-ip value for a node under the given
// family: "v4,v6" for dual, "v6" for ipv6. It fails if the node has no
// global IPv6 address, since that means the family cannot be satisfied.
func (m *Manager) nodeIPArg(node string, family IPFamily) (string, error) {
	v6, err := m.waitNodeIPv6(node, 45*time.Second)
	if err != nil {
		return "", err
	}
	if family == IPv6 {
		return v6, nil
	}
	v4, err := m.rt.IP(node)
	if err != nil {
		return "", fmt.Errorf("resolving IPv4 address of %s: %w", node, err)
	}
	return v4 + "," + v6, nil
}

// waitNodeIPv6 polls for a node's global IPv6 address, which is assigned
// by SLAAC a moment after the VM boots (the vmnet router advertisement
// has to arrive first), so a single lookup right after WaitReady often
// races ahead of it. Returns a clear "no IPv6" error if none appears in
// time, which for a create means the network is not IPv6-enabled.
func (m *Manager) waitNodeIPv6(node string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		v6, err := m.rt.IPv6(node)
		if err != nil {
			return "", fmt.Errorf("resolving IPv6 address of %s: %w", node, err)
		}
		if v6 != "" {
			return v6, nil
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("node %s got no global IPv6 address within %s; --ip-family dual/ipv6 needs an IPv6-enabled container network (macOS 26+); check: container network inspect default", node, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// installCNI applies the selected pod network. kindnet ships inside the
// node image; cilium requires the full custom kernel (--kernel full)
// and the cilium CLI on the host; "none" skips installation for other
// BYO CNIs.
func (m *Manager) installCNI(cp string, cfg Config) error {
	switch cfg.CNI {
	case "", "kindnet":
		return ui.Step("Installing CNI (kindnet)", func() error {
			subnet := cfg.family().podCIDR(kubeadmPodCIDRv4, kubeadmPodCIDRv6)
			_, err := m.rt.Exec(cp, "sh", "-euc",
				`sed -e 's@{{ .PodSubnet }}@`+subnet+`@' /kind/manifests/default-cni.yaml | kubectl --kubeconfig `+adminConf+` apply -f -`)
			return err
		})
	case "cilium":
		return m.installCilium(cp, cfg)
	case "flannel", "calico":
		return fmt.Errorf("%s needs kernel features the stock node kernel does not enable; use --cni cilium with --kernel full, or --cni none to bring your own", cfg.CNI)
	case "none":
		ui.Infof("skipping CNI: install your own before nodes go Ready (note: the stock kernel lacks br_netfilter/VXLAN/eBPF)")
		return nil
	default:
		return fmt.Errorf("unknown --cni %q (supported: kindnet, cilium, none)", cfg.CNI)
	}
}

// installCilium drives the host's cilium CLI (the official installer)
// against a temporary kubeconfig for the new cluster. Cilium's eBPF
// datapath needs the full custom kernel; the stock node kernel has no
// BPF JIT, VXLAN, or br_netfilter, so this path refuses to proceed
// without one rather than produce agents in a crash loop. Bonus of the
// vxlan tunnel datapath: cross-node pod traffic rides node-IP-addressed
// packets, which take vmnet's fast path (~285MB/s measured) instead of
// the slow forwarded path that punishes routed CNIs.
func (m *Manager) installCilium(cp string, cfg Config) error {
	if cfg.Kernel == "" {
		return fmt.Errorf("--cni cilium needs the full node kernel: add --kernel full (or a --kernel path)")
	}
	if _, err := exec.LookPath("cilium"); err != nil {
		return fmt.Errorf("--cni cilium drives the official installer, which is not on PATH; install it with: brew install cilium-cli")
	}
	return ui.Step("Installing CNI (Cilium)", func() error {
		raw, err := m.rt.Exec(cp, "cat", adminConf)
		if err != nil {
			return err
		}
		ip, err := m.rt.IP(cp)
		if err != nil {
			return err
		}
		kubeconfig, err := writeTempKubeconfig(cfg.Name, raw, ip)
		if err != nil {
			return err
		}
		defer os.Remove(kubeconfig)
		cmd := exec.Command("cilium", "install", "--wait")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cilium install: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return nil
	})
}

// preflight validates the host before any VM is booted.
func (m *Manager) preflight() error {
	if !m.rt.Available() {
		return fmt.Errorf("apple/container CLI not found on PATH; install it from https://github.com/apple/container/releases")
	}
	version, err := m.rt.Version()
	if err != nil {
		return fmt.Errorf("checking apple/container version: %w", err)
	}
	if err := runtime.ValidateNodeRuntimeVersion(version); err != nil {
		return err
	}
	if !m.rt.SystemRunning() {
		if err := m.rt.SystemStart(); err != nil {
			return fmt.Errorf("container system service is not running and could not be started: %w", err)
		}
	}
	return nil
}

// validateIPFamily rejects an IP family the host or the rest of the
// config cannot satisfy, before any VM boots. It does not touch the
// network (that is preflightIPv6Network, which needs the runtime); it
// only enforces the value itself and its interaction with the kernel and
// CNI. Kernel gating is by design: the stock kernel has no IPv6
// netfilter, so a non-ipv4 family without the full kernel would create a
// cluster whose IPv6 Services silently never connect.
func validateIPFamily(cfg Config) error {
	if !cfg.family().Valid() {
		return fmt.Errorf("invalid --ip-family %q (supported: ipv4, dual, ipv6)", cfg.IPFamily)
	}
	if !cfg.family().WantsIPv6() {
		return nil
	}
	if cfg.Kernel == "" {
		return fmt.Errorf("--ip-family %s needs the full node kernel (the stock kernel has no IPv6 netfilter); this should have been auto-selected, pass --kernel full explicitly", cfg.family())
	}
	if cfg.CNI == "cilium" {
		return fmt.Errorf("--ip-family %s does not support --cni cilium yet: Cilium's installer and IPAM are not wired for dual-stack CIDRs", cfg.family())
	}
	return nil
}

// preflightIPv6Network fails fast when an IPv6-carrying family is asked
// for but the container network hands out no IPv6 (older macOS, or a
// custom v4-only network). Without this the cluster would boot and only
// reveal the problem when a pod fails to get a v6 address.
func (m *Manager) preflightIPv6Network(cfg Config) error {
	if !cfg.family().WantsIPv6() {
		return nil
	}
	hasV6, err := m.rt.NetworkHasIPv6("default")
	if err != nil {
		return fmt.Errorf("checking the container network for IPv6 support: %w", err)
	}
	if !hasV6 {
		return fmt.Errorf("--ip-family %s needs an IPv6-enabled container network, but the default network advertises no IPv6 subnet (needs macOS 26+); check: container network inspect default", cfg.family())
	}
	return nil
}

// cleanupOnFailure tears down half-created node VMs so a failed create
// never leaves the host dirty. Errors are deliberately ignored.
func (m *Manager) cleanupOnFailure(name string) {
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return
	}
	var names []string
	for _, i := range infos {
		names = append(names, i.Name)
	}
	_ = m.rt.Remove(names...)
	fmt.Fprintf(os.Stderr, "\n  cleaned up partial cluster %q\n", name)
}

// Delete removes a cluster's node VMs and its kubeconfig entries.
func (m *Manager) Delete(name string) error {
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return fmt.Errorf("no cluster named %q found", name)
	}
	var names []string
	for _, i := range infos {
		names = append(names, i.Name)
	}
	if err := ui.Step(fmt.Sprintf("Deleting %d node VM(s)", len(names)), func() error {
		return m.rt.Remove(names...)
	}); err != nil {
		return err
	}
	if err := ui.Step("Removing kubeconfig entries", func() error {
		return removeKubeconfig(name)
	}); err != nil {
		return err
	}
	ui.Successf("Cluster %q deleted.", name)
	return nil
}

// Kubeconfig renders a standalone kubeconfig for one cluster.
func (m *Manager) Kubeconfig(name string) (string, error) {
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", fmt.Errorf("no cluster named %q found", name)
	}
	cp := ControlPlane(name)
	configPath := kubeconfigPathForNodes(infos)
	raw, err := m.rt.Exec(cp, "cat", configPath)
	if err != nil {
		return "", fmt.Errorf("reading %s from %s: %w", configPath, cp, err)
	}
	ip, err := m.rt.IP(cp)
	if err != nil {
		return "", err
	}
	return standaloneKubeconfig(name, raw, ip)
}

func kubeconfigPathForNodes(infos []runtime.Info) string {
	if distroFromNodes(infos) == "k3s" {
		return k3sKubeconfig
	}
	return adminConf
}

// Clusters lists cluster names derived from running kiac containers.
func (m *Manager) Clusters() ([]string, error) {
	infos, err := m.rt.List("kiac-")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, i := range infos {
		rest := strings.TrimPrefix(i.Name, "kiac-")
		var name string
		if idx := strings.LastIndex(rest, "-control-plane"); idx >= 0 {
			name = rest[:idx]
		} else if idx := strings.LastIndex(rest, "-worker-"); idx >= 0 {
			name = rest[:idx]
		} else {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// Nodes lists node containers for one cluster.
func (m *Manager) Nodes(name string) ([]runtime.Info, error) {
	return m.rt.List(prefix(name))
}

// LoadImages copies locally-built images into every node's containerd so
// pods can use them without a registry, mirroring `kind load docker-image`.
func (m *Manager) LoadImages(name string, images []string) error {
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return fmt.Errorf("no cluster named %q found", name)
	}
	for _, img := range images {
		tar, err := os.CreateTemp("", "kiac-image-*.tar")
		if err != nil {
			return err
		}
		tarPath := tar.Name()
		tar.Close()
		err = ui.Step(fmt.Sprintf("Loading %s into %d node(s)", img, len(infos)), func() error {
			if err := m.rt.ImageSave(img, tarPath); err != nil {
				return err
			}
			for _, node := range infos {
				f, err := os.Open(tarPath)
				if err != nil {
					return err
				}
				err = m.rt.ExecStdin(node.Name, f, "ctr", "-n", "k8s.io", "image", "import", "-")
				f.Close()
				if err != nil {
					return err
				}
			}
			return nil
		})
		os.Remove(tarPath)
		if err != nil {
			return err
		}
	}
	return nil
}

// inParallel runs fn(0..n-1) concurrently and waits for every call to
// finish: execs into node VMs cannot be cancelled mid-flight, so no
// goroutine may be abandoned. The lowest-index error wins, keeping the
// reported failure deterministic regardless of goroutine scheduling.
func inParallel(n int, fn func(i int) error) error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn(i)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func shortImage(image string) string {
	s := image
	if idx := strings.Index(s, "@"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimPrefix(s, "docker.io/")
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && strings.Contains(l, "kubeadm join") {
			return l
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
