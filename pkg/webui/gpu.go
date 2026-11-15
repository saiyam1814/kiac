package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/runtime"
)

const (
	gpuRefreshInterval = 10 * time.Second
	gpuSnapshotMaxAge  = 30 * time.Second
	gpuInventoryMaxAge = 20 * time.Second
	gpuInspectTimeout  = 20 * time.Second
	gpuOutputLimit     = 1 << 20
)

type gpuInventory struct {
	nodes   []gpuTarget
	updated time.Time
}

type gpuTarget struct {
	name, backend, status, created string
	gpu                            bool
}

type gpuResponse struct {
	State      string                 `json:"state"`
	CapturedAt *time.Time             `json:"capturedAt,omitempty"`
	Stale      bool                   `json:"stale"`
	Message    string                 `json:"message,omitempty"`
	Inspection *cluster.GPUInspection `json:"inspection,omitempty"`
}

type gpuCache struct {
	response gpuResponse
	attempt  time.Time
	running  bool
	cancel   context.CancelFunc
}

type gpuPending struct {
	name    string
	targets []gpuTarget
	cache   *gpuCache
}

// Inventory is supplied by the ordinary cluster-list poll, never by a caller's
// node names. Changes invalidate both cached data and running inspections.
func (s *server) recordGPUInventory(clusters map[string][]runtime.Info) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gpuInventory == nil {
		s.gpuInventory = map[string]gpuInventory{}
		s.gpuCache = map[string]*gpuCache{}
		s.gpuSlots = make(chan struct{}, 2)
	}
	for name, rows := range clusters {
		var targets []gpuTarget
		for _, row := range rows {
			if !clusterNode(name, row.Name) {
				continue
			}
			backend := row.Backend
			if backend == "" {
				backend = runtime.BackendContainer
			}
			targets = append(targets, gpuTarget{row.Name, backend, strings.ToLower(row.Status), row.Created, row.GPU || nodeRole(row.Name) == "gpu-worker"})
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
		if old, exists := s.gpuInventory[name]; !exists || !reflect.DeepEqual(old.nodes, targets) {
			s.invalidateGPULocked(name)
		}
		s.gpuInventory[name] = gpuInventory{nodes: targets, updated: now}
	}
	for name := range s.gpuInventory {
		if _, exists := clusters[name]; !exists {
			s.invalidateGPULocked(name)
			delete(s.gpuInventory, name)
		}
	}
	s.pruneGPUQueueLocked()
}

func (s *server) invalidateGPULocked(name string) {
	if cache := s.gpuCache[name]; cache != nil && cache.cancel != nil {
		cache.cancel()
	}
	delete(s.gpuCache, name)
}

func (s *server) pruneGPUQueueLocked() {
	queue := s.gpuQueue[:0]
	for _, pending := range s.gpuQueue {
		if s.gpuCache[pending.name] == pending.cache {
			queue = append(queue, pending)
		}
	}
	s.gpuQueue = queue
}

func idleGPUResponse(name string, targets []gpuTarget) gpuResponse {
	var found, running, controlPlaneRunning bool
	for _, target := range targets {
		if target.name == cluster.ControlPlane(name) {
			controlPlaneRunning = target.status == "running"
		}
		if target.gpu && target.backend == runtime.BackendKrunkit {
			found = true
			running = running || target.status == "running"
		}
	}
	if !found {
		return gpuResponse{State: "unsupported", Message: "This cluster has no Kiac GPU workers"}
	}
	if !running || !controlPlaneRunning {
		return gpuResponse{State: "stopped", Message: "Start the control plane and a GPU worker to inspect GPU resources"}
	}
	return gpuResponse{State: "collecting", Message: "Collecting GPU health and scheduling information"}
}

