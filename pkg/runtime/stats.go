package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"time"
)

// VMStats describes a sampled Apple container, not Kubernetes metrics-server
// data or the host hypervisor's memory footprint. Missing counters stay nil.
type VMStats struct {
	Node             string   `json:"node"`
	CPUCapacity      *uint64  `json:"cpuCapacity,omitempty"`
	CPUCoreUsage     *float64 `json:"cpuCoreUsage,omitempty"`
	CPUPercent       *float64 `json:"cpuPercent,omitempty"`
	MemoryUsageBytes *uint64  `json:"memoryUsageBytes,omitempty"`
	MemoryLimitBytes *uint64  `json:"memoryLimitBytes,omitempty"`
	MemoryPercent    *float64 `json:"memoryPercent,omitempty"`
}

// VMStats samples only explicit IDs supplied by the caller. In particular an
// empty list never expands into Apple's host-wide stats command. The caller
// must establish ownership before invoking this read-only adapter.
func (c *Client) VMStats(ctx context.Context, names []string) ([]VMStats, error) {
	if len(names) == 0 {
		return []VMStats{}, nil
	}
	if len(names) > 128 {
		return nil, fmt.Errorf("too many VM stats targets")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if !validVMName(name) || wanted[name] {
			return nil, fmt.Errorf("invalid or duplicate VM stats target")
		}
		wanted[name] = true
	}
	// Apple takes two samples two seconds apart, including --no-stream. One
	// shared deadline bounds both commands even when the caller omitted one.
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	capacity := map[string]uint64{}
	inspect, err := c.statsCommand(ctx, append([]string{"inspect"}, names...)...)
	if err == nil {
		capacity = parseVMCPUCapacity(inspect, wanted)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out, err := c.statsCommand(ctx, append([]string{"stats", "--no-stream", "--format", "json"}, names...)...)
	if err != nil {
		return nil, err
	}
	return parseVMStats(out, wanted, capacity)
}

func parseVMStats(out []byte, wanted map[string]bool, capacity map[string]uint64) ([]VMStats, error) {
	var rows []struct {
		ID               string   `json:"id"`
		CPUUtilization   *float64 `json:"cpuUtilization"`
		MemoryUsageBytes *uint64  `json:"memoryUsageBytes"`
		MemoryLimitBytes *uint64  `json:"memoryLimitBytes"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parsing container stats: %w", err)
	}
	result := make([]VMStats, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if !wanted[row.ID] {
			continue
		}
		if seen[row.ID] {
			return nil, fmt.Errorf("duplicate container stats ID")
		}
		seen[row.ID] = true
		stat := VMStats{Node: row.ID, MemoryUsageBytes: row.MemoryUsageBytes, MemoryLimitBytes: row.MemoryLimitBytes}
		if cpus := capacity[row.ID]; cpus > 0 {
			stat.CPUCapacity = &cpus
		}
		// Apple 1.7.1 emits fraction of one core: 2.5 means 2.5 busy
		// cores, NOT 2.5% or 250% of the allocated VM capacity.
		if cpu := row.CPUUtilization; cpu != nil && *cpu >= 0 && !math.IsInf(*cpu, 0) && !math.IsNaN(*cpu) {
			stat.CPUCoreUsage = cpu
			if stat.CPUCapacity != nil {
				percent := *cpu / float64(*stat.CPUCapacity) * 100
				if !math.IsInf(percent, 0) {
					stat.CPUPercent = &percent
				}
			}
		}
		if row.MemoryUsageBytes != nil && row.MemoryLimitBytes != nil && *row.MemoryLimitBytes > 0 {
			percent := float64(*row.MemoryUsageBytes) / float64(*row.MemoryLimitBytes) * 100
			stat.MemoryPercent = &percent
		}
		result = append(result, stat)
	}
	return result, nil
}

func parseVMCPUCapacity(out []byte, wanted map[string]bool) map[string]uint64 {
	var rows []struct {
		Configuration struct {
			ID        string `json:"id"`
			Resources struct {
				CPUs uint64 `json:"cpus"`
			} `json:"resources"`
		} `json:"configuration"`
	}
	result := map[string]uint64{}
	if json.Unmarshal(out, &rows) != nil {
		return result
	}
	for _, row := range rows {
		id, cpus := row.Configuration.ID, row.Configuration.Resources.CPUs
		if wanted[id] && cpus > 0 && cpus <= 65536 {
			result[id] = cpus
		}
	}
	return result
}

// statsCommand is deliberately separate from general runtime commands: a
// dashboard poll must bound both output allocation and inherited pipe waits.
func (c *Client) statsCommand(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.WaitDelay = pipeWaitDelay
	stdout := &statsOutput{remaining: 1 << 20, cancel: cancel}
	stderr := &statsOutput{remaining: 8 << 10, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("container stats output exceeded limit")
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("container stats command failed: %w", err)
	}
	return stdout.buffer.Bytes(), nil
}

type statsOutput struct {
	// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy
	// bypass Write, defeating these limits when os/exec drains the pipes.
	buffer    bytes.Buffer
	remaining int
	overflow  bool
	cancel    context.CancelFunc
}

func (b *statsOutput) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		n, _ := b.buffer.Write(p[:b.remaining])
		b.remaining = 0
		b.overflow = true
		if b.cancel != nil {
			b.cancel()
		}
		return n, io.ErrShortBuffer
	}
	b.remaining -= len(p)
	return b.buffer.Write(p)
}
