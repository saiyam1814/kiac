package cluster

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
	"gopkg.in/yaml.v3"
)

const (
	k3sSELinuxURL    = "https://rpm.rancher.io/k3s/stable/common/centos/8/noarch/k3s-selinux-1.6-1.el8.noarch.rpm"
	k3sSELinuxSHA256 = "a1e24b0d82b1a6806cd420e2c0398a1796b055efe368fa4aebc8bb173850934f"
	k3sSELinuxFile   = "k3s-selinux-1.6-1.el8.noarch.rpm"
)

type k3sBinaryAsset struct {
	Release string
	SHA256  string
}

// K3s publishes one statically linked ARM64 binary per release. These pins
// mirror the OCI image minors in versions.go, but the krunkit path installs
// the binary into a persistent Fedora VM instead of booting the OCI image.
var k3sARM64Assets = map[string]k3sBinaryAsset{
	"v1.32.13-k3s1": {Release: "v1.32.13+k3s1", SHA256: "66b5f9991fe6973169b3eb6e2200022474bb7242d702ada825fa467991717d3a"},
	"v1.33.13-k3s1": {Release: "v1.33.13+k3s1", SHA256: "007652573124c2e2b581894919d595fed0b7a9b2d54c9eb956d18bc43912f12b"},
	"v1.34.11-k3s1": {Release: "v1.34.11+k3s1", SHA256: "272f45b9efc69d0bbdb7042156156c6903087829a5003d4593af0ad2d08d76d4"},
	"v1.35.8-k3s1":  {Release: "v1.35.8+k3s1", SHA256: "898476e008704289382377ef19946f23b511cf2678042cb5c8aef991e64f840a"},
	"v1.36.4-k3s1":  {Release: "v1.36.4+k3s1", SHA256: "c920706346d5ad4e5cd3c7bf1bb09ce71ebe07fec829e513e40f1caf98aed8bb"},
}

type k3sGPUArtifacts struct {
	Binary  string
	SELinux string
}

func ensureK3sGPUArtifacts(version string) (k3sGPUArtifacts, error) {
	asset, ok := k3sARM64Assets[version]
	if !ok {
		return k3sGPUArtifacts{}, fmt.Errorf("K3s %q has no pinned ARM64 GPU-node binary; use one of the pinned minors %s", version, strings.Join(SupportedK3sVersions(), ", "))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return k3sGPUArtifacts{}, fmt.Errorf("resolving K3s cache: %w", err)
	}
	dir := filepath.Join(home, ".kiac", "k3s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return k3sGPUArtifacts{}, err
	}
	binary := filepath.Join(dir, "k3s-arm64-"+asset.Release)
	if err := verifyFileSHA256(binary, asset.SHA256); err != nil {
		url := "https://github.com/k3s-io/k3s/releases/download/" + asset.Release + "/k3s-arm64"
		if err := downloadVerified(url, binary, asset.SHA256); err != nil {
			return k3sGPUArtifacts{}, fmt.Errorf("downloading K3s %s: %w", asset.Release, err)
		}
	}
	selinux := filepath.Join(dir, k3sSELinuxFile)
	if err := verifyFileSHA256(selinux, k3sSELinuxSHA256); err != nil {
		if err := downloadVerified(k3sSELinuxURL, selinux, k3sSELinuxSHA256); err != nil {
			return k3sGPUArtifacts{}, fmt.Errorf("downloading K3s SELinux policy: %w", err)
		}
	}
	return k3sGPUArtifacts{Binary: binary, SELinux: selinux}, nil
}

func normalizeK3sStateVersion(version string) string {
	return strings.Replace(strings.TrimSpace(version), "+k3s", "-k3s", 1)
}

