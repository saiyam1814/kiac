package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type managedDeployment struct {
	namespace string
	name      string
}

// systemdManagedDeployments is deliberately an allow-list. Resume should
// guarantee that Kiac's own cluster services are usable without waiting for
// unrelated user Deployments that may intentionally be unavailable.
var systemdManagedDeployments = []managedDeployment{
	{namespace: "kube-system", name: "coredns"},
	{namespace: "kube-system", name: "metrics-server"},
	{namespace: "kube-system", name: "local-path-provisioner"},        // K3s
	{namespace: "local-path-storage", name: "local-path-provisioner"}, // kubeadm
	{namespace: obsNamespace, name: "kube-state-metrics"},
	{namespace: obsNamespace, name: "prometheus"},
	{namespace: obsNamespace, name: "grafana"},
	{namespace: "kiac-gateway", name: "traefik"},
}

type deploymentInventory struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	} `json:"items"`
}

// waitSystemdManagedAddons closes the lifecycle contract for persistent GPU
// VMs. Node Ready only says kubelet recovered; metrics-server, local-path,
// and optional Kiac addons may still be starting after a whole-cluster boot.
func (m *Manager) waitSystemdManagedAddons(cp, distro string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	queryTimeout := remainingAddonWait(deadline)
	out, err := m.diagnosticKubectl(cp, distro, queryTimeout, "get", "deployments", "-A", "-o", "json")
	if err != nil {
		return fmt.Errorf("discovering Kiac-managed deployments: %w", err)
	}
	var inventory deploymentInventory
	if err := json.Unmarshal([]byte(out), &inventory); err != nil {
		return fmt.Errorf("parsing Kubernetes deployment inventory: %w", err)
	}
	present := make(map[string]bool, len(inventory.Items))
	for _, deployment := range inventory.Items {
		present[deployment.Metadata.Namespace+"/"+deployment.Metadata.Name] = true
	}

	var waits []managedDeployment
	for _, deployment := range systemdManagedDeployments {
		if present[deployment.namespace+"/"+deployment.name] {
			waits = append(waits, deployment)
		}
	}

	queryTimeout = remainingAddonWait(deadline)
	apiService, err := m.diagnosticKubectl(cp, distro, queryTimeout,
		"get", "apiservice", "v1beta1.metrics.k8s.io", "--ignore-not-found", "-o", "name")
	if err != nil {
		return fmt.Errorf("discovering metrics APIService: %w", err)
	}
	waitMetricsAPI := strings.TrimSpace(apiService) != ""
	taskCount := len(waits)
	if waitMetricsAPI {
		taskCount++
	}
	return inParallel(taskCount, func(i int) error {
		remaining := remainingAddonWait(deadline)
		timeoutArg := kubectlTimeoutArg(remaining)
		if i < len(waits) {
			deployment := waits[i]
			resource := "deployment/" + deployment.name
			_, err := m.diagnosticKubectl(cp, distro, remaining,
				"wait", "--for=condition=Available", resource,
				"-n", deployment.namespace, timeoutArg)
			if err != nil {
				return fmt.Errorf("waiting for %s/%s: %w", deployment.namespace, resource, err)
			}
			return nil
		}
		_, err := m.diagnosticKubectl(cp, distro, remaining,
			"wait", "--for=condition=Available", "apiservice/v1beta1.metrics.k8s.io", timeoutArg)
		if err != nil {
			return fmt.Errorf("waiting for metrics APIService: %w", err)
		}
		return nil
	})
}

func remainingAddonWait(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining < time.Second {
		return time.Second
	}
	return remaining
}

func kubectlTimeoutArg(timeout time.Duration) string {
	seconds := int64((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("--timeout=%ds", seconds)
}
