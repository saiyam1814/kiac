package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The upstream CNI plugin binaries the k3s distro needs. kindnet's
// conflist references ptp, which k3s's bundled multicall binary does
// not implement (its flannel path only needs bridge/loopback/etc), and
// the bridge plugin is not an option: the node kernel has no
// br_netfilter, so bridged same-node service traffic bypasses iptables
// un-DNAT. The official release archive is downloaded once, verified,
// cached beside the kernels, and streamed into each node VM.
const (
	cniPluginsVersion = "v1.7.1"
	cniPluginsSHA256  = "119fcb508d1ac2149e49a550752f9cd64d023a1d70e189b59c476e4d2bf7c497"
	cniPluginsURL     = "https://github.com/containernetworking/plugins/releases/download/" +
		cniPluginsVersion + "/cni-plugins-linux-arm64-" + cniPluginsVersion + ".tgz"
)

// extractCNIPlugins streams the named members of the verified plugins
// archive into /opt/cni/bin of a node VM. Callers resolve the archive
// once via ensureCNIPluginsArchive and pass its path, so a fan-out over
// many nodes reads the already-cached file instead of racing N
// downloads. tar exits non-zero on a missing member, which ExecStdin
// surfaces as an error.
func (m *Manager) extractCNIPlugins(node, archive string, members ...string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return m.rt.ExecStdin(node, f, "/bin/sh", "-c",
		"mkdir -p /opt/cni/bin && tar -xz -C /opt/cni/bin "+strings.Join(members, " "))
}

// ensureCNIPluginsArchive returns the local path of the verified CNI
// plugins archive, downloading it on first use.
func ensureCNIPluginsArchive() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving cache dir: %w", err)
	}
	dir := filepath.Join(home, ".kiac")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "cni-plugins-linux-arm64-"+cniPluginsVersion+".tgz")
	if err := verifyFileSHA256(dest, cniPluginsSHA256); err == nil {
		return dest, nil
	}
	if err := downloadVerified(cniPluginsURL, dest, cniPluginsSHA256); err != nil {
		return "", fmt.Errorf("downloading CNI plugins %s: %w", cniPluginsVersion, err)
	}
	return dest, nil
}
