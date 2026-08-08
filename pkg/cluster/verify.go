package cluster

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// VerificationStatus is the machine-readable outcome of one cluster check.
type VerificationStatus string

const (
	VerificationPass VerificationStatus = "pass"
	VerificationWarn VerificationStatus = "warn"
	VerificationFail VerificationStatus = "fail"
	VerificationSkip VerificationStatus = "skip"
)

// VerificationCheck is one stable, independently actionable health signal.
type VerificationCheck struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Status VerificationStatus `json:"status"`
	Detail string             `json:"detail,omitempty"`
	Hint   string             `json:"hint,omitempty"`
}

// VerificationReport is emitted by `kiac verify cluster -o json` and embedded
// in support bundles. SchemaVersion changes only for incompatible JSON changes.
type VerificationReport struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Cluster       string              `json:"cluster"`
	Distro        string              `json:"distro,omitempty"`
	Healthy       bool                `json:"healthy"`
	CheckedAt     string              `json:"checkedAt"`
	Duration      string              `json:"duration"`
	Checks        []VerificationCheck `json:"checks"`
}

// FailureCount returns the number of required checks that failed.
func (r VerificationReport) FailureCount() int {
	n := 0
	for _, check := range r.Checks {
		if check.Status == VerificationFail {
			n++
		}
	}
	return n
}

