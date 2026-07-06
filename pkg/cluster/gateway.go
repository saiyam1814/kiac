package cluster

import "fmt"

// installGateway deploys built-in Gateway API support: the Gateway API
// CRDs plus Traefik v3 as the bundled implementation, from manifests
// embedded in the binary (same pattern as MetalLB in assets.go).
// Traefik runs as a single Deployment exposed via a type LoadBalancer
// Service, and a default GatewayClass + Gateway are created, so
// `--gateway` lets `kubectl apply` of an HTTPRoute work with zero
// configuration. Requires the LB pool (cfg.NoLB must be false to get an
// EXTERNAL-IP). Steps report through ui.Step like the rest of Create.
func (m *Manager) installGateway(cp string, cfg Config) error {
	return fmt.Errorf("gateway stack not implemented yet")
}
