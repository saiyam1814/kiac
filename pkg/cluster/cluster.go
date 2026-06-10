// Package cluster turns apple/container lightweight VMs into a Kubernetes
// cluster: one VM per node, kubeadm inside, kindnet CNI, metrics-server on
// by default.
package cluster

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

// DefaultNodeImage is the kindest/node build kiac boots by default. The
// digest is pinned because node images are only tested at exact digests.
const DefaultNodeImage = "docker.io/kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"

const adminConf = "/etc/kubernetes/admin.conf"

// Config describes a cluster to create.
type Config struct {
	Name        string
	Workers     int
	Image       string
	CPUs        string
	Memory      string
	NoMetrics   bool
	NoStorage   bool
	NoLB        bool
	WaitTimeout time.Duration
}

// Manager orchestrates cluster lifecycle on top of the container runtime.
type Manager struct {
	rt *runtime.Client
}

func NewManager() *Manager { return &Manager{rt: runtime.New()} }

func (m *Manager) Runtime() *runtime.Client { return m.rt }

func prefix(name string) string { return "kiac-" + name + "-" }

// ControlPlane returns the control-plane container name for a cluster.
func ControlPlane(name string) string { return prefix(name) + "control-plane" }

func worker(name string, i int) string { return fmt.Sprintf("%sworker-%d", prefix(name), i) }

// Create boots the node VMs and brings up Kubernetes. Progress is
// reported through ui.Step so the caller stays a thin cobra shim.
func (m *Manager) Create(cfg Config) error {
	start := time.Now()
	cp := ControlPlane(cfg.Name)

	existing, err := m.rt.List(prefix(cfg.Name))
	if err == nil && len(existing) > 0 {
		return fmt.Errorf("cluster %q already exists; delete it first with: kiac delete cluster --name %s", cfg.Name, cfg.Name)
	}

	if err := ui.Step("Preflight checks", func() error { return m.preflight() }); err != nil {
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

	if err := ui.Step(fmt.Sprintf("Booting %d node VM(s)", len(nodes)), func() error {
		for _, n := range nodes {
			if err := m.rt.RunDetached(runtime.RunOpts{Name: n, Image: cfg.Image, CPUs: cfg.CPUs, Memory: cfg.Memory}); err != nil {
				return err
			}
		}
		for _, n := range nodes {
			if err := m.rt.WaitReady(n, cfg.WaitTimeout); err != nil {
				return err
			}
			if _, err := m.rt.Exec(n, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if err := ui.Step("Initializing Kubernetes control plane", func() error {
		_, err := m.rt.Exec(cp, "kubeadm", "init",
			"--pod-network-cidr=10.244.0.0/16",
			"--ignore-preflight-errors=all")
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
			for i := 1; i <= cfg.Workers; i++ {
				if _, err := m.rt.Exec(worker(cfg.Name, i), join...); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := ui.Step("Installing CNI (kindnet)", func() error {
		_, err := m.rt.Exec(cp, "sh", "-euc",
			`sed -e 's@{{ .PodSubnet }}@10.244.0.0/16@' /kind/manifests/default-cni.yaml | kubectl --kubeconfig `+adminConf+` apply -f -`)
		return err
	}); err != nil {
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

	if !cfg.NoStorage {
		if err := ui.Step("Installing storage (local-path-provisioner)", func() error {
			_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
				"apply", "-f", "/kind/manifests/default-storage.yaml")
			return err
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if !cfg.NoMetrics {
		if err := ui.Step("Installing metrics-server", func() error {
			return m.rt.ExecStdin(cp, strings.NewReader(metricsServerManifest),
				"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if !cfg.NoLB {
		if err := ui.Step("Installing LoadBalancer (MetalLB)", func() error {
			return m.rt.ExecStdin(cp, strings.NewReader(metallbManifest),
				"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := ui.Step("Waiting for nodes to be Ready", func() error {
		_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
			"wait", "--for=condition=Ready", "nodes", "--all",
			fmt.Sprintf("--timeout=%ds", int(cfg.WaitTimeout.Seconds())))
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if !cfg.NoLB {
		if err := ui.Step("Configuring LoadBalancer IP pool", func() error {
			return m.configureLBPool(cp, cfg, nodes)
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	var kubeconfigPath string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", adminConf)
		if err != nil {
			return err
		}
		ip, err := m.rt.IP(cp)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(cfg.Name, raw, ip)
		return err
	}); err != nil {
		return err
	}

	ui.Successf("Cluster %q is ready in %s. Every node is its own lightweight VM.",
		cfg.Name, time.Since(start).Round(time.Second))
	ui.Hintf("kubectl get nodes")
	if !cfg.NoMetrics {
		ui.Hintf("kubectl top nodes        # native metrics, give it ~60s to scrape")
	}
	_ = kubeconfigPath
	return nil
}

// configureLBPool points MetalLB at the cluster's own node IPs. Worker
// IPs are pooled when workers exist (keeps control-plane ports like 6443
// out of the LB surface); the control plane is pooled on single-node
// clusters. The webhook needs a moment after rollout, so apply retries.
func (m *Manager) configureLBPool(cp string, cfg Config, nodes []string) error {
	pool := nodes
	if cfg.Workers > 0 {
		pool = nodes[1:]
	}
	var addrs []string
	for _, n := range pool {
		ip, err := m.rt.IP(n)
		if err != nil {
			return err
		}
		addrs = append(addrs, fmt.Sprintf("%q", ip+"/32"))
	}

	manifest := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kiac-node-ips
  namespace: metallb-system
spec:
  addresses: [%s]
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kiac-l2
  namespace: metallb-system
spec:
  ipAddressPools: [kiac-node-ips]
`, strings.Join(addrs, ", "))

	_, _ = m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
		"wait", "--for=condition=Available", "deployment/controller",
		"-n", "metallb-system", "--timeout=180s")

	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		lastErr = m.rt.ExecStdin(cp, strings.NewReader(manifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
		if lastErr == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("MetalLB webhook never became ready: %w", lastErr)
}

// preflight validates the host before any VM is booted.
func (m *Manager) preflight() error {
	if !m.rt.Available() {
		return fmt.Errorf("apple/container CLI not found on PATH; install it from https://github.com/apple/container/releases")
	}
	if !m.rt.SystemRunning() {
		if err := m.rt.SystemStart(); err != nil {
			return fmt.Errorf("container system service is not running and could not be started: %w", err)
		}
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