// Verify inspects a cluster without changing it. timeout bounds each guest
// command independently so one wedged node cannot hang the whole diagnostic.
func (m *Manager) Verify(name string, timeout time.Duration) (VerificationReport, error) {
	started := time.Now()
	report := VerificationReport{
		SchemaVersion: 1,
		Cluster:       name,
		CheckedAt:     started.UTC().Format(time.RFC3339),
		Checks:        []VerificationCheck{},
	}
	finish := func() {
		report.Duration = time.Since(started).Round(time.Millisecond).String()
		report.Healthy = report.FailureCount() == 0
	}
	if !ValidName(name) {
		finish()
		return report, fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", name)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	infos, err := m.rt.List(prefix(name))
	if err != nil {
		finish()
		return report, err
	}
	if len(infos) == 0 {
		finish()
		return report, fmt.Errorf("no cluster named %q found", name)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	report.Distro = distroFromNodes(infos)

	cpName := ControlPlane(name)
	var cpInfo *runtime.Info
	running := 0
	for i := range infos {
		if strings.EqualFold(infos[i].Status, "running") {
			running++
		}
		if infos[i].Name == cpName {
			cpInfo = &infos[i]
		}
	}
	if cpInfo == nil {
		report.add(VerificationFail, "runtime.layout", "cluster layout",
			fmt.Sprintf("control plane %s is missing", cpName),
			"restore the missing VM or recreate the cluster")
	} else {
		report.add(VerificationPass, "runtime.layout", "cluster layout",
			fmt.Sprintf("control plane and %d worker(s) found", len(infos)-1), "")
	}
	vmStatus := VerificationPass
	vmHint := ""
	if running != len(infos) {
		vmStatus = VerificationFail
		vmHint = fmt.Sprintf("run: kiac resume cluster --name %s", name)
	}
	report.add(vmStatus, "runtime.nodes", "node VMs",
		fmt.Sprintf("%d/%d running", running, len(infos)), vmHint)

	for _, info := range infos {
		id := "runtime.node." + strings.TrimPrefix(info.Name, prefix(name))
		label := "node services: " + info.Name
		if !strings.EqualFold(info.Status, "running") {
			report.add(VerificationSkip, id, label, "VM is "+orUnknown(info.Status), "")
			continue
		}
		var command []string
		if report.Distro == "k3s" {
			command = []string{"sh", "-ec", `test -r /proc/1/status && (test -x /bin/kubectl || test -x /usr/local/bin/kubectl)`}
		} else {
			command = []string{"systemctl", "is-active", "--quiet", "containerd", "kubelet"}
		}
		if _, err := m.rt.ExecTimeout(info.Name, timeout, command...); err != nil {
			report.add(VerificationFail, id, label, "guest responds, but required node services are not healthy",
				fmt.Sprintf("inspect: container exec %s %s", info.Name, nodeLogHint(report.Distro)))
		} else {
			report.add(VerificationPass, id, label, "guest and required node services are healthy", "")
		}
	}

	if cpInfo == nil || !strings.EqualFold(cpInfo.Status, "running") {
		report.skipKubernetesChecks("control-plane VM is not running")
		report.add(VerificationSkip, "host.api", "API reachability from Mac", "control-plane VM is not running", "")
		finish()
		return report, nil
	}

	apiReady := false
	if out, err := m.diagnosticKubectl(cpName, report.Distro, timeout, "get", "--raw=/readyz"); err != nil {
		report.add(VerificationFail, "kubernetes.api", "Kubernetes API", compactError(err),
			fmt.Sprintf("inspect from the VM: container exec %s kubectl get --raw=/readyz", cpName))
	} else if !strings.Contains(strings.ToLower(out), "ok") {
		report.add(VerificationFail, "kubernetes.api", "Kubernetes API", "readyz did not return ok: "+compact(out), "")
	} else {
		apiReady = true
		report.add(VerificationPass, "kubernetes.api", "Kubernetes API", "readyz returned ok", "")
	}

	if !apiReady {
		report.skipKubernetesDataChecks("API server is not ready")
	} else {
		m.verifyKubernetesData(&report, infos, cpName, timeout)
	}

	m.verifyNodeAddons(&report, infos, cpName, timeout)

	ip := cpInfo.IP
	// kubeadm's admin.conf retains the endpoint family chosen at create
	// time. This matters for IPv6-only clusters: the VM still has a vmnet
	// IPv4 address for egress, but the API server and host kubeconfig use
	// its IPv6 address. k3s writes loopback into its in-VM kubeconfig, so
	// that distro continues to use the runtime-reported node address.
	if report.Distro == "kubeadm" {
		if endpoint, err := m.diagnosticKubectl(cpName, report.Distro, timeout,
			"config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"); err == nil {
			if host := endpointHostname(endpoint); host != "" {
				ip = host
			}
		}
	}
	if ip == "" {
		ip, _ = m.rt.IP(cpName)
	}
	if ip == "" {
		report.add(VerificationFail, "host.api", "API reachability from Mac", "control-plane API address is unavailable", "")
	} else {
		endpoint := net.JoinHostPort(ip, "6443")
		hostTimeout := timeout
		if hostTimeout > 5*time.Second {
			hostTimeout = 5 * time.Second
		}
		if hostReachAPI(ip, hostTimeout) {
			report.add(VerificationPass, "host.api", "API reachability from Mac", endpoint+" accepts TCP connections", "")
		} else {
			hint := fmt.Sprintf("reset vmnet, then run: kiac resume cluster --name %s", name)
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
				hint = "inspect: container network inspect default"
			}
			report.add(VerificationFail, "host.api", "API reachability from Mac", endpoint+" is unreachable", hint)
		}
	}

	finish()
	return report, nil
}

func endpointHostname(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func (r *VerificationReport) add(status VerificationStatus, id, name, detail, hint string) {
	r.Checks = append(r.Checks, VerificationCheck{ID: id, Name: name, Status: status, Detail: detail, Hint: hint})
}

func (r *VerificationReport) skipKubernetesChecks(reason string) {
	r.add(VerificationSkip, "kubernetes.api", "Kubernetes API", reason, "")
	r.skipKubernetesDataChecks(reason)
	r.add(VerificationSkip, "network.edge-proxy", "edge proxy", reason, "")
	r.add(VerificationSkip, "network.load-balancer", "LoadBalancer controller", reason, "")
}

func (r *VerificationReport) skipKubernetesDataChecks(reason string) {
	for _, check := range []struct{ id, name string }{
		{"kubernetes.nodes", "Kubernetes nodes"},
		{"kubernetes.pods", "Kubernetes workloads"},
		{"kubernetes.dns", "cluster DNS"},
		{"storage.default-class", "default storage"},
		{"metrics.api", "metrics API"},
		{"gateway.api", "Gateway API"},
		{"observability.stack", "observability"},
	} {
		r.add(VerificationSkip, check.id, check.name, reason, "")
	}
}

func (m *Manager) verifyKubernetesData(report *VerificationReport, infos []runtime.Info, cp string, timeout time.Duration) {
	out, err := m.diagnosticKubectl(cp, report.Distro, timeout, "get", "nodes", "-o", "json")
	if err != nil {
		report.add(VerificationFail, "kubernetes.nodes", "Kubernetes nodes", compactError(err), "kubectl get nodes")
	} else {
		var list kubeNodeList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			report.add(VerificationFail, "kubernetes.nodes", "Kubernetes nodes", "cannot parse kubectl output: "+err.Error(), "")
		} else {
			registered := map[string]bool{}
			var unready []string
			for _, node := range list.Items {
				registered[node.Metadata.Name] = true
				if !conditionTrue(node.Status.Conditions, "Ready") {
					unready = append(unready, node.Metadata.Name)
				}
			}
			var missing []string
			for _, info := range infos {
				if !registered[info.Name] {
					missing = append(missing, info.Name)
				}
			}
			if len(missing)+len(unready) > 0 {
				detail := joinProblems(problem("missing", missing), problem("not Ready", unready))
				report.add(VerificationFail, "kubernetes.nodes", "Kubernetes nodes", detail, "kubectl get nodes -o wide")
			} else {
				report.add(VerificationPass, "kubernetes.nodes", "Kubernetes nodes",
					fmt.Sprintf("%d/%d registered nodes are Ready", len(list.Items), len(list.Items)), "")
			}
		}
	}

	out, err = m.diagnosticKubectl(cp, report.Distro, timeout, "get", "pods", "-A", "-o", "json")
	if err != nil {
		report.add(VerificationFail, "kubernetes.pods", "Kubernetes workloads", compactError(err), "kubectl get pods -A")
	} else {
		var list kubePodList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			report.add(VerificationFail, "kubernetes.pods", "Kubernetes workloads", "cannot parse kubectl output: "+err.Error(), "")
		} else {
			var unhealthy []string
			for _, pod := range list.Items {
				if pod.Metadata.DeletionTimestamp != "" || pod.Status.Phase == "Succeeded" {
					continue
				}
				ready := pod.Status.Phase == "Running" && len(pod.Status.ContainerStatuses) > 0
				for _, container := range pod.Status.ContainerStatuses {
					ready = ready && container.Ready
				}
				if !ready {
					unhealthy = append(unhealthy, pod.Metadata.Namespace+"/"+pod.Metadata.Name+" ("+orUnknown(pod.Status.Phase)+")")
				}
			}
			if len(unhealthy) > 0 {
				report.add(VerificationFail, "kubernetes.pods", "Kubernetes workloads",
					fmt.Sprintf("%d unhealthy: %s", len(unhealthy), limitedList(unhealthy, 6)), "kubectl get pods -A -o wide")
			} else {
				report.add(VerificationPass, "kubernetes.pods", "Kubernetes workloads",
					fmt.Sprintf("%d pod(s) Running/Ready or Succeeded", len(list.Items)), "")
			}
		}
	}

	out, err = m.diagnosticKubectl(cp, report.Distro, timeout, "get", "endpointslice", "-n", "kube-system", "-l", "k8s-app=kube-dns", "-o", "json")
	if err != nil {
		report.add(VerificationFail, "kubernetes.dns", "cluster DNS", compactError(err), "kubectl -n kube-system get pods,svc,endpointslice -l k8s-app=kube-dns")
	} else {
		var list kubeEndpointSliceList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			report.add(VerificationFail, "kubernetes.dns", "cluster DNS", "cannot parse kubectl output: "+err.Error(), "")
		} else {
			ready := 0
			for _, slice := range list.Items {
				for _, endpoint := range slice.Endpoints {
					if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
						ready++
					}
				}
			}
			if ready == 0 {
				report.add(VerificationFail, "kubernetes.dns", "cluster DNS", "kube-dns has no ready endpoints", "kubectl -n kube-system get endpointslice -l k8s-app=kube-dns")
			} else {
				report.add(VerificationPass, "kubernetes.dns", "cluster DNS", fmt.Sprintf("%d ready endpoint(s)", ready), "")
			}
		}
	}

	m.verifyOptionalKubernetesAddons(report, cp, timeout)
}

