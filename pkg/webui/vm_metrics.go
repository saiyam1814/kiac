package webui

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

const (
	vmRefreshInterval = 5 * time.Second
	vmSampleMaxAge    = 15 * time.Second
	vmInventoryMaxAge = 20 * time.Second
)

type vmInventory struct {
	nodes   []vmNodeTarget
	updated time.Time
}

type vmNodeTarget struct {
	name, backend, status, created string
}

type vmMetric struct {
	runtime.VMStats
	Backend string `json:"backend"`
	State   string `json:"state"` // sampled | stopped | unsupported | unavailable
	Message string `json:"message,omitempty"`
}

type vmMetricsResponse struct {
	Source    string     `json:"source"`
	State     string     `json:"state"` // collecting | sampled | unavailable
	SampledAt *time.Time `json:"sampledAt,omitempty"`
	Stale     bool       `json:"stale"`
	Nodes     []vmMetric `json:"nodes"`
}

type vmMetricsCache struct {
	response vmMetricsResponse
	attempt  time.Time
	running  bool
}

type vmPending struct {
	name    string
	targets []vmNodeTarget
	cache   *vmMetricsCache
}

// recordVMInventory is called by the existing cluster-list poll. Sampling can
// therefore stay entirely off the request path, while scope comes from real
// runtime inventory rather than names supplied in a URL. A changed/stopped or
// deleted node invalidates its cached sample and any in-flight collection.
func (s *server) recordVMInventory(clusters map[string][]runtime.Info) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vmInventory == nil {
		s.vmInventory = map[string]vmInventory{}
		s.vmCache = map[string]*vmMetricsCache{}
		s.vmSlots = make(chan struct{}, 2)
	}
	for name, rows := range clusters {
		var targets []vmNodeTarget
		for _, row := range rows {
			if !clusterNode(name, row.Name) {
				continue
			}
			backend := row.Backend
			if backend == "" {
				backend = runtime.BackendContainer
			}
			targets = append(targets, vmNodeTarget{row.Name, backend, strings.ToLower(row.Status), row.Created})
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
		if old, ok := s.vmInventory[name]; !ok || !reflect.DeepEqual(old.nodes, targets) {
			delete(s.vmCache, name)
		}
		s.vmInventory[name] = vmInventory{nodes: targets, updated: now}
	}
	for name := range s.vmInventory {
		if _, exists := clusters[name]; !exists {
			delete(s.vmInventory, name)
			delete(s.vmCache, name)
		}
	}
	// At most one queued item per current cache entry. Inventory churn
	// cannot accumulate deleted/recreated clusters behind active collectors.
	queue := s.vmQueue[:0]
	for _, pending := range s.vmQueue {
		if s.vmCache[pending.name] == pending.cache {
			queue = append(queue, pending)
		}
	}
	s.vmQueue = queue
}

func emptyVMMetrics(targets []vmNodeTarget) vmMetricsResponse {
	response := vmMetricsResponse{Source: "apple/container", State: "collecting", Nodes: []vmMetric{}}
	for _, target := range targets {
		row := vmMetric{VMStats: runtime.VMStats{Node: target.name}, Backend: target.backend, State: "unavailable", Message: "Waiting for a runtime sample"}
		switch {
		case target.backend != runtime.BackendContainer:
			row.State, row.Message = "unsupported", "VM resource samples are unavailable for this backend"
		case target.status != "running":
			row.State, row.Message = "stopped", "Node is not running"
		}
		response.Nodes = append(response.Nodes, row)
	}
	return response
}

