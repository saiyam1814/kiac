package cluster

import (
	"bytes"
	"strings"
	"testing"
)

func TestGPUAgentEmbeddedBinary(t *testing.T) {
	binary, err := gpuAgentBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(binary, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatal("embedded GPU agent is not an ELF binary")
	}
	if len(binary) < 1024*1024 {
		t.Fatalf("embedded GPU agent is suspiciously small: %d bytes", len(binary))
	}
}

func TestGPUDRAManifestContract(t *testing.T) {
	for _, want := range []string{
		"kind: DeviceClass",
		"name: gpu.kiac.dev",
		"extendedResourceName: kiac.dev/gpu",
		"kind: DaemonSet",
		"kiac.dev/gpu.present: \"true\"",
		gpuAgentNodePath,
		"resources: [resourceslices]",
	} {
		if !strings.Contains(gpuDRAManifest, want) {
			t.Errorf("DRA manifest missing %q", want)
		}
	}
	for _, forbidden := range []string{"nvidia.com/gpu", "verbs: [*]", "resources: [*]", ":latest"} {
		if strings.Contains(gpuDRAManifest, forbidden) {
			t.Errorf("DRA manifest unexpectedly contains %q", forbidden)
		}
	}
}

func TestRequireDRAKubernetesVersion(t *testing.T) {
	for _, version := range []string{"v1.36.4-k3s1", "v1.37.0"} {
		if err := requireDRAKubernetesVersion(version); err != nil {
			t.Errorf("%s: %v", version, err)
		}
	}
	for _, version := range []string{"", "v1.35.8-k3s1", "not-a-version"} {
		if err := requireDRAKubernetesVersion(version); err == nil {
			t.Errorf("%s: expected rejection", version)
		}
	}
}

func TestValidateGPUClusterConfigRejectsUnsupportedDRABeforeCreate(t *testing.T) {
	err := validateGPUClusterConfig(Config{
		Name: "gpu", GPUWorkers: 1, GPUDriver: "dra", K8sVersion: "v1.35.8-k3s1", IPFamily: IPv4,
	})
	if err == nil || !strings.Contains(err.Error(), "requires Kubernetes 1.36 or newer") {
		t.Fatalf("validation error = %v, want DRA version rejection", err)
	}
}
