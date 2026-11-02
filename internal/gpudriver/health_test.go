package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func testHealthyMonitor() *healthMonitor {
	m := newHealthMonitor("", "pod", "node", "boot")
	m.state = healthState{healthy: true, checkedAt: time.Now(), message: "passed"}
	return m
}
func testReport(now time.Time) healthReport {
	return healthReport{Version: 1, PodUID: "pod", NodeName: "node", BootID: "boot", Healthy: true, CheckedAt: now, Message: "transfer passed"}
}
func TestHealthReportValidation(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "report.json")
	tests := []struct {
		name   string
		mutate func(*healthReport)
		suffix string
		valid  bool
	}{
		{name: "healthy", valid: true}, {name: "unhealthy valid report", mutate: func(r *healthReport) { r.Healthy = false }, valid: true},
		{name: "different pod", mutate: func(r *healthReport) { r.PodUID = "old" }},
		{name: "different boot", mutate: func(r *healthReport) { r.BootID = "old" }},
		{name: "different node", mutate: func(r *healthReport) { r.NodeName = "other" }},
		{name: "stale", mutate: func(r *healthReport) { r.CheckedAt = now.Add(-healthMaxAge - time.Second) }},
		{name: "future", mutate: func(r *healthReport) { r.CheckedAt = now.Add(time.Minute) }},
		{name: "missing timestamp", mutate: func(r *healthReport) { r.CheckedAt = time.Time{} }},
		{name: "version", mutate: func(r *healthReport) { r.Version = 2 }},
		{name: "second object", suffix: "{}"}, {name: "trailing junk", suffix: "x"},
		{name: "oversized", mutate: func(r *healthReport) { r.Message = strings.Repeat("x", 5000) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testReport(now)
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			raw, _ := json.Marshal(r)
			raw = append(raw, tc.suffix...)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			state, err := readHealthReport(path, "pod", "node", "boot", now)
			if (err == nil) != tc.valid {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if tc.valid && state.healthy != r.Healthy {
				t.Fatal("health changed")
			}
		})
	}
	for _, raw := range []string{"", `{"version":1`, `null`, `[]`, `{"version":1,"unknown":true}`} {
		os.WriteFile(path, []byte(raw), 0600)
		if _, err := readHealthReport(path, "pod", "node", "boot", now); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
func TestHealthReportRejectsUnsafeFilesWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	os.WriteFile(target, []byte("{}"), 0600)
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{symlink, fifo, root} {
		start := time.Now()
		if _, err := readHealthReport(path, "pod", "node", "boot", time.Now()); err == nil {
			t.Fatalf("accepted %s", path)
		}
		if time.Since(start) > time.Second {
			t.Fatal("unsafe file blocked")
		}
	}
}
func TestHealthMonitorFailureRecoveryAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	m := newHealthMonitor(path, "pod", "node", "boot")
	m.refresh()
	if m.requireHealthy() == nil {
		t.Fatal("missing report healthy")
	}
	if err := publishHealth(path, testReport(time.Now())); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	if err := m.requireHealthy(); err != nil {
		t.Fatal(err)
	}
	r := testReport(time.Now())
	r.Healthy = false
	r.Message = "queue failed"
	publishHealth(path, r)
	m.refresh()
	if m.requireHealthy() == nil {
		t.Fatal("failure remained healthy")
	}
	publishHealth(path, testReport(time.Now()))
	m.refresh()
	if err := m.requireHealthy(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.state.checkedAt = time.Now().Add(-healthMaxAge - time.Second)
	m.mu.Unlock()
	if m.requireHealthy() == nil {
		t.Fatal("stalled monitor extended health")
	}
}
func TestDRAHealthPreservesCapacityAndGatesPrepare(t *testing.T) {
	resources, err := driverResources("node", 32768)
	if err != nil {
		t.Fatal(err)
	}
	applyResourceHealth(&resources, false)
	device := resources.Pools["node"].Slices[0].Devices[0]
	capacity := device.Capacity["memory"]
	if len(device.Taints) != 1 || device.Taints[0].Effect != resourceapi.DeviceTaintEffectNoSchedule || capacity.Value.String() != "32Gi" {
		t.Fatalf("unhealthy inventory: %+v", device)
	}
	applyResourceHealth(&resources, true)
	if len(resources.Pools["node"].Slices[0].Devices[0].Taints) != 0 {
		t.Fatal("health did not clear owned taint")
	}
	drv := &driver{nodeName: "node", health: newHealthMonitor("", "pod", "node", "boot")}
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim")}}
	result, _ := drv.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if result[claim.UID].Err == nil {
		t.Fatal("unhealthy prepare succeeded")
	}
}
func TestDRAHealthWatchInitialFailureRecoveryAndCancellation(t *testing.T) {
	m := newHealthMonitor(filepath.Join(t.TempDir(), "report.json"), "pod", "node", "boot")
	d := &driver{nodeName: "node", health: m}
	ctx, cancel := context.WithCancel(t.Context())
	reports := make(chan kubeletplugin.DeviceHealthReport)
	done := make(chan error, 1)
	go func() { done <- d.WatchHealthStatus(ctx, reports) }()
	initial := <-reports
	if initial.Devices[0].Health != kubeletplugin.HealthStatusUnhealthy {
		t.Fatal("startup did not fail closed")
	}
	publishHealth(m.path, testReport(time.Now()))
	m.refresh()
	select {
	case report := <-reports:
		if report.Devices[0].Health != kubeletplugin.HealthStatusHealthy {
			t.Fatal("recovery missing")
		}
	case <-time.After(time.Second):
		t.Fatal("health watch missed transition")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch did not cancel")
	}
}

func TestProbeCommandChild(t *testing.T) {
	switch os.Getenv("KIAC_PROBE_TEST_CHILD") {
	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "output":
		for i := 0; i < 10000; i++ {
			os.Stderr.WriteString("probe failed\n")
		}
		os.Exit(1)
	}
}
func TestProbeCommandDeadlineAndOutputBound(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_PROBE_TEST_CHILD", "hang")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := executeProbeCommand(ctx, binary, "-test.run=^TestProbeCommandChild$"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("deadline result: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("probe child was not bounded")
	}
	t.Setenv("KIAC_PROBE_TEST_CHILD", "output")
	err = executeProbeCommand(t.Context(), binary, "-test.run=^TestProbeCommandChild$")
	if err == nil {
		t.Fatal("failed child unexpectedly succeeded")
	}
	if len(err.Error()) > 2300 {
		t.Fatalf("output bound failed: error length=%d", len(err.Error()))
	}
}
func TestWatchdogPublishesFailureAndFreshRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	opts := options{healthFile: path, podUID: "pod", nodeName: "node"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	calls := 0
	failed := make(chan struct{})
	recoverNow := make(chan struct{})
	probe := func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		select {
		case <-recoverNow:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() { done <- watchHealth(ctx, opts, "boot", probe, 10*time.Millisecond, time.Second) }()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		state, err := readHealthReport(path, "pod", "node", "boot", time.Now())
		if err == nil && !state.healthy {
			close(failed)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed probe not reported")
		}
	}
	<-failed
	close(recoverNow)
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		state, err := readHealthReport(path, "pod", "node", "boot", time.Now())
		if err == nil && state.healthy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery not reported")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