// The HTTP request only schedules work. At most two subprocesses run globally,
// and a FIFO queue deduplicates waiting work against current owned inventory.
func (s *server) gpu(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid name"})
		return
	}
	now := time.Now()
	s.mu.Lock()
	inventory, exists := s.gpuInventory[name]
	if !exists {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cluster inventory unavailable; refresh cluster list"})
		return
	}
	response := idleGPUResponse(name, inventory.nodes)
	switch {
	case s.busy[name]:
		s.invalidateGPULocked(name)
		response = gpuResponse{State: "unavailable", Message: "Cluster action in progress"}
	case now.Sub(inventory.updated) > gpuInventoryMaxAge:
		s.invalidateGPULocked(name)
		response = gpuResponse{State: "unavailable", Stale: true, Message: "Cluster inventory is stale; refresh cluster list"}
	case response.State != "collecting":
		s.invalidateGPULocked(name)
	default:
		cache := s.gpuCache[name]
		if cache == nil {
			cache = &gpuCache{response: response}
			s.gpuCache[name] = cache
		}
		response = cache.response
		if response.CapturedAt != nil && now.Sub(*response.CapturedAt) > gpuSnapshotMaxAge {
			response = gpuResponse{State: "unavailable", CapturedAt: response.CapturedAt, Stale: true, Message: "GPU snapshot expired; collecting an update"}
		}
		if !cache.running && now.Sub(cache.attempt) >= gpuRefreshInterval {
			cache.running, cache.attempt = true, now
			s.gpuQueue = append(s.gpuQueue, gpuPending{name, inventory.nodes, cache})
			s.startGPUCollectorsLocked(now)
		}
	}
	s.pruneGPUQueueLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (s *server) startGPUCollectorsLocked(now time.Time) {
	for len(s.gpuQueue) > 0 && len(s.gpuSlots) < cap(s.gpuSlots) {
		pending := s.gpuQueue[0]
		s.gpuQueue[0] = gpuPending{}
		s.gpuQueue = s.gpuQueue[1:]
		if s.gpuCache[pending.name] != pending.cache {
			continue
		}
		inventory, exists := s.gpuInventory[pending.name]
		if !exists || s.busy[pending.name] || now.Sub(inventory.updated) > gpuInventoryMaxAge {
			pending.cache.running = false
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), gpuInspectTimeout)
		pending.cache.cancel = cancel
		s.gpuSlots <- struct{}{}
		go s.collectGPU(ctx, pending, cancel)
	}
}

func (s *server) collectGPU(ctx context.Context, pending gpuPending, cancel context.CancelFunc) {
	defer cancel()
	response := gpuResponse{State: "unavailable", Message: "GPU inspection unavailable; check that the cluster and GPU driver are ready"}
	if s.gpuInspect != nil {
		inspection, err := s.gpuInspect(ctx, pending.name)
		if err == nil && ctx.Err() == nil && validGPUInspection(pending.name, pending.targets, inspection) {
			now := time.Now().UTC()
			response = gpuResponse{State: "ready", CapturedAt: &now, Inspection: &inspection}
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	<-s.gpuSlots
	// A late result cannot restore data from a deleted, restarted, or busy
	// cluster: every lifecycle change replaces this identity and cancels work.
	if s.gpuCache[pending.name] == pending.cache {
		pending.cache.response, pending.cache.running, pending.cache.cancel = response, false, nil
	}
	s.startGPUCollectorsLocked(now)
}

func validGPUInspection(name string, targets []gpuTarget, inspection cluster.GPUInspection) bool {
	if inspection.Status.Cluster != name || len(inspection.Status.Nodes) == 0 {
		return false
	}
	owned, gpu := map[string]bool{}, map[string]bool{}
	for _, target := range targets {
		owned[target.name] = true
		gpu[target.name] = target.gpu && target.backend == runtime.BackendKrunkit
	}
	for _, node := range inspection.Status.Nodes {
		if !gpu[node.Name] {
			return false
		}
	}
	for _, device := range inspection.Devices {
		if device.Node != "" && !gpu[device.Node] {
			return false
		}
	}
	for _, workload := range inspection.Workloads {
		if workload.Node != "" && !owned[workload.Node] {
			return false
		}
	}
	return true
}

// A subprocess bounds the entire existing inspection, including its nested
// runtime and Kubernetes calls. Kill its process group on cancellation so no
// child CLI remains alive after the dashboard stops waiting.
func inspectGPUCommand(ctx context.Context, self, name string) (cluster.GPUInspection, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "gpu", "inspect", "--name", name, "-o", "json")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	cmd.WaitDelay = 500 * time.Millisecond
	out := &gpuOutputBuffer{cancel: cancel}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	if err := cmd.Run(); err != nil || out.overflow || ctx.Err() != nil {
		return cluster.GPUInspection{}, errors.New("GPU inspection command unavailable")
	}
	var inspection cluster.GPUInspection
	if json.Unmarshal(out.buffer.Bytes(), &inspection) != nil {
		return cluster.GPUInspection{}, errors.New("GPU inspection returned invalid data")
	}
	return inspection, nil
}

// Use a named buffer: embedding bytes.Buffer would promote ReadFrom, allowing
// the subprocess output copier to bypass Write and the streaming size bound.
type gpuOutputBuffer struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *gpuOutputBuffer) Write(p []byte) (int, error) {
	remaining := gpuOutputLimit - b.buffer.Len()
	if len(p) <= remaining {
		return b.buffer.Write(p)
	}
	_, _ = b.buffer.Write(p[:remaining])
	b.overflow = true
	b.cancel()
	return remaining, errors.New("GPU inspection output exceeded limit")
}
