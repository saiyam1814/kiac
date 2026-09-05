package cluster

import _ "embed"

// gpuDevicePluginManifest is rendered from squat/generic-device-plugin 0.2.0.
// The image is pinned to its multi-architecture manifest digest and the
// DaemonSet is restricted to Kiac's real Venus nodes.
//
//go:embed assets/gpu/device-plugin.yaml
var gpuDevicePluginManifest string

// gpuRuntimeClassManifest lets manifests that only set runtimeClassName:
// nvidia use the node's ordinary runc handler. It does not emulate CUDA and
// does not expose nvidia.com/gpu.
//
//go:embed assets/gpu/runtimeclass.yaml
var gpuRuntimeClassManifest string

// gpuDRAManifest runs the embedded node agent only on real Venus nodes and
// maps Kiac's DeviceClass to the same kiac.dev/gpu resource contract used by
// the device-plugin mode.
//
//go:embed assets/gpu/dra.yaml
var gpuDRAManifest string

// gpuCompatCoreManifest and gpuCompatWebhookManifest are rendered only when
// a user explicitly enables NVIDIA resource-name compatibility. Keeping the
// webhook out of cluster creation leaves the default GPU path with no extra
// control-plane process.
//
//go:embed assets/gpu/compat-core.yaml.tmpl
var gpuCompatCoreManifest string

//go:embed assets/gpu/compat-webhook.yaml.tmpl
var gpuCompatWebhookManifest string
