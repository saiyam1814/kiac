package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func getVMMetrics(t *testing.T, s *server, name string) (int, vmMetricsResponse) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/clusters/"+name+"/vm-metrics", nil)
	r.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.vmMetrics(w, r)
	var response vmMetricsResponse
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, response
}

func waitVMSample(t *testing.T, s *server, name string) vmMetricsResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, response := getVMMetrics(t, s, name)
		if response.State == "sampled" {
			return response
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime sampler did not complete")
	return vmMetricsResponse{}
}

func TestVMMetricsAsyncScopedAndDeduplicated(t *testing.T) {
	started, release := make(chan []string, 1), make(chan struct{})
	var calls atomic.Int32
	s := &server{vmStats: func(ctx context.Context, names []string) ([]runtime.VMStats, error) {
		calls.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("sampler context has no deadline")
		}
		started <- names
		<-release
		usage := uint64(123)
		return []runtime.VMStats{{Node: "kiac-dev-control-plane", MemoryUsageBytes: &usage}, {Node: "other", MemoryUsageBytes: &usage}}, nil
	}}
	s.recordVMInventory(map[string][]runtime.Info{"dev": {
		{Name: "kiac-dev-control-plane", Status: "running"},
		{Name: "kiac-dev-worker-1", Status: "stopped"},
		{Name: "kiac-dev-gpu-1", Status: "running", Backend: runtime.BackendKrunkit},
		{Name: "kiac-dev-extra-worker-1", Status: "running"},
	}})
	start := time.Now()
	code, response := getVMMetrics(t, s, "dev")
	if code != 200 || response.State != "collecting" || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("request waited for sampler: %d %+v", code, response)
	}
	if got := <-started; !reflect.DeepEqual(got, []string{"kiac-dev-control-plane"}) {
		t.Fatalf("sampler escaped ownership/backend scope: %v", got)
	}
	for range 4 {
		getVMMetrics(t, s, "dev")
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent polls spawned %d collectors", calls.Load())
	}
	close(release)
	response = waitVMSample(t, s, "dev")
	if response.SampledAt == nil || response.Stale || len(response.Nodes) != 3 {
		t.Fatalf("sample response = %+v", response)
	}
	states := map[string]string{}
	for _, row := range response.Nodes {
		states[row.Node] = row.State
		if row.Node != "kiac-dev-control-plane" && row.MemoryUsageBytes != nil {
			t.Fatalf("unsupported node received a fabricated sample: %+v", row)
		}
	}
	if states["kiac-dev-gpu-1"] != "unsupported" || states["kiac-dev-worker-1"] != "stopped" || states["kiac-dev-control-plane"] != "sampled" {
		t.Fatalf("states = %v", states)
	}
}

func TestVMMetricsExpiresAndInvalidatesSamples(t *testing.T) {
	s := &server{}
	inventory := map[string][]runtime.Info{"dev": {{Name: "kiac-dev-control-plane", Status: "running", Created: "old"}}}
	s.recordVMInventory(inventory)
	old := time.Now().Add(-time.Minute)
	value := uint64(123)
	s.vmCache["dev"] = &vmMetricsCache{attempt: time.Now(), response: vmMetricsResponse{
		Source: "apple/container", State: "sampled", SampledAt: &old,
		Nodes: []vmMetric{{VMStats: runtime.VMStats{Node: "kiac-dev-control-plane", MemoryUsageBytes: &value}, State: "sampled"}},
	}}
	_, response := getVMMetrics(t, s, "dev")
	if !response.Stale || response.State != "unavailable" || response.Nodes[0].MemoryUsageBytes != nil {
		t.Fatalf("expired sample presented as current: %+v", response)
	}
	inventory["dev"][0].Created = "new"
	s.recordVMInventory(inventory)
	if s.vmCache["dev"] != nil {
		t.Fatal("recreated VM retained old cache")
	}
	s.recordVMInventory(map[string][]runtime.Info{})
	if code, _ := getVMMetrics(t, s, "dev"); code != 404 {
		t.Fatalf("deleted cluster metrics status = %d", code)
	}
}

