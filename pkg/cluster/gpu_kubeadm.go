package cluster

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

type kubeadmGPUBinary struct {
	Name   string
	SHA256 string
}

type kubeadmGPURelease struct {
	Version  string
	Binaries []kubeadmGPUBinary
}

// The systemd-backed kubeadm path installs official Kubernetes binaries
// directly into the Fedora VM. Start with the current pinned release; unlike
// an unpinned package repository, a missing or changed byte fails closed.
var kubeadmGPUReleases = map[string]kubeadmGPURelease{
	"v1.37.0": {
		Version: "v1.37.0",
		Binaries: []kubeadmGPUBinary{
			{Name: "kubeadm", SHA256: "f0fb888ee3eea7ee01c84151ec1ca9df8fa2fc1207e0265dea9aac8db50510dc"},
			{Name: "kubelet", SHA256: "7855ce2c3a3393d3c871fdeedcef32e38531f6852179130764763a9da79e2aa8"},
			{Name: "kubectl", SHA256: "922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3"},
		},
	},
}

func ensureKubeadmGPUArtifacts(version string) (map[string]string, error) {
	release, ok := kubeadmGPUReleases[version]
	if !ok {
		return nil, fmt.Errorf("Kubernetes %q has no pinned kubeadm GPU-node binaries; currently supported: v1.37.0", version)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving Kubernetes cache: %w", err)
	}
	dir := filepath.Join(home, ".kiac", "kubernetes", release.Version, "linux-arm64")
	paths := make(map[string]string, len(release.Binaries))
	for _, binary := range release.Binaries {
		destination := filepath.Join(dir, binary.Name)
		if err := verifyFileSHA256(destination, binary.SHA256); err != nil {
			url := "https://dl.k8s.io/release/" + release.Version + "/bin/linux/arm64/" + binary.Name
			if err := downloadVerified(url, destination, binary.SHA256); err != nil {
				return nil, fmt.Errorf("downloading Kubernetes %s %s: %w", release.Version, binary.Name, err)
			}
		}
		if err := os.Chmod(destination, 0o755); err != nil {
			return nil, err
		}
		paths[binary.Name] = destination
	}
	return paths, nil
}