func (m *Manager) verifyOptionalKubernetesAddons(report *VerificationReport, cp string, timeout time.Duration) {
	out, err := m.diagnosticKubectl(cp, report.Distro, timeout, "get", "storageclass", "-o", "json")
	if err != nil {
		report.add(VerificationFail, "storage.default-class", "default storage", compactError(err), "kubectl get storageclass")
	} else {
		var list kubeStorageClassList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			report.add(VerificationFail, "storage.default-class", "default storage", "cannot parse kubectl output: "+err.Error(), "")
		} else if len(list.Items) == 0 {
			report.add(VerificationSkip, "storage.default-class", "default storage", "no StorageClass installed (--no-storage cluster)", "")
		} else {
			var defaults []string
			for _, item := range list.Items {
				if item.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
					item.Metadata.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
					defaults = append(defaults, item.Metadata.Name)
				}
			}
			if len(defaults) == 0 {
				report.add(VerificationWarn, "storage.default-class", "default storage", "StorageClass exists, but none is default", "kubectl get storageclass")
			} else {
				report.add(VerificationPass, "storage.default-class", "default storage", "default StorageClass: "+strings.Join(defaults, ", "), "")
			}
		}
	}

	out, err = m.diagnosticKubectl(cp, report.Distro, timeout, "get", "apiservice", "v1beta1.metrics.k8s.io", "--ignore-not-found", "-o", "json")
	if err != nil {
		report.add(VerificationWarn, "metrics.api", "metrics API", compactError(err), "kubectl get apiservice v1beta1.metrics.k8s.io")
	} else if strings.TrimSpace(out) == "" {
		report.add(VerificationSkip, "metrics.api", "metrics API", "not installed (--no-metrics cluster)", "")
	} else {
		var service kubeAPIService
		if err := json.Unmarshal([]byte(out), &service); err != nil {
			report.add(VerificationFail, "metrics.api", "metrics API", "cannot parse kubectl output: "+err.Error(), "")
		} else if conditionTrue(service.Status.Conditions, "Available") {
			report.add(VerificationPass, "metrics.api", "metrics API", "APIService is Available", "")
		} else {
			report.add(VerificationFail, "metrics.api", "metrics API", "APIService is not Available", "kubectl describe apiservice v1beta1.metrics.k8s.io")
		}
	}

	out, err = m.diagnosticKubectl(cp, report.Distro, timeout, "get", "crd", "gateways.gateway.networking.k8s.io", "--ignore-not-found", "-o", "name")
	if err != nil {
		report.add(VerificationWarn, "gateway.api", "Gateway API", compactError(err), "kubectl get crd gateways.gateway.networking.k8s.io")
	} else if strings.TrimSpace(out) == "" {
		report.add(VerificationSkip, "gateway.api", "Gateway API", "built-in Gateway is not installed", "")
	} else {
		out, err = m.diagnosticKubectl(cp, report.Distro, timeout, "get", "gateway", "kiac", "-n", "kiac-gateway", "--ignore-not-found", "-o", "json")
		var gateway kubeGateway
		if err != nil {
			report.add(VerificationFail, "gateway.api", "Gateway API", compactError(err), "kubectl get gateway -A")
		} else if strings.TrimSpace(out) == "" {
			report.add(VerificationFail, "gateway.api", "Gateway API", "Gateway API CRDs exist, but built-in Gateway kiac-gateway/kiac is missing", "kubectl get gateway -A")
		} else if err := json.Unmarshal([]byte(out), &gateway); err != nil {
			report.add(VerificationFail, "gateway.api", "Gateway API", "cannot parse kubectl output: "+err.Error(), "")
		} else if len(gateway.Status.Addresses) == 0 || conditionFalse(gateway.Status.Conditions, "Programmed") {
			report.add(VerificationFail, "gateway.api", "Gateway API", "Gateway kiac-gateway/kiac has no programmed address", "kubectl describe gateway -n kiac-gateway kiac")
		} else {
			report.add(VerificationPass, "gateway.api", "Gateway API", "Gateway address: "+gateway.Status.Addresses[0].Value, "")
		}
	}

	out, err = m.diagnosticKubectl(cp, report.Distro, timeout, "get", "namespace", obsNamespace, "--ignore-not-found", "-o", "name")
	if err != nil {
		report.add(VerificationWarn, "observability.stack", "observability", compactError(err), "kubectl get pods -n "+obsNamespace)
	} else if strings.TrimSpace(out) == "" {
		report.add(VerificationSkip, "observability.stack", "observability", "built-in observability is not installed", "")
	} else {
		address, err := m.diagnosticKubectl(cp, report.Distro, timeout, "get", "service", "grafana", "-n", obsNamespace, "-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
		if err != nil || strings.TrimSpace(address) == "" {
			report.add(VerificationFail, "observability.stack", "observability", "Grafana has no LoadBalancer address", "kubectl get pods,svc -n "+obsNamespace)
		} else {
			report.add(VerificationPass, "observability.stack", "observability", "Grafana: http://"+strings.TrimSpace(address)+":3000", "")
		}
	}
}

