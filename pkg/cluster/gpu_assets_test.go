package cluster

import (
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
