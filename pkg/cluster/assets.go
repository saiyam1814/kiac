package cluster

import _ "embed"

// metricsServerManifest is the upstream metrics-server components.yaml
// (v0.8.1) with --kubelet-insecure-tls added, since kubeadm-issued kubelet
// serving certs are self-signed in local clusters.
//
//go:embed assets/metrics-server.yaml
var metricsServerManifest string

// metallbManifest is the upstream MetalLB native manifest (v0.16.0).
// kiac runs it in L2 mode with a pool of the cluster's own node IPs:
// vmnet only delivers frames addressed to a VM's assigned IP, so floating
// VIPs never reach the guest, while node IPs are host-reachable and
// kube-proxy programs LoadBalancer ingress IPs into iptables.
//
//go:embed assets/metallb-native.yaml
var metallbManifest string

// Flannel/Calico/Cilium manifests are deliberately absent: the stock
// node kernel ships without CONFIG_BRIDGE_NETFILTER, VXLAN, and eBPF
// prerequisites, so they cannot start. Revisit when kiac supports
// custom kernels via `container run --kernel`.
