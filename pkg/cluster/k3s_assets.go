package cluster

import (
	_ "embed"
	"strings"
)

// k3sKindnetManifest replaces k3s's bundled flannel (started with
// --flannel-backend=none). Flannel wires pods onto a cni0 bridge, and
// the node kernel has no br_netfilter, so same-node pod-to-ClusterIP
// return traffic bypasses iptables un-DNAT and every same-node service
// call times out. kindnet's PTP veth topology keeps all pod traffic
// routed, exactly like the kubeadm path. Vendored from the pinned
// kindest/node image's /kind/manifests/default-cni.yaml with the pod
// subnet set to k3s's default cluster CIDR.
//
//go:embed assets/k3s/kindnet.yaml
var k3sKindnetManifest string

// k3sKindnetManifestFor returns the kindnet manifest with its POD_SUBNET
// rewritten for the family. The vendored manifest hardcodes k3s's IPv4
// default (10.42.0.0/16); dual-stack needs kindnet to hand out both
// families, so the value becomes the comma-joined dual CIDR. IPv4 keeps
// the manifest untouched (ipv6-only k3s is rejected before this runs).
func k3sKindnetManifestFor(family IPFamily) string {
	if family != DualStack {
		return k3sKindnetManifest
	}
	return strings.Replace(k3sKindnetManifest, k3sPodCIDRv4,
		family.podCIDR(k3sPodCIDRv4, k3sPodCIDRv6), 1)
}
