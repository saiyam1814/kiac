package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/runtime"
)

func gpuTestInventory(names ...string) map[string][]runtime.Info {
	result := map[string][]runtime.Info{}
	for _, name := range names {
		result[name] = []runtime.Info{
			{Name: cluster.ControlPlane(name), Status: "running", Created: "original"},
			{Name: "kiac-" + name + "-gpu-1", Status: "running", Backend: runtime.BackendKrunkit, GPU: true, Created: "original"},
		}
	}
	return result
}

func gpuTestInspection(name string) cluster.GPUInspection {
	return cluster.GPUInspection{Status: cluster.GPUStatus{Cluster: name, Driver: "dra", Nodes: []cluster.GPUNodeStatus{{Name: "kiac-" + name + "-gpu-1", VMState: "running"}}}, CapturedAt: time.Now().UTC()}
}

func getGPU(t *testing.T, s *server, name string) (int, gpuResponse) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/clusters/"+name+"/gpu", nil)
	r.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.gpu(w, r)
	var response gpuResponse
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, response
}

func waitGPUState(t *testing.T, s *server, name, state string) gpuResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, response := getGPU(t, s, name)
		if response.State == state {
			return response
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("GPU inspection did not reach %s", state)
	return gpuResponse{}
}

func TestGPUInspectionAsyncDeduplicatedAndBounded(t *testing.T) {
	started, release := make(chan string, 1), make(chan struct{})
	var calls atomic.Int32
	s := &server{gpuInspect: func(ctx context.Context, name string) (cluster.GPUInspection, error) {
		calls.Add(1)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > gpuInspectTimeout {
			t.Error("GPU inspection has no bounded deadline")
		}
		started <- name
		<-release
		return gpuTestInspection(name), nil
	}}
	inventory := gpuTestInventory("dev")
	inventory["dev"] = append(inventory["dev"], runtime.Info{Name: "kiac-dev-extra-gpu-1", Backend: runtime.BackendKrunkit, GPU: true})
	s.recordGPUInventory(inventory)
	start := time.Now()
	code, response := getGPU(t, s, "dev")
	if code != 200 || response.State != "collecting" || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("HTTP request waited for inspection: %d %+v", code, response)
	}
	if name := <-started; name != "dev" {
		t.Fatalf("inspection name = %q", name)
	}
	for range 5 {
		getGPU(t, s, "dev")
	}
	if calls.Load() != 1 || len(s.gpuInventory["dev"].nodes) != 2 {
		t.Fatalf("collector duplicated or inventory escaped ownership: %d %+v", calls.Load(), s.gpuInventory)
	}
	close(release)
	response = waitGPUState(t, s, "dev", "ready")
	if response.Stale || response.CapturedAt == nil || response.Inspection == nil || response.Inspection.Status.Cluster != "dev" {
		t.Fatalf("snapshot = %+v", response)
	}
}

func TestGPUInspectionIdleAndStaleInventoryNeverCollect(t *testing.T) {
	var calls atomic.Int32
	s := &server{busy: map[string]bool{}, gpuInspect: func(context.Context, string) (cluster.GPUInspection, error) {
		calls.Add(1)
		return cluster.GPUInspection{}, nil
	}}
	inventory := gpuTestInventory("stopped", "control-stopped", "busy", "stale")
	inventory["stopped"][1].Status = "stopped"
	inventory["control-stopped"][0].Status = "stopped"
	inventory["ordinary"] = []runtime.Info{{Name: cluster.ControlPlane("ordinary"), Status: "running"}}
	s.recordGPUInventory(inventory)
	s.busy["busy"] = true
	stale := s.gpuInventory["stale"]
	stale.updated = time.Now().Add(-time.Minute)
	s.gpuInventory["stale"] = stale
	for name, state := range map[string]string{"stopped": "stopped", "control-stopped": "stopped", "ordinary": "unsupported", "busy": "unavailable", "stale": "unavailable"} {
		code, response := getGPU(t, s, name)
		if code != 200 || response.State != state || response.Inspection != nil || response.Stale != (name == "stale") {
			t.Fatalf("%s snapshot = %d %+v", name, code, response)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unavailable clusters triggered %d inspections", calls.Load())
	}
	if code, _ := getGPU(t, s, "../invalid"); code != 400 {
		t.Fatalf("invalid name returned %d", code)
	}
	if code, _ := getGPU(t, s, "missing"); code != 404 {
		t.Fatalf("unknown cluster returned %d", code)
	}
}

func TestGPUInspectionExpiredSnapshotWithheld(t *testing.T) {
	s := &server{}
	s.recordGPUInventory(gpuTestInventory("dev"))
	old := time.Now().Add(-time.Minute)
	inspection := gpuTestInspection("dev")
	s.gpuCache["dev"] = &gpuCache{attempt: time.Now(), response: gpuResponse{State: "ready", CapturedAt: &old, Inspection: &inspection}}
	_, response := getGPU(t, s, "dev")
	if !response.Stale || response.State != "unavailable" || response.Inspection != nil || response.CapturedAt == nil {
		t.Fatalf("expired snapshot exposed as current: %+v", response)
	}
}

func TestGPUInspectionLifecycleCancelsAndRejectsLateResults(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	s := &server{gpuInspect: func(ctx context.Context, name string) (cluster.GPUInspection, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		// Even a collector ignoring cancellation and returning success must
		// not repopulate the new inventory with its previous snapshot.
		return gpuTestInspection(name), nil
	}}
	inventory := gpuTestInventory("dev")
	s.recordGPUInventory(inventory)
	getGPU(t, s, "dev")
	<-started
	inventory["dev"][1].Status = "stopped"
	s.recordGPUInventory(inventory)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("inventory change did not cancel active inspection")
	}
	_, response := getGPU(t, s, "dev")
	if response.State != "stopped" || response.Inspection != nil {
		t.Fatalf("late result reintroduced stopped GPU: %+v", response)
	}
	s.recordGPUInventory(map[string][]runtime.Info{})
	if code, _ := getGPU(t, s, "dev"); code != 404 {
		t.Fatalf("deleted cluster returned %d", code)
	}
}

