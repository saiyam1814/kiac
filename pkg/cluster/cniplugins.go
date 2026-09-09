package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
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
func (m *Manager) extractCNIPlugins(node, archive string, timeout time.Duration, members ...string) error {
	// Member names select entries from the archive and appear in error
	// text. Every caller passes compile-time literals today; refuse
	// anything else so a future config-derived name cannot smuggle a
	// path or option through.
	for _, member := range members {
		if !cniPluginMember.MatchString(member) {
			return fmt.Errorf("refusing CNI plugin archive member %q: expected ./<name>", member)
		}
	}
	payload, err := selectCNIPluginMembers(archive, members)
	if err != nil {
		return err
	}
	// Only the selected binaries cross into the node, as a plain tar the
	// node unpacks without a member list on its command line.
	return m.rt.ExecStdinTimeout(node, transferBudget(timeout), bytes.NewReader(payload), "/bin/sh", "-c",
		"mkdir -p /opt/cni/bin && tar -x -C /opt/cni/bin")
}

// selectCNIPluginMembers re-packs just members from the verified gzip
// archive into an uncompressed tar. Every node then receives the few
// megabytes it needs instead of the whole 50MB archive, and a member
// missing from the archive fails here, before any node is touched.
func selectCNIPluginMembers(archive string, members []string) ([]byte, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("reading CNI plugins archive %s: %w", archive, err)
	}
	defer gz.Close()
	want := map[string]bool{}
	for _, member := range members {
		want[member] = true
	}
	found := map[string]bool{}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CNI plugins archive %s: %w", archive, err)
		}
		if !want[hdr.Name] {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("CNI plugins archive member %q is not a regular file", hdr.Name)
		}
		if err := tw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: hdr.Mode, Size: hdr.Size, ModTime: hdr.ModTime, Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, fmt.Errorf("reading CNI plugins archive member %q: %w", hdr.Name, err)
		}
		found[hdr.Name] = true
	}
	for _, member := range members {
		if !found[member] {
			return nil, fmt.Errorf("CNI plugins archive %s has no member %q", archive, member)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// transferBudget bounds a data transfer into a node (archive extraction,
// manifest apply) by the cluster wait budget, floored at a minute so a
// deliberately tiny --wait, which is a readiness knob, cannot cut a
// transfer that would have finished.
func transferBudget(wait time.Duration) time.Duration {
	return max(wait, time.Minute)
}

// cniPluginMember is the only shape of archive member extractCNIPlugins
// will pass to tar: a ./-relative plain binary name.
var cniPluginMember = regexp.MustCompile(`^\./[a-z0-9][a-z0-9._-]*$`)

// resolveCNIPluginsArchive is ensureCNIPluginsArchive behind a var so
// install paths can be exercised against a fake runtime without the
// download.
var resolveCNIPluginsArchive = ensureCNIPluginsArchive

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
