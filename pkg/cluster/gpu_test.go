package cluster

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGPUMockManifests(t *testing.T) {
	manifests := []struct {
		name     string
		manifest string
		contains []string
	}{
		{"namespace", gpuNamespaceManifest, []string{"kind: Namespace", "name: " + gpuNamespace}},
		{"runtimeclass", gpuRuntimeClassManifest, []string{"kind: RuntimeClass", "name: " + gpuRuntimeClass, "handler: runc"}},
		{"device-plugin", gpuDevicePluginManifest, []string{
			"kind: DaemonSet", "name: " + gpuDevicePlugin, "--domain=" + gpuResourceDomain,
			"path: /dev/null", "mountPath: " + gpuMockDevicePath,
		}},
	}
	if got := len(gpuMockManifests()); got != len(manifests) {
		t.Fatalf("gpuMockManifests() returned %d manifests, want %d", got, len(manifests))
	}
	for _, item := range manifests {
		if got := decodeAll(t, item.name, item.manifest); got != 1 {
			t.Errorf("%s parsed as %d documents, want 1", item.name, got)
		}
		for _, want := range item.contains {
			if !strings.Contains(item.manifest, want) {
				t.Errorf("%s: missing %q", item.name, want)
			}
		}
	}

	var devicePlugin map[string]any
	if err := yaml.Unmarshal([]byte(gpuDevicePluginManifest), &devicePlugin); err != nil {
		t.Fatal(err)
	}
	images := collectImages(devicePlugin)
	wantImage := "docker.io/squat/generic-device-plugin:0.2.0@sha256:66c8d5c270eb2b721f1064c549b9b7898152a6d2f0163380a5d37dc7636c20ff"
	if len(images) != 1 || images[0] != wantImage {
		t.Fatalf("device-plugin images = %v, want [%s]", images, wantImage)
	}
	if strings.Contains(gpuDevicePluginManifest, "privileged:") {
		t.Error("mock device plugin should not need a privileged container")
	}
}

func TestPositiveResourceQuantity(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"1", true}, {" 2 ", true}, {"0", false}, {"", false}, {"1m", false}, {"nope", false}} {
		if got := positiveResourceQuantity(tc.value); got != tc.want {
			t.Errorf("positiveResourceQuantity(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestEscapedJSONPathKey(t *testing.T) {
	if got, want := escapedJSONPathKey("kiac.dev/gpu.api"), `kiac\.dev/gpu\.api`; got != want {
		t.Errorf("escapedJSONPathKey = %q, want %q", got, want)
	}
}
