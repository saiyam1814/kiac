package cluster

import _ "embed"

// edgeProxyLinuxARM64Gzip is a stripped, static linux/arm64 build of
// ./cmd/kiac-edge-proxy. It is copied into each node VM and run directly
// on the node, avoiding any proxy image pull on cluster create.
//
//go:embed assets/kiac-edge-proxy-linux-arm64.gz
var edgeProxyLinuxARM64Gzip []byte
