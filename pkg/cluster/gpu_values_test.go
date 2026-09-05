package cluster

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGPUValuesTemplates(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{name: "vllm", want: []string{"requestGPUType: kiac.dev/gpu", "enabled: false", "Stock vLLM images require CUDA"}},
		{name: "kserve", want: []string{"serving.kserve.io/gpu-resource-types", "kiac.dev/gpu: \"1\"", "Venus/Vulkan-capable"}},
		{name: "ollama", want: []string{"nvidiaResource: kiac.dev/gpu", "runtimeClassName: nvidia", "stock Ollama image"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := GPUValues(test.name)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := yaml.Unmarshal([]byte(raw), &value); err != nil {
				t.Fatalf("generated values are not YAML: %v", err)
			}
			for _, want := range test.want {
				if !strings.Contains(raw, want) {
					t.Errorf("generated values missing %q", want)
				}
			}
			if strings.Contains(raw, "nvidia.com/gpu") {
				t.Fatal("generated values must not request NVIDIA capacity")
			}
		})
	}
}

func TestGPUValuesRejectsUnknownTarget(t *testing.T) {
	if _, err := GPUValues("made-up"); err == nil || !strings.Contains(err.Error(), "vllm-production-stack") {
		t.Fatalf("unexpected error: %v", err)
	}
}
