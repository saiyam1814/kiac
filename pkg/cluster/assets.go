package cluster

import _ "embed"

// metricsServerManifest is the upstream metrics-server components.yaml
// (v0.8.1) with --kubelet-insecure-tls added, since kubeadm-issued kubelet
// serving certs are self-signed in local clusters.
//
//go:embed assets/metrics-server.yaml
var metricsServerManifest string