func TestGPUInspectionMutationCancelsActiveCollector(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	s := &server{self: "/usr/bin/true", busy: map[string]bool{}, jobs: map[string]*job{}, gpuInspect: func(ctx context.Context, name string) (cluster.GPUInspection, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return gpuTestInspection(name), nil
	}}
	s.recordGPUInventory(gpuTestInventory("dev"))
	getGPU(t, s, "dev")
	<-started
	if _, err := s.startJob("dev", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cluster mutation did not cancel GPU inspection")
	}
	s.mu.Lock()
	cached := s.gpuCache["dev"]
	s.mu.Unlock()
	if cached != nil {
		t.Fatal("cluster mutation retained the previous GPU snapshot")
	}
}

func TestGPUInspectionFIFOAndGlobalConcurrency(t *testing.T) {
	started, release := make(chan string, 10), make(chan struct{})
	s := &server{gpuInspect: func(_ context.Context, name string) (cluster.GPUInspection, error) {
		started <- name
		<-release
		return gpuTestInspection(name), nil
	}}
	names := []string{"one", "two", "three", "four", "five"}
	s.recordGPUInventory(gpuTestInventory(names...))
	for range 3 {
		for _, name := range names {
			getGPU(t, s, name)
		}
	}
	<-started
	<-started
	select {
	case name := <-started:
		t.Fatalf("third concurrent collector started: %s", name)
	default:
	}
	s.mu.Lock()
	queued := len(s.gpuQueue)
	s.mu.Unlock()
	if queued != 3 {
		t.Fatalf("queue contains %d entries, want three deduplicated clusters", queued)
	}
	close(release)
	for _, name := range names {
		waitGPUState(t, s, name, "ready")
	}
	if len(started) != 3 {
		t.Fatalf("waiting clusters were duplicated or starved: %d remaining starts", len(started))
	}
}

func TestGPUInspectionFailureAndScopeValidation(t *testing.T) {
	for _, scenario := range []string{"error", "wrong-cluster", "foreign-node", "foreign-device", "foreign-workload"} {
		t.Run(scenario, func(t *testing.T) {
			s := &server{gpuInspect: func(_ context.Context, name string) (cluster.GPUInspection, error) {
				inspection := gpuTestInspection(name)
				switch scenario {
				case "error":
					return inspection, errors.New("private runtime stderr")
				case "wrong-cluster":
					inspection.Status.Cluster = "other"
				case "foreign-node":
					inspection.Status.Nodes[0].Name = "kiac-other-gpu-1"
				case "foreign-device":
					inspection.Devices = []cluster.GPUDeviceAccounting{{Node: "kiac-other-gpu-1"}}
				case "foreign-workload":
					inspection.Workloads = []cluster.GPUWorkloadStatus{{Node: "kiac-other-worker-1"}}
				}
				return inspection, nil
			}}
			s.recordGPUInventory(gpuTestInventory("dev"))
			getGPU(t, s, "dev")
			response := waitGPUState(t, s, "dev", "unavailable")
			if response.Inspection != nil || response.CapturedAt != nil || strings.Contains(response.Message, "private") {
				t.Fatalf("failed or unowned report exposed: %+v", response)
			}
		})
	}
}

func gpuTestScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kiac")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGPUInspectionCommandParsesJSONAndWithholdsDiagnostics(t *testing.T) {
	data, err := json.Marshal(gpuTestInspection("dev"))
	if err != nil {
		t.Fatal(err)
	}
	path := gpuTestScript(t, "[ \"$*\" = 'gpu inspect --name dev -o json' ] || exit 99\nprintf '%s' '"+string(data)+"'\nprintf '%s' 'private stderr' >&2")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inspection, err := inspectGPUCommand(ctx, path, "dev")
	if err != nil || inspection.Status.Cluster != "dev" {
		t.Fatalf("inspection = %+v, %v", inspection, err)
	}
	for _, body := range []string{"printf '%s' 'private stdout'; printf '%s' 'private stderr' >&2; exit 1", "printf '%s' 'private invalid JSON'"} {
		if _, err := inspectGPUCommand(ctx, gpuTestScript(t, body), "dev"); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("command error leaks data: %v", err)
		}
	}
}

func TestGPUInspectionCommandCancelsOutputOverflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := gpuTestScript(t, "exec /usr/bin/yes private-output")
	started := time.Now()
	_, err := inspectGPUCommand(ctx, path, "dev")
	if err == nil || strings.Contains(err.Error(), "private-output") || ctx.Err() != nil || time.Since(started) > 4*time.Second {
		t.Fatalf("overflow did not stop a streaming writer: %v elapsed=%s", err, time.Since(started))
	}
}

func TestGPUInspectionCommandCancellationKillsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "orphan-finished")
	path := gpuTestScript(t, "(sleep 0.3; printf orphan > '"+marker+"') &\nwait")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := inspectGPUCommand(ctx, path, "dev"); err == nil || time.Since(started) > time.Second {
		t.Fatalf("inspection cancellation failed: %v elapsed=%s", err, time.Since(started))
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a canceled inspection left a live descendant: %v", err)
	}
}
