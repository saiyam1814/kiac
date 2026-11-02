package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

const (
	defaultHealthFile = "/run/kiac-gpu-health/report.json"
	healthMaxAge      = 20 * time.Second
	probeInterval     = 5 * time.Second
	probeTimeout      = 8 * time.Second
	healthTaintKey    = "gpu.kiac.dev/unhealthy"
)

type healthReport struct {
	Version   int       `json:"version"`
	PodUID    string    `json:"podUID"`
	NodeName  string    `json:"nodeName"`
	BootID    string    `json:"bootID"`
	Healthy   bool      `json:"healthy"`
	CheckedAt time.Time `json:"checkedAt"`
	Message   string    `json:"message"`
}

type healthState struct {
	healthy   bool
	checkedAt time.Time
	message   string
}

type healthMonitor struct {
	path, podUID, nodeName, bootID string
	mu                             sync.Mutex
	state                          healthState
	changed                        chan struct{}
}

func newHealthMonitor(path, podUID, nodeName, bootID string) *healthMonitor {
	return &healthMonitor{path: path, podUID: podUID, nodeName: nodeName, bootID: bootID, state: healthState{message: "waiting for a fresh Vulkan transfer probe"}, changed: make(chan struct{})}
}

// Reports are input from a separate container. Never follow a symlink or block
// on a FIFO, and accept only one bounded JSON object from this Pod and boot.
func readHealthReport(path, podUID, nodeName, bootID string, now time.Time) (healthState, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return healthState{}, fmt.Errorf("read Vulkan health report: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return healthState{}, err
	}
	if !st.Mode().IsRegular() || st.Size() > 4096 {
		return healthState{}, errors.New("Vulkan health report is not a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return healthState{}, err
	}
	if len(raw) > 4096 {
		return healthState{}, errors.New("Vulkan health report exceeds 4096 bytes")
	}
	var report healthReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return healthState{}, fmt.Errorf("invalid Vulkan health report: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return healthState{}, errors.New("Vulkan health report contains trailing data")
	}
	if report.Version != 1 || podUID == "" || bootID == "" || report.PodUID != podUID || report.NodeName != nodeName || report.BootID != bootID {
		return healthState{}, errors.New("Vulkan health report identity does not match this Pod and VM boot")
	}
	age := now.Sub(report.CheckedAt)
	if report.CheckedAt.IsZero() || age < -time.Second || age > healthMaxAge {
		return healthState{}, errors.New("Vulkan health report is stale or has an invalid timestamp")
	}
	if len(report.Message) > 1024 {
		return healthState{}, errors.New("Vulkan health message exceeds limit")
	}
	return healthState{healthy: report.Healthy, checkedAt: report.CheckedAt, message: report.Message}, nil
}

func (m *healthMonitor) refresh() {
	state, err := readHealthReport(m.path, m.podUID, m.nodeName, m.bootID, time.Now())
	if err != nil {
		state = healthState{message: err.Error()}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if state != m.state {
		m.state = state
		close(m.changed)
		m.changed = make(chan struct{})
	}
}
func (m *healthMonitor) snapshot() (healthState, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	// A stalled monitor must never extend the lease of a previous good probe.
	if state.healthy && time.Since(state.checkedAt) > healthMaxAge {
		state.healthy = false
		state.message = "Vulkan health report expired"
	}
	return state, m.changed
}
func (m *healthMonitor) run(ctx context.Context) {
	m.refresh()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.refresh()
		}
	}
}
func (m *healthMonitor) requireHealthy() error {
	if m == nil {
		return errors.New("Vulkan health monitor is not configured")
	}
	state, _ := m.snapshot()
	if !state.healthy {
		return fmt.Errorf("Venus GPU is unavailable: %s", state.message)
	}
	return nil
}
func currentBootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read VM boot identity: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", errors.New("VM boot identity is empty")
	}
	return id, nil
}

// Limited output also bounds a misbehaving native driver child's diagnostics.
type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.buffer.Len() < 2048 {
		_, _ = b.buffer.Write(p[:min(len(p), 2048-b.buffer.Len())])
	}
	return n, nil
}
func (b *boundedOutput) String() string { return b.buffer.String() }
func executeVulkanProbe(ctx context.Context, binary, renderDevice string) error {
	return executeProbeCommand(ctx, binary, "--render-device="+renderDevice)
}
func executeProbeCommand(ctx context.Context, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	out := &boundedOutput{}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("Vulkan transfer probe timed out: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("Vulkan transfer probe failed: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}
func publishHealth(path string, report healthReport) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".health-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0644); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func runHealthWatchdog(ctx context.Context, opts options) error {
	if opts.podUID == "" || opts.nodeName == "" {
		return errors.New("health watchdog requires --pod-uid and --node-name")
	}
	if !filepath.IsAbs(opts.probeBinary) {
		return errors.New("health watchdog requires an absolute --probe-binary path")
	}
	bootID, err := currentBootID()
	if err != nil {
		return err
	}
	return watchHealth(ctx, opts, bootID, func(ctx context.Context) error {
		if err := requireCharacterDevice(opts.renderDevice); err != nil {
			return fmt.Errorf("configured Venus render device: %w", err)
		}
		return executeVulkanProbe(ctx, opts.probeBinary, opts.renderDevice)
	}, probeInterval, probeTimeout)
}
func watchHealth(ctx context.Context, opts options, bootID string, probe func(context.Context) error, interval, timeout time.Duration) error {
	for {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		err := probe(probeCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		message := "Vulkan queue submission and transfer readback passed"
		if err != nil {
			message = err.Error()
		}
		if len(message) > 1024 {
			message = message[:1024]
		}
		report := healthReport{Version: 1, PodUID: opts.podUID, NodeName: opts.nodeName, BootID: bootID, Healthy: err == nil, CheckedAt: time.Now().UTC(), Message: message}
		if err := publishHealth(opts.healthFile, report); err != nil {
			return fmt.Errorf("publish Vulkan health: %w", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		state, changed := d.health.snapshot()
		status := kubeletplugin.HealthStatusUnhealthy
		if state.healthy {
			status = kubeletplugin.HealthStatusHealthy
		}
		report := kubeletplugin.DeviceHealthReport{Devices: []kubeletplugin.DeviceHealth{{PoolName: d.nodeName, DeviceName: deviceName, Health: status, LastUpdated: state.checkedAt, HealthCheckTimeout: healthMaxAge, Message: state.message}}}
		select {
		case <-ctx.Done():
			return nil
		case reports <- report:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-tick.C:
		}
	}
}

// This is transfer/queue health, not a compute-shader or performance guarantee.
type healthSummary struct {
	State     string    `json:"state"`
	Scope     string    `json:"scope"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	Message   string    `json:"message"`
}

func summarizeHealth(state healthState, err error) healthSummary {
	summary := healthSummary{State: "unhealthy", Scope: "vulkan-transfer", CheckedAt: state.checkedAt, Message: state.message}
	if err != nil {
		summary.State = "unknown"
		summary.Message = err.Error()
	} else if state.healthy {
		summary.State = "healthy"
	}
	return summary
}
