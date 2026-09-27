package cluster

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var kubeadmVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// Use the version installed in the selected image. Without an explicit
// version, kubeadm resolves upstream stable and can silently run a newer
// control plane than the pinned node image and its kubelet.
func (m *Manager) initKubeadm(cp string, cfg Config) error {
	version, err := m.rt.Exec(cp, "kubeadm", "version", "-o", "short")
	if err != nil {
		return fmt.Errorf("reading Kubernetes version from node image: %w", err)
	}
	version = strings.TrimSpace(version)
	if !kubeadmVersionPattern.MatchString(version) {
		return fmt.Errorf("node image returned invalid kubeadm version %q", version)
	}
	args := []string{"init", "--kubernetes-version=" + version,
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
	_, err = m.rt.Exec(cp, append([]string{"kubeadm"}, args...)...)
	return err
}