func (m *Manager) verifyNodeAddons(report *VerificationReport, infos []runtime.Info, cp string, timeout time.Duration) {
	var installed, missing, unhealthy []string
	for _, info := range infos {
		if !strings.EqualFold(info.Status, "running") {
			continue
		}
		if _, err := m.rt.ExecTimeout(info.Name, timeout, "test", "-x", edgeProxyNodePath); err != nil {
			missing = append(missing, info.Name)
			continue
		}
		installed = append(installed, info.Name)
		active := `PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux:$PATH"; IPT="$(command -v iptables-legacy || command -v iptables)"; `
		if report.Distro == "k3s" {
			active += `found=; for p in /proc/[0-9]*; do [ "$(readlink "$p/exe" 2>/dev/null)" = "` + edgeProxyNodePath + `" ] && found=1 && break; done; [ -n "$found" ]; `
		} else {
			active += `systemctl is-active --quiet kiac-edge-proxy.service; `
		}
		active += `"$IPT" -w -t nat -C PREROUTING -j KIAC-EDGE && "$IPT" -w -t nat -C OUTPUT -j KIAC-EDGE-OUTPUT`
		if _, err := m.rt.ExecTimeout(info.Name, timeout, "sh", "-ec", active); err != nil {
			unhealthy = append(unhealthy, info.Name)
		}
	}
	if len(installed) == 0 {
		report.add(VerificationSkip, "network.edge-proxy", "edge proxy", "not installed (--no-edge-proxy cluster)", "")
	} else if len(missing) > 0 || len(unhealthy) > 0 {
		report.add(VerificationFail, "network.edge-proxy", "edge proxy",
			joinProblems(problem("missing", missing), problem("process/rules unhealthy", unhealthy)),
			"inspect /var/log/kiac-edge-proxy.log on the affected node")
	} else {
		report.add(VerificationPass, "network.edge-proxy", "edge proxy",
			fmt.Sprintf("process and redirect rules healthy on %d node(s)", len(installed)), "")
	}

	if _, err := m.rt.ExecTimeout(cp, timeout, "test", "-x", kiacLBScriptPath); err != nil {
		report.add(VerificationSkip, "network.load-balancer", "LoadBalancer controller", "not installed (--no-lb cluster)", "")
		return
	}
	var err error
	if report.Distro == "k3s" {
		_, err = m.rt.ExecTimeout(cp, timeout, "sh", "-ec", `pid="$(cat `+kiacLBK3sSupervisorPID+` 2>/dev/null)"; [ -n "$pid" ] && kill -0 "$pid"`)
	} else {
		_, err = m.rt.ExecTimeout(cp, timeout, "systemctl", "is-active", "--quiet", "kiac-lb.service")
	}
	if err != nil {
		report.add(VerificationFail, "network.load-balancer", "LoadBalancer controller", "controller process is not healthy", nodeLBLogHint(report.Distro, cp))
		return
	}
	out, svcErr := m.diagnosticKubectl(cp, report.Distro, timeout, "get", "services", "-A", "-o", "json")
	if svcErr != nil {
		report.add(VerificationFail, "network.load-balancer", "LoadBalancer controller", compactError(svcErr), "kubectl get services -A")
		return
	}
	var services kubeServiceList
	if err := json.Unmarshal([]byte(out), &services); err != nil {
		report.add(VerificationFail, "network.load-balancer", "LoadBalancer controller", "cannot parse kubectl output: "+err.Error(), "")
		return
	}
	var pending []string
	count := 0
	for _, svc := range services.Items {
		if svc.Spec.Type != "LoadBalancer" {
			continue
		}
		count++
		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			pending = append(pending, svc.Metadata.Namespace+"/"+svc.Metadata.Name)
		}
	}
	if len(pending) > 0 {
		report.add(VerificationFail, "network.load-balancer", "LoadBalancer controller", "pending Services: "+limitedList(pending, 6), nodeLBLogHint(report.Distro, cp))
	} else {
		report.add(VerificationPass, "network.load-balancer", "LoadBalancer controller", fmt.Sprintf("controller healthy; %d LoadBalancer Service(s) assigned", count), "")
	}
}

