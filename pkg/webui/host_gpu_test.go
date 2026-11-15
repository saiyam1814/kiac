package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostGPUInventoryKeepsOnlyHardwareIdentity(t *testing.T) {
	devices, err := parseHostGPUInventory([]byte(`{"SPDisplaysDataType":[{"sppci_model":"Apple M1 Max","spdisplays_metal":"spdisplays_supported","spdisplays_ndrvs":[{"serial":"PRIVATE-SCREEN-SERIAL"}]}],"secret":"PRIVATE"}`))
	if err != nil || len(devices) != 1 || devices[0].Name != "Apple M1 Max" || devices[0].MetalSupport != "Supported" {
		t.Fatalf("devices=%+v err=%v", devices, err)
	}
	encoded, _ := json.Marshal(devices)
	if strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "Metal 4") {
		t.Fatalf("leaked or inferred data: %s", encoded)
	}
	modern, err := parseHostGPUInventory([]byte(`{"SPDisplaysDataType":[{"sppci_model":"Apple M1 Max","spdisplays_mtlgpufamilysupport":"spdisplays_metal4"}]}`))
	if err != nil || len(modern) != 1 || modern[0].MetalSupport != "Metal 4" {
		t.Fatalf("modern inventory=%+v err=%v", modern, err)
	}
	for _, data := range []string{`{`, strings.Repeat("x", hostGPUOutputLimit+1)} {
		if _, err := parseHostGPUInventory([]byte(data)); err == nil {
			t.Fatal("invalid data accepted")
		}
	}
}

func getHostGPU(t *testing.T, s *server) hostGPUResponse {
	t.Helper()
	w := httptest.NewRecorder()
	s.hostGPU(w, httptest.NewRequest("GET", "/api/gpu/host", nil))
	var response hostGPUResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestHostGPUCollectionDeduplicatesAndExpires(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	s := &server{}
	s.hostGPUCache.probe = func(ctx context.Context) ([]hostGPUDevice, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []hostGPUDevice{{Name: "Apple M1 Max"}}, nil
	}
	for range 10 {
		if got := getHostGPU(t, s); got.State != "collecting" {
			t.Fatalf("state=%s", got.State)
		}
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	var response hostGPUResponse
	for time.Now().Before(deadline) {
		response = getHostGPU(t, s)
		if response.State == "ready" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if response.State != "ready" || calls.Load() != 1 {
		t.Fatalf("response=%+v calls=%d", response, calls.Load())
	}
	s.hostGPUCache.mu.Lock()
	old := time.Now().Add(-hostGPUMaxAge - time.Second)
	s.hostGPUCache.response.CapturedAt = &old
	s.hostGPUCache.mu.Unlock()
	response = getHostGPU(t, s)
	if !response.Stale || response.State != "unavailable" || len(response.Devices) != 0 {
		t.Fatalf("expired=%+v", response)
	}
}

func TestHostGPUFailureDoesNotExposeDiagnostics(t *testing.T) {
	c := hostGPUCache{}
	c.collect(func(ctx context.Context) ([]hostGPUDevice, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("hardware probe has no deadline")
		}
		return nil, errors.New("PRIVATE SCREEN SERIAL")
	})
	encoded, _ := json.Marshal(c.response)
	if c.response.State != "unavailable" || strings.Contains(string(encoded), "PRIVATE") {
		t.Fatalf("response=%s", encoded)
	}
}

func TestHostGPUCommandBoundsAndCancellation(t *testing.T) {
	if os.Getenv("KIAC_HOST_GPU_TEST_PROCESS") == "1" {
		for {
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 8192)))
		}
	}
	t.Setenv("KIAC_HOST_GPU_TEST_PROCESS", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := runHostGPUInventory(ctx, os.Args[0], "-test.run=^TestHostGPUCommandBoundsAndCancellation$"); err == nil {
		t.Fatal("unbounded subprocess accepted")
	}
	if ctx.Err() != nil {
		t.Fatal("subprocess reached its deadline instead of being stopped by the output limit")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		return
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel2()
	if _, err := runHostGPUInventory(ctx2, "sh", "-c", "exec sleep 10"); err == nil {
		t.Fatal("timeout not enforced")
	}
}
