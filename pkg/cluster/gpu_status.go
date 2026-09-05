package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// GPUNodeStatus is the real host/guest and Kubernetes view of one GPU node.
type GPUNodeStatus struct {
	Name          string `json:"name"`
	VMState       string `json:"vmState"`
	MemoryMiB     int    `json:"memoryMiB"`
	RenderDevice  bool   `json:"renderDevice"`
	KubernetesAPI string `json:"api,omitempty"`
	DriverReady   bool   `json:"driverReady"`
	ResourceSlice bool   `json:"resourceSlice,omitempty"`
	Schedulable   bool   `json:"schedulable"`
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
		nodeStatus := GPUNodeStatus{Name: info.Name, VMState: info.Status, MemoryMiB: state.GPUMemoryMiB}
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
	inventoryReady := status.RenderDevice &&
		status.KubernetesAPI == "venus" &&
		node.Metadata.Labels[gpuResourceDomain+"/gpu.present"] == "true"
	if !inventoryReady || !status.DriverReady {
		return false
	}
	if driver == "dra" {
		return status.ResourceSlice
	}
	return driver == "device-plugin" && node.Status.Allocatable[gpuResourceName] == "1"
}

type gpuPodList struct {
	Items []struct {
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
		if item.Spec.Driver == "gpu.kiac.dev" && len(item.Spec.Devices) == 1 && item.Spec.Devices[0].Name == "venus-0" {
			ready[item.Spec.NodeName] = true
		}
	}
	return ready, nil
}

func (m *Manager) renderDeviceExists(node string) bool {
	_, err := m.rt.Exec(node, "test", "-c", "/dev/dri/renderD128")
	return err == nil
}