func (m *Manager) diagnosticKubectl(cp, distro string, timeout time.Duration, args ...string) (string, error) {
	base := []string{"kubectl", "--kubeconfig", adminConf}
	if distro == "k3s" {
		base = []string{"kubectl", "--kubeconfig", k3sKubeconfig}
	}
	return m.rt.ExecTimeout(cp, timeout, append(base, args...)...)
}

func nodeLogHint(distro string) string {
	if distro == "k3s" {
		return "cat /proc/1/status"
	}
	return "journalctl -u containerd -u kubelet --no-pager -n 100"
}

func nodeLBLogHint(distro, cp string) string {
	if distro == "k3s" {
		return fmt.Sprintf("inspect: container exec %s tail -100 %s", cp, kiacLBK3sLogPath)
	}
	return fmt.Sprintf("inspect: container exec %s journalctl -u kiac-lb --no-pager -n 100", cp)
}

func conditionTrue(conditions []kubeCondition, kind string) bool {
	for _, condition := range conditions {
		if condition.Type == kind {
			return strings.EqualFold(condition.Status, "true")
		}
	}
	return false
}

func conditionFalse(conditions []kubeCondition, kind string) bool {
	for _, condition := range conditions {
		if condition.Type == kind {
			return strings.EqualFold(condition.Status, "false")
		}
	}
	return false
}

