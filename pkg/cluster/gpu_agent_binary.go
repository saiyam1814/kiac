package cluster

import _ "embed"

// gpuAgentLinuxARM64Gzip is a stripped, static linux/arm64 build of
// ./internal/gpudriver. Kiac uploads it to GPU VMs before applying the DRA
// DaemonSet, so cluster creation never depends on a Kiac-specific image.
//
//go:embed assets/kiac-gpu-agent-linux-arm64.gz
var gpuAgentLinuxARM64Gzip []byte
