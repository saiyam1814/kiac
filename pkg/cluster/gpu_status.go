package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// GPUNodeStatus is the real host/guest and Kubernetes view of one GPU node.
type GPUNodeStatus struct {
	Name            string    `json:"name"`
	VMState         string    `json:"vmState"`
	MemoryMiB       int       `json:"memoryMiB"`
	RenderDevice    bool      `json:"renderDevice"`
	KubernetesAPI   string    `json:"api,omitempty"`
	DriverReady     bool      `json:"driverReady"`
	Health          string    `json:"health"`
	HealthScope     string    `json:"healthScope,omitempty"`
	HealthMessage   string    `json:"healthMessage,omitempty"`
	HealthCheckedAt time.Time `json:"healthCheckedAt,omitempty"`
	ResourceSlice   bool      `json:"resourceSlice,omitempty"`
	Schedulable     bool      `json:"schedulable"`
}

// GPUStatus is returned by `kiac gpu status`.
type GPUStatus struct {
	Cluster       string                 `json:"cluster"`
	Distro        string                 `json:"distro"`
	Resource      string                 `json:"resource"`
	Driver        string                 `json:"driver"`
	Nodes         []GPUNodeStatus        `json:"nodes"`
	Compatibility GPUCompatibilityStatus `json:"compatibility"`
}

// GPUPreflight verifies that the host has the real krunkit/Venus stack.
func (m *Manager) GPUPreflight() error {
	return m.krunkit.Preflight()
}

// GPUStatusForCluster reports the persisted GPU window and live Kubernetes
// registration without changing cluster state.
func (m *Manager) GPUStatusForCluster(name string) (GPUStatus, error) {
	cp, distro, err := m.gpuClusterContext(name)
	if err != nil {
		return GPUStatus{}, err
	}
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return GPUStatus{}, err
	}
	report := GPUStatus{Cluster: name, Distro: distro, Resource: gpuResourceName, Driver: "unknown"}

	draName, draErr := m.gpuKubectl(cp, distro, "get", "daemonset", "kiac-gpu-dra", "-n", "kube-system", "--ignore-not-found", "-o", "name")
	if draErr != nil {
		return GPUStatus{}, draErr
	}
	if strings.TrimSpace(draName) != "" {
		report.Driver = "dra"
	} else {
		pluginName, pluginErr := m.gpuKubectl(cp, distro, "get", "daemonset", "kiac-gpu-device-plugin", "-n", "kube-system", "--ignore-not-found", "-o", "name")
		if pluginErr != nil {
			return GPUStatus{}, pluginErr
		}
		if strings.TrimSpace(pluginName) != "" {
			report.Driver = "device-plugin"
		}
	}
	driverPods, err := m.gpuDriverReadyNodes(cp, distro, report.Driver)
	if err != nil {
		return GPUStatus{}, err
	}
	healthNodes, err := m.gpuDriverHealthNodes(cp, distro, report.Driver, 10*time.Second)
	if err != nil {
		return GPUStatus{}, err
	}
	draSlices := map[string]bool{}
	if report.Driver == "dra" {
		draSlices, err = m.gpuDRAReadyNodes(cp, distro)
		if err != nil {
			return GPUStatus{}, err
		}
	}

	nodeJSON, err := m.gpuKubectl(cp, distro, "get", "nodes", "-o", "json")
	if err != nil {
		return GPUStatus{}, err
	}
	var nodes kubeNodeList
	if err := json.Unmarshal([]byte(nodeJSON), &nodes); err != nil {
		return GPUStatus{}, fmt.Errorf("parse Kubernetes nodes: %w", err)
	}
	byName := make(map[string]kubeNode, len(nodes.Items))
	for _, node := range nodes.Items {
		byName[node.Metadata.Name] = node
	}
	for _, info := range infos {
		if !info.GPU && !isGPUNode(info.Name) {
			continue
		}
		state, err := m.krunkit.State(info.Name)
		if err != nil {
			return GPUStatus{}, err
		}
		nodeStatus := GPUNodeStatus{Name: info.Name, VMState: info.Status, MemoryMiB: state.GPUMemoryMiB, Health: "unknown"}
		if health, ok := healthNodes[info.Name]; ok {
			nodeStatus.Health = health.State
			nodeStatus.HealthScope = health.Scope
			nodeStatus.HealthMessage = health.Message
			nodeStatus.HealthCheckedAt = health.CheckedAt
		}
		if strings.EqualFold(info.Status, "running") {
			nodeStatus.RenderDevice = m.renderDeviceExists(info.Name)
		}
		if node, ok := byName[info.Name]; ok {
			nodeStatus.KubernetesAPI = node.Metadata.Labels[gpuResourceDomain+"/gpu.api"]
			nodeStatus.DriverReady = driverPods[info.Name]
			if report.Driver == "dra" {
				nodeStatus.ResourceSlice = draSlices[info.Name]
			}
			nodeStatus.Schedulable = gpuNodeSchedulable(report.Driver, nodeStatus, node)
			if node.Status.Capacity["nvidia.com/gpu"] != "" {
				return GPUStatus{}, fmt.Errorf("node %s incorrectly advertises nvidia.com/gpu", info.Name)
			}
		}
		report.Nodes = append(report.Nodes, nodeStatus)
	}
	if len(report.Nodes) == 0 {
		return GPUStatus{}, fmt.Errorf("cluster %q has no real Apple GPU nodes", name)
	}
	sort.Slice(report.Nodes, func(i, j int) bool { return report.Nodes[i].Name < report.Nodes[j].Name })
	report.Compatibility, err = m.GPUCompatibilityStatus(name)
	if err != nil {
		return GPUStatus{}, err
	}
	return report, nil
}

