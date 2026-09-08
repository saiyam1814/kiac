package cluster

import (
	"fmt"
	"sort"
	"strings"
)

type gpuValuesTemplate struct {
	Aliases []string
	YAML    string
}

var gpuValuesTemplates = map[string]gpuValuesTemplate{
	"vllm-production-stack": {
		Aliases: []string{"vllm", "vllm-production-stack"},
		YAML: `# Kiac scheduling overrides for vLLM Production Stack.
# Stock vLLM images require CUDA and cannot execute on Apple Venus. Replace
# repository/tag with a Vulkan-capable vLLM-compatible image before enabling
# the model. These values configure scheduling only.
servingEngineSpec:
  runtimeClassName: nvidia
  tolerations:
    - key: kiac.dev/gpu
      operator: Equal
      value: "true"
      effect: NoSchedule
  modelSpec:
    - name: replace-me
      enabled: false
      repository: replace-with-a-venus-compatible-image
      tag: replace-me
      runtimeClassName: nvidia
      requestGPU: 1
      requestGPUType: kiac.dev/gpu
      tolerations:
        - key: kiac.dev/gpu
          operator: Equal
          value: "true"
          effect: NoSchedule
`,
	},
	"kserve": {
		Aliases: []string{"kserve"},
		YAML: `# Kiac scheduling fragment for a KServe InferenceService.
# Merge these fields into an InferenceService that uses a Venus/Vulkan-capable
# predictor image. Stock CUDA/vLLM predictor images do not run on Apple Venus.
metadata:
  annotations:
    serving.kserve.io/gpu-resource-types: '["kiac.dev/gpu"]'
spec:
  predictor:
    runtimeClassName: nvidia
    tolerations:
      - key: kiac.dev/gpu
        operator: Equal
        value: "true"
        effect: NoSchedule
    containers:
      - name: kserve-container
        image: replace-with-a-venus-compatible-image
        resources:
          requests:
            kiac.dev/gpu: "1"
          limits:
            kiac.dev/gpu: "1"
`,
	},
	"ollama": {
		Aliases: []string{"ollama", "ollama-helm"},
		YAML: `# Kiac scheduling overrides for the otwld/ollama Helm chart.
# The stock Ollama image selects CUDA for type=nvidia and cannot execute on
# Apple Venus. Supply a tested Vulkan-capable Ollama image before installing;
# these values configure scheduling only.
runtimeClassName: nvidia
tolerations:
  - key: kiac.dev/gpu
    operator: Equal
    value: "true"
    effect: NoSchedule
ollama:
  gpu:
    enabled: true
    type: nvidia
    number: 1
    nvidiaResource: kiac.dev/gpu
image:
  repository: replace-with-a-venus-compatible-ollama-image
  tag: replace-me
`,
	},
}

// SupportedGPUValueCharts returns the canonical integration names accepted by
// GPUValues. The generated snippets deliberately separate Kubernetes
// scheduling compatibility from application-level CUDA compatibility.
func SupportedGPUValueCharts() []string {
	charts := make([]string, 0, len(gpuValuesTemplates))
	for name := range gpuValuesTemplates {
		charts = append(charts, name)
	}
	sort.Strings(charts)
	return charts
}

// GPUValues returns a scheduling override for a known inference integration.
func GPUValues(chart string) (string, error) {
	chart = strings.ToLower(strings.TrimSpace(chart))
	for _, spec := range gpuValuesTemplates {
		for _, alias := range spec.Aliases {
			if chart == alias {
				return spec.YAML, nil
			}
		}
	}
	return "", fmt.Errorf("unknown GPU values target %q (supported: %s)", chart, strings.Join(SupportedGPUValueCharts(), ", "))
}
