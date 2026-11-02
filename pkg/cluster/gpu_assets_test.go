package cluster

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"io"
	apps "k8s.io/api/apps/v1"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var digestPinnedImage = regexp.MustCompile(`^[a-z0-9./_-]+(?::[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}$`)

func TestGPUEmbeddedManifests(t *testing.T) {
	tls, err := newGPUCompatTLS(time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	tls.ControlPlane = "kiac-test-control-plane"
	compatCore, err := renderGPUCompatManifest("core", gpuCompatCoreManifest, tls)
	if err != nil {
		t.Fatal(err)
	}
	compatWebhook, err := renderGPUCompatManifest("webhook", gpuCompatWebhookManifest, tls)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		manifest string
		minDocs  int
	}{
		{name: "device-plugin", manifest: gpuDevicePluginManifest, minDocs: 1},
		{name: "runtime-class", manifest: gpuRuntimeClassManifest, minDocs: 1},
		{name: "dra", manifest: gpuDRAManifest, minDocs: 4},
		{name: "compat-core", manifest: compatCore, minDocs: 3},
		{name: "compat-webhook", manifest: compatWebhook, minDocs: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := decodeAll(t, test.name, test.manifest); got < test.minDocs {
				t.Fatalf("parsed %d documents, want at least %d", got, test.minDocs)
			}
			if strings.Contains(test.manifest, ":latest") {
				t.Fatal("manifest contains a moving latest tag")
			}
			for _, image := range collectManifestImages(t, test.name, test.manifest) {
				if !digestPinnedImage.MatchString(image) {
					t.Errorf("image %q is not pinned by sha256 digest", image)
				}
			}
		})
	}
}

func TestGPUExampleManifests(t *testing.T) {
	for _, name := range []string{
		"gpu-cluster.yaml",
		"gpu-dra-memory.yaml",
		"gpu-inference.yaml",
		"gpu-vulkan.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeAll(t, name, string(raw)); got == 0 {
				t.Fatal("manifest contains no YAML documents")
			}
			for _, image := range collectManifestImages(t, name, string(raw)) {
				if !digestPinnedImage.MatchString(image) {
					t.Errorf("image %q is not pinned by sha256 digest", image)
				}
			}
		})
	}
}

func collectManifestImages(t *testing.T, name, manifest string) []string {
	t.Helper()
	images := regexp.MustCompile(`(?m)^\s*image:\s*["']?([^"'[:space:]]+)`).FindAllStringSubmatch(manifest, -1)
	result := make([]string, 0, len(images))
	for _, match := range images {
		result = append(result, match[1])
	}
	return result
}

func TestGPUHealthContainersAreIsolatedAndFailClosed(t *testing.T) {
	for name, manifest := range map[string]string{"dra": gpuDRAManifest, "device-plugin": gpuDevicePluginManifest} {
		t.Run(name, func(t *testing.T) {
			decoder := yaml.NewDecoder(strings.NewReader(manifest))
			var daemon apps.DaemonSet
			for {
				var decoded map[string]any
				if err := decoder.Decode(&decoded); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
				var kind struct {
					Kind string `json:"kind"`
				}
				if err := json.Unmarshal(raw, &kind); err != nil {
					t.Fatal(err)
				}
				if kind.Kind == "DaemonSet" {
					if err := json.Unmarshal(raw, &daemon); err != nil {
						t.Fatal(err)
					}
				}
			}
			pod := daemon.Spec.Template.Spec
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Fatal("automatic API token must be disabled")
			}
			if len(pod.Containers) != 2 {
				t.Fatalf("containers=%d", len(pod.Containers))
			}
			main, probe := pod.Containers[0], pod.Containers[1]
			if probe.Name != "health-probe" || !strings.Contains(probe.Image, "fedora-vgpu-llama@sha256:") {
				t.Fatal("missing pinned Vulkan probe container")
			}
			if main.ReadinessProbe == nil || main.ReadinessProbe.Exec == nil || !strings.Contains(strings.Join(main.ReadinessProbe.Exec.Command, " "), "--mode=health-check") {
				t.Fatal("readiness does not require fresh Vulkan health")
			}
			if main.LivenessProbe != nil || probe.LivenessProbe != nil {
				t.Fatal("GPU failure must not restart away the health monitor")
			}
			mainHealth, probeHealth, credentials, nativeProbe := false, false, false, false
			for _, mount := range main.VolumeMounts {
				if mount.Name == "native-probe" {
					t.Fatal("static main agent must not mount native probe binary")
				}
				if mount.Name == "health" {
					mainHealth = mount.ReadOnly
				}
				if mount.Name == "api-credentials" {
					credentials = mount.ReadOnly
				}
			}
			for _, mount := range probe.VolumeMounts {
				if mount.Name == "native-probe" {
					nativeProbe = mount.ReadOnly
				}
				if mount.Name == "health" {
					probeHealth = !mount.ReadOnly
				}
				if mount.Name == "api-credentials" || mount.Name == "plugins" || mount.Name == "plugins-registry" || mount.Name == "device-plugins" || mount.Name == "cdi" {
					t.Fatalf("probe has unrelated privileged mount %s", mount.Name)
				}
			}
			if !nativeProbe {
				t.Fatal("Fedora sidecar must mount native probe read-only")
			}
			if !mainHealth || !probeHealth {
				t.Fatal("health volume permissions are incorrect")
			}
			if (name == "dra") != credentials {
				t.Fatal("API credentials not restricted to DRA main agent")
			}
			for _, volume := range pod.Volumes {
				if volume.Name == "health" && volume.EmptyDir == nil {
					t.Fatal("health report must be Pod-local")
				}
			}
		})
	}
}
