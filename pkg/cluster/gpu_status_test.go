package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/saiyam1814/kiac/pkg/runtime"
	"os"
	"os/exec"
	"path/filepath"
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
		Health:        "healthy",
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
			func(status *GPUNodeStatus) { status.Health = "unknown" },
			func(status *GPUNodeStatus) { status.Health = "unhealthy" },
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
			status := GPUNodeStatus{RenderDevice: true, KubernetesAPI: "venus", DriverReady: true, ResourceSlice: true, Health: "healthy"}
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

func TestGPULiveHealthRejectsStaleAndUnsupportedResults(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, state, scope string
		age                time.Duration
		want               string
	}{
		{"fresh", "healthy", "vulkan-transfer", time.Second, "healthy"},
		{"failed", "unhealthy", "vulkan-transfer", time.Second, "unhealthy"},
		{"stale", "healthy", "vulkan-transfer", 21 * time.Second, "unknown"},
		{"future", "healthy", "vulkan-transfer", -time.Minute, "unknown"},
		{"wrong scope", "healthy", "file-exists", time.Second, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(gpuLiveHealth{State: tc.state, Scope: tc.scope, CheckedAt: now.Add(-tc.age)})
			if got := parseGPULiveHealth(string(raw), now); got.State != tc.want {
				t.Fatalf("health=%+v want %s", got, tc.want)
			}
		})
	}
	if got := parseGPULiveHealth("not json", now); got.State != "unknown" {
		t.Fatal("invalid JSON accepted")
	}
}
func TestGPUResourceSliceTaintsPreventScheduling(t *testing.T) {
	for _, effect := range []string{"NoSchedule", "NoExecute"} {
		var list resourceSliceList
		raw := `{"items":[{"spec":{"driver":"gpu.kiac.dev","nodeName":"gpu","devices":[{"name":"venus-0","taints":[{"key":"other.example/maintenance","effect":"` + effect + `"}]}]}}]}`
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			t.Fatal(err)
		}
		if gpuSliceDeviceSchedulable(list.Items[0].Spec.Devices[0].Taints) {
			t.Fatalf("ignored %s device taint", effect)
		}
	}
}

func TestGPULiveHealthReadsHonorDiagnosticTimeout(t *testing.T) {
	for _, stage := range []string{"list", "exec"} {
		t.Run(stage, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "container")
			script := `#!/bin/sh
case "$*" in
 *"get pods"*)
  if [ "$KIAC_HEALTH_HANG" = list ]; then exec sleep 30; fi
  printf '%s\n' '{"items":[{"metadata":{"name":"agent"},"spec":{"nodeName":"gpu"}}]}'
  ;;
 *"--mode=health-status"*) exec sleep 30 ;;
 *) exit 0 ;;
esac
`
			if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			// Warm the newly created executable before measuring a sub-second
			// command deadline; macOS may inspect a new script on first exec.
			warmCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			if err := exec.CommandContext(warmCtx, binary, "warmup").Run(); err != nil {
				stop()
				t.Fatal(err)
			}
			stop()
			t.Setenv("KIAC_HEALTH_HANG", stage)
			m := &Manager{rt: &runtime.Client{Bin: binary}}
			start := time.Now()
			health, err := m.gpuDriverHealthNodes("cp", "kubeadm", "dra", 250*time.Millisecond)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("diagnostic ignored short deadline: %s", elapsed)
			}
			if stage == "list" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("list timeout=%v", err)
				}
			} else if err != nil || health["gpu"].State != "unknown" || !strings.Contains(health["gpu"].Message, "deadline exceeded") {
				t.Fatalf("exec timeout health=%+v err=%v", health, err)
			}
		})
	}
}
