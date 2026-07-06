package cluster

import _ "embed"

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
