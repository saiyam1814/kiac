package cluster

import _ "embed"

// The observability stack (--observability) is embedded one manifest per
// component. All image tags are pinned and multi-arch with linux/arm64;
// nothing here needs kernel features beyond the minimal Apple node
// kernel (/proc and /sys readers only, no eBPF, no modules).

//go:embed assets/observability/namespace.yaml
var obsNamespaceManifest string

//go:embed assets/observability/node-exporter.yaml
var obsNodeExporterManifest string

//go:embed assets/observability/kube-state-metrics.yaml
var obsKubeStateMetricsManifest string

//go:embed assets/observability/prometheus.yaml
var obsPrometheusManifest string

//go:embed assets/observability/grafana.yaml
var obsGrafanaManifest string

// observabilityManifests returns the stack in apply order: the namespace
// leads so every namespaced resource lands in the same kubectl apply.
func observabilityManifests() []string {
	return []string{
		obsNamespaceManifest,
		obsNodeExporterManifest,
		obsKubeStateMetricsManifest,
		obsPrometheusManifest,
		obsGrafanaManifest,
	}
}