func TestVMMetricsStaleInventoryAndBusyClusterDoNotSample(t *testing.T) {
	var calls atomic.Int32
	s := &server{busy: map[string]bool{}, vmStats: func(context.Context, []string) ([]runtime.VMStats, error) {
		calls.Add(1)
		return nil, nil
	}}
	s.recordVMInventory(map[string][]runtime.Info{"dev": {{Name: "kiac-dev-control-plane", Status: "running"}}})
	s.vmInventory["dev"] = vmInventory{nodes: s.vmInventory["dev"].nodes, updated: time.Now().Add(-time.Minute)}
	_, response := getVMMetrics(t, s, "dev")
	if !response.Stale || calls.Load() != 0 {
		t.Fatalf("stale inventory was sampled: %+v calls=%d", response, calls.Load())
	}
	s.recordVMInventory(map[string][]runtime.Info{"dev": {{Name: "kiac-dev-control-plane", Status: "running"}}})
	s.busy["dev"] = true
	_, response = getVMMetrics(t, s, "dev")
	if response.State != "unavailable" || calls.Load() != 0 || response.Nodes[0].Message != "Cluster action in progress" {
		t.Fatalf("mutating cluster was sampled: %+v calls=%d", response, calls.Load())
	}
	if code, _ := getVMMetrics(t, s, "../bad"); code != 400 {
		t.Fatalf("invalid name status = %d", code)
	}
}

func TestVMMetricsGlobalConcurrencyLimit(t *testing.T) {
	started, release := make(chan struct{}, 3), make(chan struct{})
	s := &server{vmStats: func(context.Context, []string) ([]runtime.VMStats, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}}
	s.recordVMInventory(map[string][]runtime.Info{
		"one":   {{Name: "kiac-one-control-plane", Status: "running"}},
		"two":   {{Name: "kiac-two-control-plane", Status: "running"}},
		"three": {{Name: "kiac-three-control-plane", Status: "running"}},
	})
	for _, name := range []string{"one", "two", "three"} {
		getVMMetrics(t, s, name)
	}
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("more than two runtime subprocess collectors started")
	default:
	}
	close(release)
}

func TestVMMetricsLateResultCannotRestoreStoppedNode(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := &server{vmStats: func(context.Context, []string) ([]runtime.VMStats, error) {
		close(started)
		<-release
		usage := uint64(123)
		return []runtime.VMStats{{Node: "kiac-dev-control-plane", MemoryUsageBytes: &usage}}, nil
	}}
	s.recordVMInventory(map[string][]runtime.Info{"dev": {{Name: "kiac-dev-control-plane", Status: "running"}}})
	getVMMetrics(t, s, "dev")
	<-started
	s.recordVMInventory(map[string][]runtime.Info{"dev": {{Name: "kiac-dev-control-plane", Status: "stopped"}}})
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for len(s.vmSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.vmSlots) != 0 {
		t.Fatal("late sampler did not finish")
	}
	_, response := getVMMetrics(t, s, "dev")
	if len(response.Nodes) != 1 || response.Nodes[0].State != "stopped" || response.Nodes[0].MemoryUsageBytes != nil || response.SampledAt != nil {
		t.Fatalf("late sample reintroduced stopped node data: %+v", response)
	}
}

func TestVMMetricsRuntimeFailureDoesNotInventCounters(t *testing.T) {
	s := &server{vmStats: func(context.Context, []string) ([]runtime.VMStats, error) {
		return nil, errors.New("private runtime diagnostic")
	}}
	s.recordVMInventory(map[string][]runtime.Info{"dev": {{Name: "kiac-dev-control-plane", Status: "running"}}})
	getVMMetrics(t, s, "dev")
	deadline := time.Now().Add(2 * time.Second)
	for len(s.vmSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, response := getVMMetrics(t, s, "dev")
	row := response.Nodes[0]
	if response.State != "unavailable" || response.SampledAt != nil || row.State != "unavailable" || row.MemoryUsageBytes != nil || row.CPUPercent != nil {
		t.Fatalf("failed runtime invented sample: %+v", response)
	}
	if row.Message == "private runtime diagnostic" {
		t.Fatal("runtime stderr leaked to browser")
	}
}
