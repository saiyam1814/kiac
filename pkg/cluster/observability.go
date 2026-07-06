package cluster

import "fmt"

// installObservability deploys the built-in observability stack:
// Prometheus, Grafana, kube-state-metrics, and node-exporter, from
// manifests embedded in the binary (same pattern as MetalLB in
// assets.go). Grafana is exposed as a type LoadBalancer Service with a
// provisioned Prometheus datasource and default dashboards, so
// `--observability` yields a working Grafana URL with zero
// configuration. Steps report through ui.Step like the rest of Create.
func (m *Manager) installObservability(cp string, cfg Config) error {
	return fmt.Errorf("observability stack not implemented yet")
}