func gpuNodeSchedulable(driver string, status GPUNodeStatus, node kubeNode) bool {
	if node.Spec.Unschedulable || node.Metadata.DeletionTimestamp != "" || !conditionTrue(node.Status.Conditions, "Ready") {
		return false
	}
	inventoryReady := status.RenderDevice &&
		status.KubernetesAPI == "venus" &&
		node.Metadata.Labels[gpuResourceDomain+"/gpu.present"] == "true"
	if !inventoryReady || !status.DriverReady || status.Health != "healthy" {
		return false
	}
	if driver == "dra" {
		return status.ResourceSlice
	}
	return driver == "device-plugin" && node.Status.Allocatable[gpuResourceName] == "1"
}

type gpuPodList struct {
	Items []struct {
		Metadata struct {
			Name              string `json:"name"`
			CreationTimestamp string `json:"creationTimestamp"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func (m *Manager) gpuDriverReadyNodes(cp, distro, driver string) (map[string]bool, error) {
	app := "kiac-gpu-device-plugin"
	if driver == "dra" {
		app = "kiac-gpu-dra"
	}
	if driver == "unknown" {
		return map[string]bool{}, nil
	}
	out, err := m.gpuKubectl(cp, distro, "get", "pods", "-n", "kube-system", "-l", "app.kubernetes.io/name="+app, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list gpuPodList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse GPU driver pods: %w", err)
	}
	ready := map[string]bool{}
	for _, pod := range list.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready[pod.Spec.NodeName] = true
			}
		}
	}
	return ready, nil
}

func (m *Manager) gpuDRAReadyNodes(cp, distro string) (map[string]bool, error) {
	out, err := m.gpuKubectl(cp, distro, "get", "resourceslices.resource.k8s.io", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list resourceSliceList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse GPU ResourceSlices: %w", err)
	}
	ready := map[string]bool{}
	for _, item := range list.Items {
		if item.Spec.Driver == "gpu.kiac.dev" && len(item.Spec.Devices) == 1 && item.Spec.Devices[0].Name == "venus-0" && gpuSliceDeviceSchedulable(item.Spec.Devices[0].Taints) {
			ready[item.Spec.NodeName] = true
		}
	}
	return ready, nil
}

func (m *Manager) renderDeviceExists(node string) bool {
	_, err := m.rt.ExecTimeout(node, 10*time.Second, "test", "-c", "/dev/dri/renderD128")
	return err == nil
}

type gpuLiveHealth struct {
	State     string    `json:"state"`
	Scope     string    `json:"scope"`
	CheckedAt time.Time `json:"checkedAt"`
	Message   string    `json:"message"`
}

func parseGPULiveHealth(raw string, now time.Time) gpuLiveHealth {
	var health gpuLiveHealth
	if err := json.Unmarshal([]byte(raw), &health); err != nil {
		return gpuLiveHealth{State: "unknown", Message: "cannot parse live Vulkan health"}
	}
	if health.Scope != "vulkan-transfer" || (health.State != "healthy" && health.State != "unhealthy" && health.State != "unknown") {
		return gpuLiveHealth{State: "unknown", Message: "unsupported GPU health report"}
	}
	if health.State == "healthy" && (health.CheckedAt.IsZero() || now.Sub(health.CheckedAt) > 20*time.Second || health.CheckedAt.After(now.Add(time.Second))) {
		health.State = "unknown"
		health.Message = "Vulkan health report is stale"
	}
	return health
}
func (m *Manager) gpuDriverHealthNodes(cp, distro, driver string, timeout time.Duration) (map[string]gpuLiveHealth, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	result := map[string]gpuLiveHealth{}
	if driver == "unknown" {
		return result, nil
	}
	app, container := "kiac-gpu-device-plugin", "device-plugin"
	if driver == "dra" {
		app, container = "kiac-gpu-dra", "agent"
	}
	out, err := m.diagnosticKubectl(cp, distro, timeout, "get", "pods", "-n", "kube-system", "-l", "app.kubernetes.io/name="+app, "-o", "json")
	if err != nil {
		return nil, err
	}
	var pods gpuPodList
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return nil, fmt.Errorf("parse GPU health pods: %w", err)
	}
	// Prefer the newest non-terminating driver Pod during a rolling update.
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Metadata.CreationTimestamp < pods.Items[j].Metadata.CreationTimestamp
	})
	for _, pod := range pods.Items {
		if pod.Metadata.DeletionTimestamp != "" || pod.Metadata.Name == "" || pod.Spec.NodeName == "" {
			continue
		}
		raw, err := m.diagnosticKubectl(cp, distro, timeout, "exec", "-n", "kube-system", pod.Metadata.Name, "-c", container, "--", "/opt/kiac/kiac-gpu-agent", "--mode=health-status")
		if err != nil {
			result[pod.Spec.NodeName] = gpuLiveHealth{State: "unknown", Message: "cannot read live Vulkan health: " + compactError(err)}
			continue
		}
		result[pod.Spec.NodeName] = parseGPULiveHealth(raw, time.Now())
	}
	return result, nil
}
