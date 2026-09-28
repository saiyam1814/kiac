package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const kiacGPUDriver = "gpu.kiac.dev"

// GPUInspection is a read-only snapshot. Capacity describes scheduler accounting,
// never live physical memory usage or an enforced per-workload memory limit.
type GPUInspection struct {
	Status     GPUStatus             `json:"status"`
	CapturedAt time.Time             `json:"capturedAt"`
	Devices    []GPUDeviceAccounting `json:"devices"`
	Claims     []GPUClaimStatus      `json:"claims"`
	Workloads  []GPUWorkloadStatus   `json:"workloads"`
	Notes      []string              `json:"notes"`
}

type GPUDeviceAccounting struct {
	Node       string  `json:"node"`
	Pool       string  `json:"pool"`
	Device     string  `json:"device"`
	Capacity   string  `json:"capacity"`
	Reserved   *string `json:"reserved,omitempty"`
	Unreserved *string `json:"unreserved,omitempty"`
}

type GPUClaimStatus struct {
	Namespace   string               `json:"namespace"`
	Name        string               `json:"name"`
	State       string               `json:"state"`
	Deleting    bool                 `json:"deleting,omitempty"`
	Allocations []GPUClaimAllocation `json:"allocations"`
	Consumers   []string             `json:"consumers"`
}

type GPUClaimAllocation struct {
	Request     string `json:"request"`
	Pool        string `json:"pool"`
	Device      string `json:"device"`
	ShareID     string `json:"shareID,omitempty"`
	Memory      string `json:"memory,omitempty"`
	AdminAccess bool   `json:"adminAccess,omitempty"`
}

type GPUWorkloadStatus struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Node      string   `json:"node,omitempty"`
	Phase     string   `json:"phase"`
	Claims    []string `json:"claims"`
	Reason    string   `json:"reason,omitempty"`
	Message   string   `json:"message,omitempty"`
}

