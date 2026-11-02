package cluster

import _ "embed"

// gpuAgentLinuxARM64Gzip is a stripped, static linux/arm64 build of
// ./internal/gpudriver. Kiac uploads it to GPU VMs before applying either GPU driver
// DaemonSet, so cluster creation never depends on a Kiac-specific image.
//
//go:embed assets/kiac-gpu-agent-linux-arm64.gz
var gpuAgentLinuxARM64Gzip []byte

// gpuProbeLinuxARM64Gzip is the separate native Vulkan loader executable.
// It runs only inside the pinned Fedora sidecar, which supplies its shared libraries.
// Keeping it separate preserves a static main agent and compatibility webhook.
//
//go:embed assets/kiac-vulkan-probe-linux-arm64.gz
var gpuProbeLinuxARM64Gzip []byte
