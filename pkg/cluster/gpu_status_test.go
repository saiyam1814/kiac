package cluster

import (
	"encoding/json"
	"testing"
)

func TestGPUNodeSchedulableRequiresRealInventory(t *testing.T) {
	node := kubeNode{}
	node.Metadata.Labels = map[string]string{
		gpuResourceDomain + "/gpu.present": "true",
	}
	node.Status.Allocatable = map[string]string{gpuResourceName: "1"}
	node.Status.Conditions = []kubeCondition{{Type: "Ready", Status: "True"}}
	base := GPUNodeStatus{
		RenderDevice:  true,
		KubernetesAPI: "venus",
		DriverReady:   true,
		ResourceSlice: true,
	}

	for _, driver := range []string{"device-plugin", "dra"} {
		if !gpuNodeSchedulable(driver, base, node) {
			t.Fatalf("healthy %s node should be schedulable", driver)
		}
		for _, mutate := range []func(*GPUNodeStatus){
			func(status *GPUNodeStatus) { status.RenderDevice = false },
			func(status *GPUNodeStatus) { status.KubernetesAPI = "" },
			func(status *GPUNodeStatus) { status.DriverReady = false },
		} {
			status := base
			mutate(&status)
			if gpuNodeSchedulable(driver, status, node) {
				t.Errorf("%s node with incomplete real inventory was reported schedulable: %+v", driver, status)
			}
		}
	}

	missingLabel := node
	missingLabel.Metadata.Labels = map[string]string{}
	if gpuNodeSchedulable("device-plugin", base, missingLabel) {
		t.Fatal("device-plugin node without gpu.present label was reported schedulable")
	}

	noSlice := base
	noSlice.ResourceSlice = false
	if gpuNodeSchedulable("dra", noSlice, node) {
		t.Fatal("DRA node without a ResourceSlice was reported schedulable")
	}

	noCapacity := node
	noCapacity.Status.Allocatable = map[string]string{}
	if gpuNodeSchedulable("device-plugin", base, noCapacity) {
		t.Fatal("device-plugin node without allocatable capacity was reported schedulable")
	}
}

func TestGPUNodeSchedulableRejectsUnavailableNodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"cordoned", `{"spec":{"unschedulable":true},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`},
		{"not ready", `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`},
		{"unknown readiness", `{"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}`},
		{"missing readiness", `{}`},
		{"deleting", `{"metadata":{"deletionTimestamp":"2026-09-07T10:00:00Z"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var node kubeNode
			if err := json.Unmarshal([]byte(tc.raw), &node); err != nil {
				t.Fatal(err)
			}
			node.Metadata.Labels = map[string]string{gpuResourceDomain + "/gpu.present": "true"}
			node.Status.Allocatable = map[string]string{gpuResourceName: "1"}
			status := GPUNodeStatus{RenderDevice: true, KubernetesAPI: "venus", DriverReady: true, ResourceSlice: true}
			for _, driver := range []string{"device-plugin", "dra"} {
				if gpuNodeSchedulable(driver, status, node) {
					t.Errorf("%s reported a %s node as schedulable", driver, tc.name)
				}
			}
		})
	}
}