func (m *Manager) createK3sGPU(cfg Config) error {
	started := time.Now()
	cfg = gpuConfigForDistro(cfg, "k3s")
	if err := validateGPUClusterConfig(cfg); err != nil {
		return err
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
	var artifacts k3sGPUArtifacts
	if err := ui.Step("Preparing verified K3s artifacts", func() error {
		var err error
		artifacts, err = ensureK3sGPUArtifacts(normalizeK3sStateVersion(cfg.K8sVersion))
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
				Distro: "k3s", K8sVersion: cfg.K8sVersion, GPU: isGPUNode(node),
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

	if err := ui.Step("Provisioning K3s nodes", func() error {
		return inParallel(len(nodes), func(i int) error {
			return m.provisionK3sGPUNode(nodes[i], artifacts, isGPUNode(nodes[i]))
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
	token, err := k3sToken()
	if err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if err := ui.Step("Initializing K3s control plane", func() error {
		if err := m.configureK3sGPUNode(cp, "server", serverIP, serverIP, token, cfg); err != nil {
			return err
		}
		return m.waitK3sAPI(cp, cfg.WaitTimeout)
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	agents := nodes[1:]
	if len(agents) > 0 {
		if err := ui.Step(fmt.Sprintf("Joining %d K3s worker(s)", len(agents)), func() error {
			return inParallel(len(agents), func(i int) error {
				nodeIP, err := m.rt.IP(agents[i])
				if err != nil {
					return err
				}
				return m.configureK3sGPUNode(agents[i], "agent", nodeIP, serverIP, token, cfg)
			})
		}); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if err := ui.Step("Waiting for nodes to be Ready", func() error {
		return m.waitK3sNodes(cp, len(nodes), cfg.WaitTimeout)
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if err := m.installGPUResources(cp, cfg, gpuNodes); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	primary := cp
	if cfg.Workers > 0 {
		primary = worker(cfg.Name, 1)
	}
	if err := ui.Step("Labeling primary addon node", func() error {
		_, err := m.k3sKubectl(cp, "label", "node", primary, "kiac.io/lb-primary=true", "--overwrite")
		return err
	}); err != nil {
		m.cleanupOnFailure(cfg.Name)
		return err
	}
	if !cfg.NoLB {
		if err := m.installKiacLBK3sSystemd(cp); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}

	if !cfg.NoEdgeProxy {
		if err := m.installEdgeProxyK3sSystemd(cp, cfg, nodes); err != nil {
			m.cleanupOnFailure(cfg.Name)
			return err
		}
	}
	if cfg.Observability {
		if err := m.installObservabilityK3s(cp, cfg.NoLB); err != nil {
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
		m.cleanupOnFailure(cfg.Name)
		return err
	}

	if !hostReachAPI(serverIP, 10*time.Second) {
		warnGPUAPIUnreachable(cfg.Name, serverIP)
	}
	ui.Successf("K3s cluster %q with %d real Apple GPU worker(s) is ready in %s.", cfg.Name, cfg.GPUWorkers, time.Since(started).Round(time.Second))
	ui.Infof("context kiac-%s merged into %s", cfg.Name, kubeconfigPath)
	ui.Hintf("kubectl get nodes -L kiac.dev/gpu.present,kiac.dev/gpu.api")
	return nil
}

func validateGPUClusterConfig(cfg Config) error {
	if !ValidName(cfg.Name) {
		return fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", cfg.Name)
	}
	if cfg.GPUWorkers <= 0 {
		return fmt.Errorf("real GPU mode needs --gpu-workers greater than zero")
	}
	if cfg.Workers < 0 {
		return fmt.Errorf("--workers must be >= 0")
	}
	if cfg.family() != IPv4 {
		return fmt.Errorf("real GPU clusters currently require --ip-family ipv4")
	}
	if cfg.Kernel != "" {
		return fmt.Errorf("custom kernels cannot be combined with krunkit GPU nodes")
	}
	if cfg.Gateway && cfg.NoLB {
		return fmt.Errorf("--gateway needs the built-in LoadBalancer; drop --no-lb")
	}
	if cfg.GPUDriver != "device-plugin" && cfg.GPUDriver != "dra" {
		return fmt.Errorf("unknown --gpu-resource-driver %q (supported: device-plugin, dra)", cfg.GPUDriver)
	}
	if cfg.GPUDriver == "dra" {
		if err := requireDRAKubernetesVersion(cfg.K8sVersion); err != nil {
			return err
		}
	}
	if err := validateDNS(cfg); err != nil {
		return err
	}
	return runtime.ValidateMounts(cfg.Mounts)
}

func gpuClusterNodeNames(cfg Config) ([]string, []string) {
	nodes := []string{ControlPlane(cfg.Name)}
	for i := 1; i <= cfg.Workers; i++ {
		nodes = append(nodes, worker(cfg.Name, i))
	}
	gpuNodes := make([]string, 0, cfg.GPUWorkers)
	for i := 1; i <= cfg.GPUWorkers; i++ {
		name := gpuWorker(cfg.Name, i)
		nodes = append(nodes, name)
		gpuNodes = append(gpuNodes, name)
	}
	return nodes, gpuNodes
}

func validateGPUNodeNames(clusterName string, nodes []string) error {
	for _, node := range nodes {
		if len(node) > maxNodeNameLen {
			return fmt.Errorf("cluster name %q is too long: node name %q is %d chars, over the %d-char limit", clusterName, node, len(node), maxNodeNameLen)
		}
	}
	return nil
}

func isGPUNode(name string) bool {
	marker := strings.LastIndex(name, "-gpu-")
	if marker < 0 {
		return false
	}
	return canonicalPositiveIndex(name[marker+len("-gpu-"):])
}

func (m *Manager) ensureGPUClusterAbsent(name string) error {
	infos, err := m.krunkit.List(prefix(name))
	if err != nil {
		return err
	}
	if len(infos) > 0 {
		return fmt.Errorf("cluster %q already exists; delete it first with: kiac delete cluster --name %s", name, name)
	}
	if m.container != nil && m.container.Available() {
		if infos, err := m.container.List(prefix(name)); err == nil && len(infos) > 0 {
			return fmt.Errorf("cluster %q already exists in apple/container; delete it first with: kiac delete cluster --name %s", name, name)
		}
	}
	return nil
}

func (m *Manager) provisionK3sGPUNode(node string, artifacts k3sGPUArtifacts, gpu bool) error {
	if err := m.uploadFile(node, artifacts.Binary, "/usr/local/bin/k3s", 0o755); err != nil {
		return err
	}
	if err := m.uploadFile(node, artifacts.SELinux, "/var/lib/kiac/"+k3sSELinuxFile, 0o644); err != nil {
		return err
	}
	setup := `
dnf -y install container-selinux iptables ethtool /var/lib/kiac/` + k3sSELinuxFile + `
install -d -m 0755 /etc/modules-load.d /etc/sysctl.d /etc/rancher/k3s
printf '%s\n' overlay br_netfilter > /etc/modules-load.d/kiac-k3s.conf
cat > /etc/sysctl.d/90-kiac-k3s.conf <<'EOF'
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
modprobe overlay
modprobe br_netfilter
sysctl --system >/dev/null
` + senderOffloadFix + `
ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
ln -sf /usr/local/bin/k3s /usr/local/bin/ctr
ln -sf /usr/local/bin/k3s /usr/local/bin/crictl
cat > /usr/local/bin/nvidia-container-runtime <<'EOF'
#!/bin/sh
# Compatibility only: K3s maps RuntimeClass/nvidia to this ordinary runc
# wrapper. Device access still comes exclusively from kiac.dev/gpu.
for runc in /var/lib/rancher/k3s/data/current/bin/runc /var/lib/rancher/k3s/data/*/bin/runc; do
  [ -x "$runc" ] || continue
  exec "$runc" "$@"
done
echo "K3s runc binary not found" >&2
exit 127
EOF
chmod 0755 /usr/local/bin/nvidia-container-runtime
`
	if gpu {
		setup += `test -c /dev/dri/renderD128
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

func (m *Manager) uploadFile(node, source, destination string, mode os.FileMode) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	dir := filepath.Dir(destination)
	command := fmt.Sprintf("install -d -m 0755 %s; tmp=$(mktemp %s.XXXXXX); cat > \"$tmp\"; chmod %04o \"$tmp\"; mv \"$tmp\" %s", shQuote(dir), shQuote(destination), mode.Perm(), shQuote(destination))
	return m.rt.ExecStdin(node, f, "sh", "-euc", command)
}

func (m *Manager) configureK3sGPUNode(node, role, nodeIP, serverIP, token string, cfg Config) error {
	if net.ParseIP(nodeIP).To4() == nil || net.ParseIP(serverIP).To4() == nil {
		return fmt.Errorf("K3s node %s has invalid IPv4 addresses node=%q server=%q", node, nodeIP, serverIP)
	}
	config := map[string]any{
		"token":     token,
		"node-name": node,
		"node-ip":   nodeIP,
	}
	if role == "server" {
		disable := []string{"traefik", "servicelb"}
		if cfg.NoMetrics {
			disable = append(disable, "metrics-server")
		}
		if cfg.NoStorage {
			disable = append(disable, "local-storage")
		}
		config["disable"] = disable
		config["tls-san"] = []string{node, serverIP}
		config["advertise-address"] = serverIP
		config["write-kubeconfig-mode"] = "0600"
	} else if role == "agent" {
		config["server"] = "https://" + net.JoinHostPort(serverIP, "6443")
	} else {
		return fmt.Errorf("invalid K3s role %q", role)
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := m.rt.ExecStdin(node, strings.NewReader(string(raw)), "sh", "-euc", `
install -d -m 0700 /etc/rancher/k3s
tmp="$(mktemp /etc/rancher/k3s/config.yaml.XXXXXX)"
cat > "$tmp"
chmod 0600 "$tmp"
mv "$tmp" /etc/rancher/k3s/config.yaml
`); err != nil {
		return err
	}
	if _, err := m.ensureK3sGPUUnit(node, role); err != nil {
		return err
	}
	_, err = m.rt.Exec(node, "sh", "-euc", "systemctl enable k3s.service; systemctl --no-block start k3s.service")
	return err
}

func (m *Manager) ensureK3sGPUUnit(node, role string) (bool, error) {
	want := k3sGPUUnit(role)
	current, err := m.rt.Exec(node, "cat", "/etc/systemd/system/k3s.service")
	if err == nil && current == want {
		return false, nil
	}
	if err := m.rt.ExecStdin(node, strings.NewReader(want), "sh", "-euc", `
tmp="$(mktemp /etc/systemd/system/k3s.service.XXXXXX)"
cat > "$tmp"
chmod 0644 "$tmp"
mv "$tmp" /etc/systemd/system/k3s.service
systemctl daemon-reload
`); err != nil {
		return false, err
	}
	return true, nil
}

func k3sGPUUnit(role string) string {
	return `[Unit]
Description=Lightweight Kubernetes
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target

[Service]
# Kiac performs bounded API and node readiness checks after starting the
# service. Type=exec avoids an unbounded systemctl wait if an agent never
# emits sd_notify despite having registered successfully.
Type=exec
EnvironmentFile=-/etc/default/%N
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStart=/usr/local/bin/k3s ` + role + `

[Install]
WantedBy=multi-user.target
`
}

func (m *Manager) installKiacLBK3sSystemd(cp string) error {
	unit := strings.Replace(kiacLBUnit, "After=kubelet.service", "After=k3s.service", 1)
	unit = strings.Replace(unit, "[Service]\n", "[Service]\nEnvironment=KUBECONFIG="+k3sKubeconfig+"\n", 1)
	return ui.Step("Installing LoadBalancer (kiac-lb)", func() error {
		for _, step := range []struct {
			content string
			path    string
			mode    string
		}{{kiacLBScript, kiacLBScriptPath, "0755"}, {unit, kiacLBUnitPath, "0644"}} {
			if err := m.rt.ExecStdin(cp, strings.NewReader(step.content), "sh", "-euc", "cat > "+step.path+"; chmod "+step.mode+" "+step.path); err != nil {
				return err
			}
		}
		_, err := m.rt.Exec(cp, "sh", "-euc", "systemctl daemon-reload; systemctl enable --now kiac-lb.service")
		return err
	})
}

func (m *Manager) installEdgeProxyK3sSystemd(cp string, cfg Config, nodes []string) error {
	return ui.Step("Installing edge proxy (large upload fix)", func() error {
		kubeconfig, err := m.edgeProxyKubeconfig(cp, cfg.Name, k3sKubeconfig)
		if err != nil {
			return err
		}
		tunnelToken, err := newEdgeProxyTunnelToken()
		if err != nil {
			return err
		}
		if err := m.installEdgeProxyFiles(nodes, kubeconfig, tunnelToken); err != nil {
			return err
		}
		if err := m.installEdgeProxySystemd(nodes); err != nil {
			return err
		}
		return m.waitEdgeProxyRules(nodes)
	})
}
