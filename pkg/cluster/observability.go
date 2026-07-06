package cluster

import (
	"fmt"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

const obsNamespace = "kiac-observability"

// installObservability deploys the built-in observability stack:
// Prometheus, Grafana, kube-state-metrics, and node-exporter, from
// manifests embedded in the binary (same pattern as metrics-server in
// assets.go). Grafana is exposed as a type LoadBalancer Service with a
// provisioned Prometheus datasource and default dashboards, so
// `--observability` yields a working Grafana URL with zero
// configuration. Runs after the LB primary node is labeled so kiac-lb
// can give the Service an EXTERNAL-IP on that node immediately; errors
// degrade the cluster (caller logs and continues) rather than tearing
// it down.
func (m *Manager) installObservability(cp string, cfg Config) error {
	if err := ui.Step("Installing observability (Prometheus + Grafana)", func() error {
		manifest := strings.Join(observabilityManifests(), "\n---\n")
		return m.rt.ExecStdin(cp, strings.NewReader(manifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
	}); err != nil {
		return err
	}

	var grafanaIP string
	if err := ui.Step("Waiting for Grafana LoadBalancer IP", func() error {
		deadline := time.Now().Add(90 * time.Second)
		for {
			out, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
				"get", "svc", "-n", obsNamespace, "grafana",
				"-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
			if err == nil {
				if ip := strings.TrimSpace(out); ip != "" {
					grafanaIP = ip
					return nil
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("grafana Service never got a LoadBalancer IP (is kiac-lb running? journalctl -u kiac-lb on the control plane); check: kubectl get svc -n %s grafana", obsNamespace)
			}
			time.Sleep(3 * time.Second)
		}
	}); err != nil {
		return err
	}

	ui.Infof("Grafana: http://%s:3000 (anonymous admin, local-only)", grafanaIP)
	ui.Hintf("open http://%s:3000        # dashboards: Cluster Overview, Nodes", grafanaIP)
	return nil
}
