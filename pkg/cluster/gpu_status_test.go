package cluster

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestGPUInventoryReadsAreBounded(t *testing.T) {
	for _, distro := range []string{"k3s", "kubeadm"} {
		rt := &recordingRuntime{}
		m := &Manager{rt: rt}
		if _, err := m.gpuKubectl("cp", distro, "get", "nodes", "-o", "json"); err != nil {
			t.Fatal(err)
		}
		m.renderDeviceExists("gpu")
		if len(rt.execs) != 2 {
			t.Fatalf("execs=%v", rt.execs)
		}
		if rt.execs[0].timeout != 30*time.Second || rt.execs[1].timeout != 10*time.Second {
			t.Fatalf("unbounded diagnostics: %+v", rt.execs)
		}
		command := strings.Join(rt.execs[0].command, " ")
		if !strings.Contains(command, "--request-timeout=20s get nodes -o json") {
			t.Fatalf("missing API deadline: %s", command)
		}
		if (distro == "kubeadm") != strings.Contains(command, "--kubeconfig "+adminConf) {
			t.Fatalf("wrong kubeconfig: %s", command)
		}
	}
}