func (m *Manager) createKubeadmGPU(cfg Config) error {
	started := time.Now()
	cfg = gpuConfigForDistro(cfg, "kubeadm")
	if err := validateGPUClusterConfig(cfg); err != nil {
		return err
	}
	if cfg.CNI == "" {
		cfg.CNI = "kindnet"
	}
	if cfg.CNI != "kindnet" && cfg.CNI != "cilium" {
		return fmt.Errorf("real GPU kubeadm clusters support --cni kindnet or cilium")
	}
	if cfg.CNI == "cilium" {
		if _, err := exec.LookPath("cilium"); err != nil {
			return fmt.Errorf("--cni cilium drives the official installer; install it with: brew install cilium-cli")
		}
	}
	cp := ControlPlane(cfg.Name)
	nodes, gpuNodes := gpuClusterNodeNames(cfg)
	if err := validateGPUNodeNames(cfg.Name, nodes); err != nil {
		return err
	}
	if err := m.ensureGPUClusterAbsent(cfg.Name); err != nil {
		return err
	}
	if err := ui.Step("Preflight checks for Apple GPU VMs", m.krunkit.Preflight); err != nil {
		return err
	}

	var image string
	if err := ui.Step("Preparing verified Fedora GPU image", func() error {
		var err error
		image, err = ResolveGPUImage(cfg.GPUImage)
		return err
	}); err != nil {
		return err
	}
	var binaries map[string]string
	if err := ui.Step("Preparing verified Kubernetes binaries", func() error {
		var err error
		binaries, err = ensureKubeadmGPUArtifacts(cfg.K8sVersion)
		return err
	}); err != nil {
		return err
	}

	dns := nodeDNS(cfg)
	if err := ui.Step(fmt.Sprintf("Booting %d krunkit node VM(s)", len(nodes)), func() error {
		for _, node := range nodes {
			memory := cfg.Memory
			if node == cp && cfg.CPMemory != "" {
				memory = cfg.CPMemory
			}
			if err := m.rt.RunDetached(runtime.RunOpts{
				Backend: runtime.BackendKrunkit, Name: node, Image: image,
				Distro: "kubeadm", K8sVersion: cfg.K8sVersion, GPU: isGPUNode(node),
				CPUs: cfg.CPUs, Memory: memory, DiskSize: cfg.GPUDiskSize,
				NetworkID: cfg.Name, DNS: dns, Mounts: cfg.Mounts,
			}); err != nil {
				return err
			}
		}
		return inParallel(len(nodes), func(i int) error {
			return m.rt.WaitReady(nodes[i], cfg.WaitTimeout)
		})
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if err := ui.Step("Provisioning kubeadm nodes", func() error {
		return inParallel(len(nodes), func(i int) error {
			nodeIP, err := m.rt.IP(nodes[i])
			if err != nil {
				return err
			}
			return m.provisionKubeadmGPUNode(nodes[i], nodeIP, binaries, isGPUNode(nodes[i]))
		})
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	serverIP, err := m.rt.IP(cp)
	if err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if net.ParseIP(serverIP).To4() == nil {
		m.cleanupOnFailure(cfg.Name)
		return fmt.Errorf("kubeadm control-plane address %q is not IPv4", serverIP)
	}
	if err := ui.Step("Pulling Kubernetes control-plane images", func() error {
		_, err := m.rt.Exec(cp, "sh", "-euc", `
for attempt in 1 2 3; do
  kubeadm config images pull --kubernetes-version "$1" --cri-socket unix:///run/containerd/containerd.sock && exit 0
  sleep $((attempt * 3))
done
exit 1
`, "sh", cfg.K8sVersion)
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if err := ui.Step("Initializing Kubernetes control plane", func() error {
		_, err := m.rt.Exec(cp, "kubeadm", "init",
			"--kubernetes-version="+cfg.K8sVersion,
			"--apiserver-advertise-address="+serverIP,
			"--pod-network-cidr="+kubeadmPodCIDRv4,
			"--node-name="+cp,
			"--cri-socket=unix:///run/containerd/containerd.sock",
			"--ignore-preflight-errors=SystemVerification")
		return err
	}); err != nil {
		diagnostics := m.kubeadmGPUNodeDiagnostics(cp)
		m.cleanupOnFailure(cfg.Name)
		if diagnostics != "" {
			return fmt.Errorf("%w\ncontrol-plane diagnostics before cleanup:\n%s", err, diagnostics)
		}
		return err
	}

	workers := nodes[1:]
	if len(workers) > 0 {
		if err := ui.Step(fmt.Sprintf("Joining %d kubeadm worker(s)", len(workers)), func() error {
			joinCommand, err := m.rt.Exec(cp, "kubeadm", "token", "create", "--print-join-command")
			if err != nil {
				return err
			}
			join := strings.Fields(lastNonEmptyLine(joinCommand))
			if len(join) == 0 || join[0] != "kubeadm" {
				return fmt.Errorf("unexpected join command: %q", joinCommand)
			}
			join = append(join, "--cri-socket=unix:///run/containerd/containerd.sock", "--ignore-preflight-errors=SystemVerification")
			return inParallel(len(workers), func(i int) error {
				args := append(append([]string(nil), join...), "--node-name="+workers[i])
				_, err := m.rt.Exec(workers[i], args...)
				return err
			})
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := m.installKubeadmGPUCNI(cp, cfg); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if cfg.Workers == 0 {
		if err := ui.Step("Untainting control plane for workloads", func() error {
			_, err := m.gpuKubectl(cp, "kubeadm", "taint", "node", cp, "node-role.kubernetes.io/control-plane-")
			return err
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}
	if err := ui.Step("Waiting for nodes to be Ready", func() error {
		_, err := m.gpuKubectl(cp, "kubeadm", "wait", "--for=condition=Ready", "nodes", "--all",
			fmt.Sprintf("--timeout=%ds", int(cfg.WaitTimeout.Seconds())))
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if err := m.installGPUResources(cp, cfg, gpuNodes); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if !cfg.NoStorage {
		if err := ui.Step("Installing local-path storage", func() error {
			return m.gpuKubectlStdin(cp, "kubeadm", strings.NewReader(kubeadmSystemdStorageManifest), "apply", "-f", "-")
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}
	if !cfg.NoMetrics {
		if err := ui.Step("Installing metrics-server", func() error {
			return m.gpuKubectlStdin(cp, "kubeadm", strings.NewReader(metricsServerManifest), "apply", "-f", "-")
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := ui.Step("Labeling primary addon node", func() error {
		return m.labelLBPrimary(cp, cfg, nodes)
	}); err != nil {
		ui.Infof("primary addon node not labeled: %v", err)
	}
	if !cfg.NoLB {
		if err := m.installKiacLB(cp); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}
	if !cfg.NoEdgeProxy {
		if err := m.installEdgeProxy(cp, cfg, nodes); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}
	if cfg.Observability {
		if err := m.installObservability(cp, cfg); err != nil {
			ui.Infof("observability stack not installed: %v", err)
		}
	}
	if cfg.Gateway {
		if err := m.installGateway(cp, cfg); err != nil {
			ui.Infof("gateway stack not installed: %v", err)
		}
	}

	var kubeconfigPath string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", adminConf)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(cfg.Name, raw, serverIP)
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if !hostReachAPI(serverIP, 10*time.Second) {
		warnGPUAPIUnreachable(cfg.Name, serverIP)
	}
	ui.Successf("kubeadm cluster %q with %d real Apple GPU worker(s) is ready in %s.", cfg.Name, cfg.GPUWorkers, time.Since(started).Round(time.Second))
	ui.Infof("context kiac-%s merged into %s", cfg.Name, kubeconfigPath)
	ui.Hintf("kubectl get nodes -L kiac.dev/gpu.present,kiac.dev/gpu.api")
	return nil
}

func (m *Manager) kubeadmGPUNodeDiagnostics(node string) string {
	out, err := m.rt.ExecTimeout(node, 20*time.Second, "sh", "-c", `
echo '== kubelet/containerd =='
journalctl -u kubelet.service -u containerd.service --no-pager -n 160 2>&1 || true
echo '== static pod containers =='
ctr -n k8s.io containers list 2>&1 || true
echo '== listening sockets =='
ss -lnt 2>&1 || true
`)
	if err != nil {
		return "diagnostics unavailable: " + compactError(err)
	}
	return strings.TrimSpace(out)
}

func (m *Manager) provisionKubeadmGPUNode(node, nodeIP string, binaries map[string]string, gpu bool) error {
	if net.ParseIP(nodeIP).To4() == nil {
		return fmt.Errorf("kubeadm node %s has invalid IPv4 address %q", node, nodeIP)
	}
	for _, name := range []string{"kubeadm", "kubelet", "kubectl"} {
		if err := m.uploadFile(node, binaries[name], "/usr/local/bin/"+name, 0o755); err != nil {
			return err
		}
	}
	setup := `
dnf -y install containerd containernetworking-plugins conntrack-tools socat ethtool iptables
ln -sf /dev/null /etc/systemd/zram-generator.conf
systemctl mask --now dev-zram0.swap >/dev/null 2>&1 || true
swapoff -a
sed -ri '/[[:space:]]swap[[:space:]]/ s/^/#/' /etc/fstab
install -d -m 0755 /etc/containerd /etc/modules-load.d /etc/sysctl.d /etc/systemd/system/kubelet.service.d /opt/cni/bin /var/lib/kubelet
cp -a /usr/libexec/cni/. /opt/cni/bin/
containerd config default > /etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/g' /etc/containerd/config.toml
printf '%s\n' overlay br_netfilter > /etc/modules-load.d/kiac-kubernetes.conf
cat > /etc/sysctl.d/90-kiac-kubernetes.conf <<'EOF'
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
modprobe overlay
modprobe br_netfilter
sysctl --system >/dev/null
` + senderOffloadFix + `
cat > /etc/systemd/system/kubelet.service <<'EOF'
[Unit]
Description=kubelet: The Kubernetes Node Agent
Documentation=https://kubernetes.io/docs/
Wants=network-online.target
After=network-online.target containerd.service

[Service]
ExecStart=/usr/local/bin/kubelet
Restart=always
StartLimitInterval=0
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
cat > /etc/systemd/system/kubelet.service.d/10-kubeadm.conf <<'EOF'
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
EnvironmentFile=-/etc/sysconfig/kubelet
ExecStart=
ExecStart=/usr/local/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
EOF
printf '%s\n' 'KUBELET_EXTRA_ARGS=--fail-swap-on=false --node-ip=` + nodeIP + `' > /etc/sysconfig/kubelet
systemctl daemon-reload
systemctl enable --now containerd.service kubelet.service
`
	if gpu {
		setup += `
test -c /dev/dri/renderD128
cat > /etc/udev/rules.d/70-kiac-gpu.rules <<'EOF'
SUBSYSTEM=="drm", KERNEL=="card[0-9]*", MODE="0666"
SUBSYSTEM=="drm", KERNEL=="renderD[0-9]*", MODE="0666"
EOF
udevadm trigger --subsystem-match=drm
`
	}
	_, err := m.rt.Exec(node, "sh", "-euc", setup)
	return err
}

func (m *Manager) installKubeadmGPUCNI(cp string, cfg Config) error {
	switch cfg.CNI {
	case "kindnet":
		manifest := strings.Replace(k3sKindnetManifest, k3sPodCIDRv4, kubeadmPodCIDRv4, 1)
		return ui.Step("Installing CNI (kindnet)", func() error {
			return m.gpuKubectlStdin(cp, "kubeadm", strings.NewReader(manifest), "apply", "-f", "-")
		})
	case "cilium":
		return m.installCilium(cp, cfg)
	default:
		return fmt.Errorf("unsupported kubeadm GPU CNI %q", cfg.CNI)
	}
}