// InspectGPU joins driver inventory, claim allocations and workload scheduling
// conditions without creating Pods or changing the active kubeconfig context.
func (m *Manager) InspectGPU(name string) (GPUInspection, error) {
	status, err := m.GPUStatusForCluster(name)
	if err != nil {
		return GPUInspection{}, err
	}
	cp := ControlPlane(name)
	read := func(target any, args ...string) error {
		out, err := m.gpuKubectl(cp, status.Distro, append(args, "-o", "json")...)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(out), target); err != nil {
			return fmt.Errorf("parse GPU inspection %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}
	var slices resourcev1.ResourceSliceList
	var claims resourcev1.ResourceClaimList
	if status.Driver == "dra" {
		if err := read(&slices, "get", "resourceslices.resource.k8s.io"); err != nil {
			return GPUInspection{}, err
		}
		if err := read(&claims, "get", "resourceclaims.resource.k8s.io", "--all-namespaces"); err != nil {
			return GPUInspection{}, err
		}
	}
	var pods corev1.PodList
	if err := read(&pods, "get", "pods", "--all-namespaces"); err != nil {
		return GPUInspection{}, err
	}
	return buildGPUInspection(status, slices.Items, claims.Items, pods.Items), nil
}

func buildGPUInspection(status GPUStatus, slices []resourcev1.ResourceSlice, claims []resourcev1.ResourceClaim, pods []corev1.Pod) GPUInspection {
	report := GPUInspection{Status: status, CapturedAt: time.Now().UTC(), Devices: []GPUDeviceAccounting{}, Claims: []GPUClaimStatus{}, Workloads: []GPUWorkloadStatus{}, Notes: []string{
		"Memory capacity and reservations are scheduler accounting against each VM GPU window, not physical free memory, measured usage, or a hard isolation limit.",
		"All GPU workers on this Mac share the host GPU and unified memory. Do not add their windows to estimate host capacity.",
		"This snapshot uses separate API reads; allocations may change while it is collected. Unreserved capacity alone does not guarantee scheduling.",
	}}
	type accounting struct {
		device   GPUDeviceAccounting
		capacity resource.Quantity
		used     resource.Quantity
		unknown  bool
	}
	devices := map[string]*accounting{}
	// Never add stale pool generations or partially published inventories.
	pools := map[string][]resourcev1.ResourceSlice{}
	for _, slice := range slices {
		if slice.Spec.Driver != kiacGPUDriver {
			continue
		}
		current := pools[slice.Spec.Pool.Name]
		if len(current) == 0 || slice.Spec.Pool.Generation > current[0].Spec.Pool.Generation {
			pools[slice.Spec.Pool.Name] = []resourcev1.ResourceSlice{slice}
		} else if slice.Spec.Pool.Generation == current[0].Spec.Pool.Generation {
			pools[slice.Spec.Pool.Name] = append(current, slice)
		}
	}
	for pool, current := range pools {
		if int64(len(current)) != current[0].Spec.Pool.ResourceSliceCount {
			report.Notes = append(report.Notes, fmt.Sprintf("Pool %s has incomplete inventory; its capacity is unknown.", pool))
			continue
		}
		for _, slice := range current {
			for _, device := range slice.Spec.Devices {
				capacity, ok := device.Capacity["memory"]
				if !ok {
					continue
				}
				key := pool + "/" + device.Name
				if prior, exists := devices[key]; exists {
					prior.unknown = true
					continue
				}
				node := ""
				if slice.Spec.NodeName != nil {
					node = *slice.Spec.NodeName
				}
				devices[key] = &accounting{device: GPUDeviceAccounting{Node: node, Pool: pool, Device: device.Name, Capacity: capacity.Value.String()}, capacity: capacity.Value.DeepCopy()}
			}
		}
	}
	claimIndex := map[string]int{}
	for _, claim := range claims {
		relevant := false
		for _, request := range claim.Spec.Devices.Requests {
			if request.Exactly != nil && request.Exactly.DeviceClassName == kiacGPUDriver {
				relevant = true
			}
			for _, sub := range request.FirstAvailable {
				if sub.DeviceClassName == kiacGPUDriver {
					relevant = true
				}
			}
		}
		row := GPUClaimStatus{Namespace: claim.Namespace, Name: claim.Name, State: "pending", Deleting: claim.DeletionTimestamp != nil, Allocations: []GPUClaimAllocation{}, Consumers: []string{}}
		seen := map[string]bool{}
		if claim.Status.Allocation != nil {
			for _, result := range claim.Status.Allocation.Devices.Results {
				if result.Driver != kiacGPUDriver {
					continue
				}
				relevant = true
				row.State = "allocated"
				allocation := GPUClaimAllocation{Request: result.Request, Pool: result.Pool, Device: result.Device, AdminAccess: result.AdminAccess != nil && *result.AdminAccess}
				if result.ShareID != nil {
					allocation.ShareID = string(*result.ShareID)
				}
				memory, hasMemory := result.ConsumedCapacity["memory"]
				if hasMemory {
					allocation.Memory = memory.String()
				}
				row.Allocations = append(row.Allocations, allocation)
				key := result.Pool + "/" + result.Device
				shareKey := key + "/" + allocation.ShareID
				if seen[shareKey] || allocation.AdminAccess {
					continue
				}
				seen[shareKey] = true
				if device, ok := devices[key]; ok {
					if !hasMemory || memory.Sign() < 0 {
						device.unknown = true
					} else {
						device.used.Add(memory)
					}
				} else {
					report.Notes = append(report.Notes, fmt.Sprintf("Claim %s/%s references unavailable inventory %s.", claim.Namespace, claim.Name, key))
				}
			}
		}
		if !relevant {
			continue
		}
		for _, consumer := range claim.Status.ReservedFor {
			if consumer.Resource == "pods" && consumer.APIGroup == "" {
				row.Consumers = append(row.Consumers, consumer.Name)
			}
		}
		claimIndex[claim.Namespace+"/"+claim.Name] = len(report.Claims)
		report.Claims = append(report.Claims, row)
	}
	for _, pod := range pods {
		names := gpuPodClaimNames(pod)
		relevant := podRequestsKIACGPU(pod)
		for _, name := range names {
			if index, ok := claimIndex[pod.Namespace+"/"+name]; ok {
				relevant = true
				report.Claims[index].Consumers = append(report.Claims[index].Consumers, pod.Name)
			}
		}
		if !relevant {
			continue
		}
		row := GPUWorkloadStatus{Namespace: pod.Namespace, Name: pod.Name, Node: pod.Spec.NodeName, Phase: string(pod.Status.Phase), Claims: names}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
				row.Reason = condition.Reason
				row.Message = condition.Message
			}
		}
		if row.Reason == "" {
			row.Reason = pod.Status.Reason
			row.Message = pod.Status.Message
		}
		if row.Reason == "" {
			statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
			for _, container := range statuses {
				if waiting := container.State.Waiting; waiting != nil && waiting.Reason != "" {
					row.Reason = waiting.Reason
					row.Message = container.Name + ": " + waiting.Message
					break
				}
			}
		}
		report.Workloads = append(report.Workloads, row)
	}
	for _, value := range devices {
		if !value.unknown && value.used.Cmp(value.capacity) <= 0 {
			reserved := value.used.String()
			remaining := value.capacity.DeepCopy()
			remaining.Sub(value.used)
			unreserved := remaining.String()
			value.device.Reserved = &reserved
			value.device.Unreserved = &unreserved
		} else {
			report.Notes = append(report.Notes, fmt.Sprintf("Pool %s device %s has incomplete or inconsistent allocation accounting; reserved and unreserved memory are unknown.", value.device.Pool, value.device.Device))
		}
		report.Devices = append(report.Devices, value.device)
	}
	for i := range report.Claims {
		report.Claims[i].Consumers = uniqueGPUStrings(report.Claims[i].Consumers)
		sort.Slice(report.Claims[i].Allocations, func(a, b int) bool {
			x, y := report.Claims[i].Allocations[a], report.Claims[i].Allocations[b]
			return x.Request+"/"+x.Pool+"/"+x.Device+"/"+x.ShareID < y.Request+"/"+y.Pool+"/"+y.Device+"/"+y.ShareID
		})
	}
	sort.Slice(report.Claims, func(i, j int) bool {
		return report.Claims[i].Namespace+"/"+report.Claims[i].Name < report.Claims[j].Namespace+"/"+report.Claims[j].Name
	})
	sort.Slice(report.Workloads, func(i, j int) bool {
		return report.Workloads[i].Namespace+"/"+report.Workloads[i].Name < report.Workloads[j].Namespace+"/"+report.Workloads[j].Name
	})
	sort.Slice(report.Devices, func(i, j int) bool {
		return report.Devices[i].Pool+"/"+report.Devices[i].Device < report.Devices[j].Pool+"/"+report.Devices[j].Device
	})
	report.Notes = uniqueGPUStrings(report.Notes)
	return report
}

func gpuPodClaimNames(pod corev1.Pod) []string {
	names := []string{}
	for _, claim := range pod.Spec.ResourceClaims {
		if claim.ResourceClaimName != nil {
			names = append(names, *claim.ResourceClaimName)
		}
	}
	for _, claim := range pod.Status.ResourceClaimStatuses {
		if claim.ResourceClaimName != nil {
			names = append(names, *claim.ResourceClaimName)
		}
	}
	if claim := pod.Status.ExtendedResourceClaimStatus; claim != nil {
		names = append(names, claim.ResourceClaimName)
	}
	return uniqueGPUStrings(names)
}

func podRequestsKIACGPU(pod corev1.Pod) bool {
	containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	for _, c := range containers {
		for _, resources := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			if quantity := resources[corev1.ResourceName(gpuResourceName)]; quantity.Sign() > 0 {
				return true
			}
		}
	}
	return false
}

func uniqueGPUStrings(values []string) []string {
	sort.Strings(values)
	result := []string{}
	for _, value := range values {
		if value != "" && (len(result) == 0 || result[len(result)-1] != value) {
			result = append(result, value)
		}
	}
	return result
}
