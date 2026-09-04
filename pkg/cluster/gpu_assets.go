package cluster

import _ "embed"

// The mock GPU addon is intentionally small: a namespace, a pass-through
// RuntimeClass, and squat/generic-device-plugin pinned to a multi-arch digest.
// It exposes a harmless marker device for scheduling tests; it does not make
// the Apple GPU available to a workload.

//go:embed assets/gpu/namespace.yaml
var gpuNamespaceManifest string

//go:embed assets/gpu/runtimeclass.yaml
var gpuRuntimeClassManifest string

//go:embed assets/gpu/device-plugin.yaml
var gpuDevicePluginManifest string

func gpuMockManifests() []string {
	return []string{
		gpuNamespaceManifest,
		gpuRuntimeClassManifest,
		gpuDevicePluginManifest,
	}
}