// vmMetrics returns immediately. Apple's one-shot stats command intentionally
// waits two seconds; detached, bounded and deduplicated collection avoids
// delaying Kubernetes metrics or creating a subprocess per browser poll.
func (s *server) vmMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid name"})
		return
	}
	now := time.Now()
	s.mu.Lock()
	inventory, exists := s.vmInventory[name]
	if !exists {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cluster inventory unavailable; refresh cluster list"})
		return
	}
	if s.busy[name] {
		response := emptyVMMetrics(inventory.nodes)
		response.State = "unavailable"
		for i := range response.Nodes {
			if response.Nodes[i].State == "unavailable" {
				response.Nodes[i].Message = "Cluster action in progress"
			}
		}
		delete(s.vmCache, name)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
		return
	}
	cache := s.vmCache[name]
	if cache == nil {
		cache = &vmMetricsCache{response: emptyVMMetrics(inventory.nodes)}
		s.vmCache[name] = cache
	}
	response := cache.response
	if now.Sub(inventory.updated) > vmInventoryMaxAge {
		response = emptyVMMetrics(inventory.nodes)
		response.State, response.Stale = "unavailable", true
		for i := range response.Nodes {
			if response.Nodes[i].State == "unavailable" {
				response.Nodes[i].Message = "Cluster inventory is stale; refresh cluster list"
			}
		}
	} else {
		if response.SampledAt != nil && now.Sub(*response.SampledAt) > vmSampleMaxAge {
			// Never return an expired CPU/memory value as a current sample.
			response = emptyVMMetrics(inventory.nodes)
			response.State, response.Stale = "unavailable", true
			response.SampledAt = cache.response.SampledAt
		}
		if !cache.running && now.Sub(cache.attempt) >= vmRefreshInterval {
			cache.running, cache.attempt = true, now
			s.vmQueue = append(s.vmQueue, vmPending{name, inventory.nodes, cache})
			s.startVMSamplersLocked(now)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

// A FIFO queue prevents a fixed browser polling order from starving later
// clusters. Only two goroutines run; waiting entries are bounded by the
// owned inventory and deduplicated with cache.running.
func (s *server) startVMSamplersLocked(now time.Time) {
	for len(s.vmQueue) > 0 && len(s.vmSlots) < cap(s.vmSlots) {
		pending := s.vmQueue[0]
		s.vmQueue[0] = vmPending{}
		s.vmQueue = s.vmQueue[1:]
		inventory, exists := s.vmInventory[pending.name]
		if s.vmCache[pending.name] != pending.cache {
			continue
		}
		if !exists || s.busy[pending.name] || now.Sub(inventory.updated) > vmInventoryMaxAge {
			pending.cache.running = false
			continue
		}
		s.vmSlots <- struct{}{}
		go s.collectVMMetrics(pending.name, pending.targets, pending.cache)
	}
}

func (s *server) collectVMMetrics(name string, targets []vmNodeTarget, cache *vmMetricsCache) {
	response := emptyVMMetrics(targets)
	response.State = "unavailable"
	var names []string
	for _, target := range targets {
		if target.backend == runtime.BackendContainer && target.status == "running" {
			names = append(names, target.name)
		}
	}
	if len(names) > 0 && s.vmStats != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		rows, err := s.vmStats(ctx, names)
		cancel()
		if err == nil {
			byName := make(map[string]runtime.VMStats, len(rows))
			for _, row := range rows {
				byName[row.Node] = row
			}
			for i := range response.Nodes {
				row := &response.Nodes[i]
				if row.State != "unavailable" {
					continue
				}
				if stats, ok := byName[row.Node]; ok && (stats.CPUCoreUsage != nil || stats.MemoryUsageBytes != nil) {
					row.VMStats, row.State, row.Message = stats, "sampled", ""
					response.State = "sampled"
				} else {
					row.Message = "Runtime did not report a sample for this node"
				}
			}
		} else {
			for i := range response.Nodes {
				if response.Nodes[i].State == "unavailable" {
					response.Nodes[i].Message = "Runtime samples unavailable; check that Apple/container supports stats"
				}
			}
		}
	}
	now := time.Now()
	if response.State == "sampled" {
		response.SampledAt = &now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	<-s.vmSlots
	// Inventory changes replace the cache pointer, preventing a late sample
	// from reintroducing a deleted/stopped node or an old cluster instance.
	if s.vmCache[name] == cache {
		cache.response, cache.running = response, false
	}
	s.startVMSamplersLocked(now)
}
