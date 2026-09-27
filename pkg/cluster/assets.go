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

// The Flannel manifest is embedded in flannel.go. Flannel and Cilium
// require explicitly selected kernels because runtime upgrades can retain
// older kernels without their prerequisites. Cilium uses its official CLI;
// Calico is not bundled.
