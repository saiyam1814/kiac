package cluster

import (
	"fmt"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

// installGateway deploys built-in Gateway API support: the Gateway API
// CRDs plus Traefik v3 as the bundled implementation, from manifests
// embedded in the binary (same pattern as metrics-server in assets.go).
// Traefik runs as a single Deployment exposed via a type LoadBalancer
// Service, and a default GatewayClass + Gateway are created, so
// `--gateway` lets `kubectl apply` of an HTTPRoute work with zero
// configuration. Requires kiac-lb (cfg.NoLB must be false to get an
// EXTERNAL-IP). Steps report through ui.Step like the rest of Create.
func (m *Manager) installGateway(cp string, cfg Config) error {
	if cfg.NoLB {
		return fmt.Errorf("--gateway needs the built-in LoadBalancer; drop --no-lb")
	}

	if err := ui.Step("Installing Gateway API CRDs", func() error {
		// Server-side apply is required: the HTTPRoute CRD alone is far
		// past the 256KB last-applied-configuration annotation cap that
		// client-side apply would need. force-conflicts keeps a re-run
		// (or an upgrade over CRDs another tool installed) idempotent.
		if err := m.rt.ExecStdin(cp, strings.NewReader(gatewayCRDsManifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
			return err
		}
		// The default Gateway below is rejected until the apiserver has
		// established the new types, so block on the one kiac uses.
		_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
			"wait", "--for", "condition=Established",
			"crd/httproutes.gateway.networking.k8s.io", "--timeout=60s")
		return err
	}); err != nil {
		return err
	}

	if err := ui.Step("Installing Traefik (GatewayClass traefik)", func() error {
		if err := m.rt.ExecStdin(cp, strings.NewReader(gatewayTraefikManifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-"); err != nil {
			return err
		}
		return m.rt.ExecStdin(cp, strings.NewReader(gatewayDefaultManifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
	}); err != nil {
		return err
	}

	var addr string
	if err := ui.Step("Waiting for Gateway address", func() error {
		var err error
		addr, err = m.waitGatewayAddress(cp, 90*time.Second)
		return err
	}); err != nil {
		return err
	}

	ui.Infof("Gateway API ready: http://%s (GatewayClass traefik, Gateway kiac-gateway/kiac)", addr)
	ui.Hintf(`attach routes with: spec.parentRefs: [{name: kiac, namespace: kiac-gateway}] in any HTTPRoute`)
	return nil
}

// waitGatewayAddress polls the Traefik Service for its LoadBalancer
// ingress. The Service IP rather than Gateway status is polled: kiac-lb
// assigns it first, and Traefik copies it into the Gateway afterwards,
// so the Service is the earliest reliable signal that traffic can flow.
func (m *Manager) waitGatewayAddress(cp string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		out, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
			"get", "svc", "traefik", "-n", "kiac-gateway",
			"-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
		if err == nil {
			if ip := strings.TrimSpace(out); ip != "" {
				return ip, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("gateway got no address within %s; check: kubectl get svc -n kiac-gateway traefik", timeout)
		}
		time.Sleep(3 * time.Second)
	}
}
