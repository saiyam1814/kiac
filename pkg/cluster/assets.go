package cluster

import _ "embed"

// metricsServerManifest is the upstream metrics-server components.yaml
// (v0.8.1) with --kubelet-insecure-tls added, since kubeadm-issued kubelet
// serving certs are self-signed in local clusters.
//
//go:embed assets/metrics-server.yaml
var metricsServerManifest string

// LoadBalancer support has no manifest: kiac-lb (see lb.go) runs as a
// systemd service inside the control-plane VM, not as pods, because the
// only host-reachable LoadBalancer addresses under vmnet are the node
// IPs themselves and kube-proxy programs ingress IPs into iptables.

// Flannel/Calico/Cilium manifests are deliberately absent: the stock
// node kernel ships without CONFIG_BRIDGE_NETFILTER, VXLAN, and eBPF
// prerequisites, so they cannot start. Revisit when kiac supports
// custom kernels via `container run --kernel`.
