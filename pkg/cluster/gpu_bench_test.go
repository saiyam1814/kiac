package cluster

import (
	"strings"
	"testing"
)

func TestParseLlamaBenchmark(t *testing.T) {
	raw := `[
  {"n_prompt":128,"n_gen":0,"avg_ts":321.5},
  {"n_prompt":0,"n_gen":64,"avg_ts":42.25}
]`
	result, err := parseLlamaBenchmark("venus", "Apple M1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.PromptTokens != 128 || result.PromptTokensPerSecond != 321.5 {
		t.Fatalf("prompt result = %#v", result)
	}
	if result.GenerateTokens != 64 || result.GenerateTokensPerSecond != 42.25 {
		t.Fatalf("generation result = %#v", result)
	}
}

func TestParseLlamaBenchmarkRequiresBothRows(t *testing.T) {
	if _, err := parseLlamaBenchmark("venus", "device", `[{"n_prompt":128,"avg_ts":1}]`); err == nil {
		t.Fatal("accepted output without generation result")
	}
}

func TestGPUBenchmarkPodContract(t *testing.T) {
	manifest := gpuBenchmarkPodManifest()
	if got := decodeAll(t, "gpu-benchmark", manifest); got != 1 {
		t.Fatalf("parsed %d YAML documents, want 1", got)
	}
	for _, want := range []string{
		"runtimeClassName: nvidia",
		"kiac.dev/gpu: \"1\"",
		"kiac.dev/gpu.present: \"true\"",
		"@sha256:",
		"GGML_VK_VISIBLE_DEVICES",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("benchmark manifest missing %q", want)
		}
	}
	if strings.Contains(manifest, "nvidia.com/gpu") || strings.Contains(manifest, ":latest") {
		t.Fatal("benchmark manifest contains a fake or unpinned GPU contract")
	}
}

func TestVenusDeviceLine(t *testing.T) {
	raw := "ggml_vulkan: 0 = Virtio-GPU Venus (Apple M1 Max) (venus) | uma: 1"
	if got := venusDeviceLine(raw); got != "Virtio-GPU Venus" {
		t.Fatalf("device = %q", got)
	}
}
