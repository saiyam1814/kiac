package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/cluster"
)

func TestGPUInspectRejectsOutputBeforeAccessingCluster(t *testing.T) {
	command := newGPUInspectCommand()
	command.SetArgs([]string{"--name", "missing", "--output", "yaml"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("error=%v", err)
	}
}

func TestGPUInspectionTextKeepsUnknownAndPendingReason(t *testing.T) {
	var out bytes.Buffer
	report := cluster.GPUInspection{
		Status:    cluster.GPUStatus{Cluster: "test", Driver: "dra"},
		Devices:   []cluster.GPUDeviceAccounting{{Pool: "gpu-1", Device: "venus-0", Capacity: "58Gi"}},
		Claims:    []cluster.GPUClaimStatus{{Namespace: "demo", Name: "waiting", State: "pending"}},
		Workloads: []cluster.GPUWorkloadStatus{{Namespace: "demo", Name: "model", Phase: "Pending", Reason: "Unschedulable", Message: "cannot allocate all claims"}},
	}
	if err := printGPUInspection(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"unknown", "demo/waiting", "pending", "Unschedulable", "cannot allocate all claims", "UNRESERVED (ACCOUNTING)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
}
