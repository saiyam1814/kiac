package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	hostGPURefresh     = time.Minute
	hostGPUMaxAge      = 5 * time.Minute
	hostGPUOutputLimit = 64 << 10
)

type hostGPUDevice struct {
	Name         string `json:"name"`
	MetalSupport string `json:"metalSupport,omitempty"`
}

// Hardware identity is not a utilization sample or proof of a compute backend.
type hostGPUResponse struct {
	State      string          `json:"state"`
	CapturedAt *time.Time      `json:"capturedAt,omitempty"`
	Stale      bool            `json:"stale"`
	Message    string          `json:"message,omitempty"`
	Devices    []hostGPUDevice `json:"devices"`
}

type hostGPUCache struct {
	mu       sync.Mutex
	running  bool
	attempt  time.Time
	response hostGPUResponse
	probe    func(context.Context) ([]hostGPUDevice, error)
}

func (s *server) hostGPU(w http.ResponseWriter, r *http.Request) {
	c := &s.hostGPUCache
	c.mu.Lock()
	response := c.response
	if response.State == "" {
		response = hostGPUResponse{State: "collecting", Devices: []hostGPUDevice{}}
	}
	if c.probe == nil && (runtime.GOOS != "darwin" || runtime.GOARCH != "arm64") {
		response.State, response.Message = "unsupported", "Mac GPU inventory requires an Apple silicon Mac"
		c.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
		return
	}
	now := time.Now()
	if response.CapturedAt != nil && now.Sub(*response.CapturedAt) > hostGPUMaxAge {
		response.State, response.Stale, response.Devices = "unavailable", true, []hostGPUDevice{}
		response.Message = "Mac GPU inventory has expired; waiting for a fresh reading"
	}
	if !c.running && now.Sub(c.attempt) >= hostGPURefresh {
		c.running, c.attempt = true, now
		probe := c.probe
		if probe == nil {
			probe = readHostGPUInventory
		}
		go c.collect(probe)
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (c *hostGPUCache) collect(probe func(context.Context) ([]hostGPUDevice, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	devices, err := probe(ctx)
	response := hostGPUResponse{State: "unavailable", Message: "Mac GPU inventory unavailable", Devices: []hostGPUDevice{}}
	if err == nil && ctx.Err() == nil && len(devices) > 0 {
		now := time.Now().UTC()
		response = hostGPUResponse{State: "ready", CapturedAt: &now, Devices: devices}
	}
	c.mu.Lock()
	c.response, c.running = response, false
	c.mu.Unlock()
}

func readHostGPUInventory(ctx context.Context) ([]hostGPUDevice, error) {
	return runHostGPUInventory(ctx, "/usr/sbin/system_profiler", "SPDisplaysDataType", "-json", "-detailLevel", "mini")
}

func runHostGPUInventory(ctx context.Context, binary string, args ...string) ([]hostGPUDevice, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 250 * time.Millisecond
	output := &hostGPUBuffer{cancel: cancel}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	if err := cmd.Run(); err != nil || output.overflow || ctx.Err() != nil {
		return nil, errors.New("Mac GPU inventory unavailable")
	}
	return parseHostGPUInventory(output.buffer.Bytes())
}

// Named buffer prevents io.Copy using an embedded bytes.Buffer.ReadFrom and
// bypassing the output bound when a subprocess writes indefinitely.
type hostGPUBuffer struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *hostGPUBuffer) Write(p []byte) (int, error) {
	remaining := hostGPUOutputLimit - b.buffer.Len()
	if len(p) <= remaining {
		return b.buffer.Write(p)
	}
	_, _ = b.buffer.Write(p[:remaining])
	b.overflow = true
	b.cancel()
	return remaining, errors.New("Mac GPU inventory exceeded output limit")
}

func parseHostGPUInventory(data []byte) ([]hostGPUDevice, error) {
	var payload struct {
		Devices []struct {
			Model       string `json:"sppci_model"`
			Name        string `json:"_name"`
			Metal       string `json:"spdisplays_metal"`
			MetalFamily string `json:"spdisplays_mtlgpufamilysupport"`
		} `json:"SPDisplaysDataType"`
	}
	if len(data) > hostGPUOutputLimit || json.Unmarshal(data, &payload) != nil || len(payload.Devices) > 32 {
		return nil, errors.New("invalid Mac GPU inventory")
	}
	devices := []hostGPUDevice{}
	for _, row := range payload.Devices {
		name := strings.TrimSpace(row.Model)
		if name == "" {
			name = strings.TrimSpace(row.Name)
		}
		if name == "" || len(name) > 256 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			continue
		}
		// Do not infer an API version from generic support, or return display
		// serial numbers, identifiers, attached screens or raw command output.
		metalLabels := map[string]string{
			"spdisplays_supported": "Supported", "spdisplays_metal3": "Metal 3", "spdisplays_metal4": "Metal 4",
		}
		metal := metalLabels[row.MetalFamily]
		if metal == "" {
			metal = metalLabels[row.Metal]
		}
		devices = append(devices, hostGPUDevice{Name: name, MetalSupport: metal})
	}
	return devices, nil
}
