// Package webui serves a local management UI for kiac clusters. Mutating
// operations shell out to this same kiac binary, so the UI and CLI can
// never drift, and job logs show exactly what the terminal would.
package webui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/saiyam1814/kiac/pkg/cluster"
)

//go:embed index.html
var indexHTML []byte

type job struct {
	mu     sync.Mutex
	Status string `json:"status"` // running | done | failed
	Log    string `json:"log"`
}

type server struct {
	mgr  *cluster.Manager
	self string

	mu   sync.Mutex
	jobs map[string]*job
	busy map[string]bool // cluster names with an in-flight create/delete
	next int
}

// Serve starts the UI server on 127.0.0.1:port and blocks.
func Serve(port int) (string, http.Handler, net.Listener, error) {
	self, err := os.Executable()
	if err != nil {
		return "", nil, nil, err
	}
	s := &server{mgr: cluster.NewManager(), self: self, jobs: map[string]*job{}, busy: map[string]bool{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/meta", s.meta)
	mux.HandleFunc("GET /api/clusters", s.listClusters)
	mux.HandleFunc("POST /api/clusters", s.createCluster)
	mux.HandleFunc("DELETE /api/clusters/{name}", s.deleteCluster)
	mux.HandleFunc("GET /api/clusters/{name}/kubeconfig", s.kubeconfig)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", nil, nil, err
	}
	url := fmt.Sprintf("http://%s", ln.Addr().String())
	return url, loopbackOnly(ln.Addr().String(), mux), ln, nil
}

// loopbackOnly rejects cross-origin and DNS-rebinding requests. The UI
// hands out cluster-admin kubeconfigs, so any web page running in the
// user's browser must not be able to reach the API: a Host header other
// than the listener (DNS rebinding) or a non-loopback Origin (CSRF via
// fetch) gets 403. curl and the served page itself are unaffected.
func loopbackOnly(addr string, next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	allowedHosts := map[string]bool{
		addr: true, "localhost:" + port: true, "[::1]:" + port: true,
	}
	allowedOrigins := map[string]bool{
		"http://" + addr: true, "http://localhost:" + port: true, "http://[::1]:" + port: true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !allowedOrigins[origin] {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) meta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"versions":       cluster.SupportedVersions(),
		"defaultVersion": cluster.DefaultK8sVersion,
		"cnis":           []string{"kindnet", "none"},
	})
}

type clusterInfo struct {
	Name  string     `json:"name"`
	Nodes []nodeInfo `json:"nodes"`
}

type nodeInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Image  string `json:"image"`
}

func (s *server) listClusters(w http.ResponseWriter, r *http.Request) {
	names, err := s.mgr.Clusters()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := []clusterInfo{}
	for _, n := range names {
		nodes, err := s.mgr.Nodes(n)
		if err != nil {
			continue
		}
		ci := clusterInfo{Name: n}
		for _, nd := range nodes {
			ci.Nodes = append(ci.Nodes, nodeInfo{Name: nd.Name, Status: nd.Status, Image: nd.Image})
		}
		out = append(out, ci)
	}
	writeJSON(w, 200, out)
}

type createReq struct {
	Name       string `json:"name"`
	Workers    int    `json:"workers"`
	K8sVersion string `json:"k8sVersion"`
	CNI        string `json:"cni"`
	Memory     string `json:"memory"`
	NoLB       bool   `json:"noLB"`
	NoMetrics  bool   `json:"noMetrics"`
	NoStorage  bool   `json:"noStorage"`
}

func sanitizeName(s string) bool { return cluster.ValidName(s) }

func (s *server) createCluster(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !sanitizeName(req.Name) {
		writeJSON(w, 400, map[string]string{"error": "name must be lowercase letters, digits, and dashes"})
		return
	}
	args := []string{"create", "cluster", "--name", req.Name,
		"--workers", strconv.Itoa(req.Workers)}
	if req.K8sVersion != "" {
		args = append(args, "--k8s-version", req.K8sVersion)
	}
	if req.CNI != "" {
		args = append(args, "--cni", req.CNI)
	}
	if req.Memory != "" {
		args = append(args, "--memory", req.Memory)
	}
	if req.NoLB {
		args = append(args, "--no-lb")
	}
	if req.NoMetrics {
		args = append(args, "--no-metrics")
	}
	if req.NoStorage {
		args = append(args, "--no-storage")
	}
	id, err := s.startJob(req.Name, args)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]string{"job": id})
}

func (s *server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	id, err := s.startJob(name, []string{"delete", "cluster", "--name", name})
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]string{"job": id})
}

func (s *server) kubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	cfg, err := s.mgr.Kubeconfig(name)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=kiac-%s.kubeconfig", name))
	_, _ = w.Write([]byte(cfg))
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j == nil {
		writeJSON(w, 404, map[string]string{"error": "no such job"})
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	writeJSON(w, 200, map[string]string{"status": j.Status, "log": j.Log})
}

// startJob runs the kiac binary itself with args, streaming combined
// output into an in-memory log the UI polls. One in-flight job per
// cluster name: a double-click cannot race two creates whose cleanup
// would tear down each other's node VMs.
func (s *server) startJob(name string, args []string) (string, error) {
	s.mu.Lock()
	if s.busy[name] {
		s.mu.Unlock()
		return "", fmt.Errorf("an operation on cluster %q is already running", name)
	}
	s.busy[name] = true
	s.next++
	id := strconv.Itoa(s.next)
	j := &job{Status: "running"}
	s.jobs[id] = j
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.busy, name)
			s.mu.Unlock()
		}()
		cmd := exec.Command(s.self, args...)
		cmd.Env = append(os.Environ(), "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		j.mu.Lock()
		defer j.mu.Unlock()
		j.Log = strings.TrimSpace(string(out))
		if err != nil {
			j.Status = "failed"
			j.Log += "\n" + err.Error()
			return
		}
		j.Status = "done"
	}()
	return id, nil
}
