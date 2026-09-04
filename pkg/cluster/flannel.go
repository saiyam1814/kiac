package cluster

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

// FlannelVersion is the pinned upstream flannel release embedded in the
// binary. The manifest is the release's kube-flannel.yml verbatim; bump
// both together and re-verify the delegate plugin set below.
const FlannelVersion = "v0.28.9"

// flannelManifest is upstream kube-flannel.yml at FlannelVersion,
// unmodified. Its pod network is patched at apply time (see
// flannelManifestWithCIDR) so the manifest stays byte-identical to the
// published release and auditable against it.
//
//go:embed assets/flannel.yaml
var flannelManifest string

// flannelUpstreamCIDR is the pod network upstream ships in net-conf.json.
// It happens to equal kiac's kubeadm IPv4 pod CIDR, but the apply path
// still rewrites it from Config so the two can never drift apart.
const flannelUpstreamCIDR = `"Network": "10.244.0.0/16"`

// flannelManifestWithCIDR returns the embedded manifest with its pod
// network set to cidr. It fails loudly if the upstream marker is gone -
// a version bump that reshapes net-conf.json must be noticed, not
// silently applied with the wrong network.
func flannelManifestWithCIDR(cidr string) (string, error) {
	patched := strings.Replace(flannelManifest, flannelUpstreamCIDR, `"Network": "`+cidr+`"`, 1)
	if patched == flannelManifest && !strings.Contains(flannelManifest, `"Network": "`+cidr+`"`) {
		return "", fmt.Errorf("embedded flannel manifest %s has no %s marker; the manifest changed shape and needs review", FlannelVersion, flannelUpstreamCIDR)
	}
	return patched, nil
}

// ensureFlannelDelegatePlugins streams the delegate CNI binaries
// flannel's conflist needs but the kindest/node image does not ship.
// The conflist delegates to bridge (isDefaultGateway) with host-local
// IPAM and chains portmap; the node image already carries host-local,
// portmap, and loopback for kindnet, leaving only bridge. It comes from
// kiac's existing sha-verified plugins archive - no second download
// path. Bridge is safe here because flannel is gated on the full
// kernel, which enables br_netfilter; without it bridged same-node
// Service traffic would bypass iptables un-DNAT (the reason the k3s
// path avoids bridge on the stock kernel).
func (m *Manager) ensureFlannelDelegatePlugins(node string) error {
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
		"mkdir -p /opt/cni/bin && tar -xz -C /opt/cni/bin ./bridge")
}

// installFlannel applies the embedded, version-pinned upstream flannel
// manifest. Flannel's VXLAN backend needs the full custom kernel (the
// stock node kernel has no VXLAN or br_netfilter), and the create path
// already refused to boot without one; this re-check keeps the
// invariant local. VXLAN carries cross-node pod traffic in node-IP
// -addressed packets, which take vmnet's fast path - the same reason
// Cilium runs its tunnel datapath here.
func (m *Manager) installFlannel(cp string, cfg Config) error {
	if cfg.Kernel == "" {
		return fmt.Errorf("--cni flannel needs the full node kernel: add --kernel full (or a --kernel path)")
	}
	nodes := []string{cp}
	for i := 1; i <= cfg.Workers; i++ {
		nodes = append(nodes, worker(cfg.Name, i))
	}
	return ui.Step(fmt.Sprintf("Installing CNI (flannel %s)", FlannelVersion), func() error {
		if err := inParallel(len(nodes), func(i int) error {
			if err := m.ensureFlannelDelegatePlugins(nodes[i]); err != nil {
				return fmt.Errorf("installing bridge CNI plugin on %s: %w", nodes[i], err)
			}
			return nil
		}); err != nil {
			return err
		}
		manifest, err := flannelManifestWithCIDR(cfg.family().podCIDR(kubeadmPodCIDRv4, kubeadmPodCIDRv6))
		if err != nil {
			return err
		}
		if err := m.rt.ExecStdin(cp, strings.NewReader(manifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-"); err != nil {
			return err
		}
		return m.waitFlannelReady(cp, cfg.WaitTimeout)
	})
}

// waitFlannelReady blocks until the kube-flannel DaemonSet is rolled
// out on every node, honoring the configured wait timeout. On timeout
// the error carries the pod list and recent logs, so a failure names
// the crashing pod instead of just "timed out".
func (m *Manager) waitFlannelReady(cp string, timeout time.Duration) error {
	_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
		"-n", "kube-flannel", "rollout", "status", "daemonset/kube-flannel-ds",
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err == nil {
		return nil
	}
	diag, diagErr := m.rt.Exec(cp, "sh", "-c",
		"kubectl --kubeconfig "+adminConf+" -n kube-flannel get pods -o wide; "+
			"kubectl --kubeconfig "+adminConf+" -n kube-flannel logs daemonset/kube-flannel-ds --tail=20 --prefix 2>/dev/null")
	if diagErr != nil || strings.TrimSpace(diag) == "" {
		return fmt.Errorf("flannel did not become ready within %s: %w", timeout, err)
	}
	return fmt.Errorf("flannel did not become ready within %s: %w\n%s", timeout, err, strings.TrimSpace(diag))
}