func compactError(err error) string {
	if err == nil {
		return ""
	}
	return compact(err.Error())
}

func compact(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) > 300 {
		return s[:297] + "..."
	}
	return s
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func problem(label string, values []string) string {
	if len(values) == 0 {
		return ""
	}
	return label + ": " + limitedList(values, 6)
}

func joinProblems(values ...string) string {
	var nonempty []string
	for _, value := range values {
		if value != "" {
			nonempty = append(nonempty, value)
		}
	}
	return strings.Join(nonempty, "; ")
}

func limitedList(values []string, limit int) string {
	if len(values) <= limit {
		return strings.Join(values, ", ")
	}
	return strings.Join(values[:limit], ", ") + fmt.Sprintf(" (+%d more)", len(values)-limit)
}

type kubeMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	DeletionTimestamp string            `json:"deletionTimestamp"`
	Annotations       map[string]string `json:"annotations"`
}

type kubeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type kubeNodeList struct {
	Items []struct {
		Metadata kubeMetadata `json:"metadata"`
		Status   struct {
			Conditions []kubeCondition `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

type kubePodList struct {
	Items []struct {
		Metadata kubeMetadata `json:"metadata"`
		Status   struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type kubeEndpointSliceList struct {
	Items []struct {
		Endpoints []struct {
			Conditions struct {
				Ready *bool `json:"ready"`
			} `json:"conditions"`
		} `json:"endpoints"`
	} `json:"items"`
}

type kubeStorageClassList struct {
	Items []struct {
		Metadata kubeMetadata `json:"metadata"`
	} `json:"items"`
}

type kubeAPIService struct {
	Status struct {
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type kubeGateway struct {
	Status struct {
		Addresses []struct {
			Value string `json:"value"`
		} `json:"addresses"`
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type kubeServiceList struct {
	Items []struct {
		Metadata kubeMetadata `json:"metadata"`
		Spec     struct {
			Type string `json:"type"`
		} `json:"spec"`
		Status struct {
			LoadBalancer struct {
				Ingress []map[string]any `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	} `json:"items"`
}
