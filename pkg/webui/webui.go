// Package webui serves a local management UI for kiac clusters. Mutating
// operations shell out to this same kiac binary, so the UI and CLI can
// never drift, and job logs show exactly what the terminal would.
package webui

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
)

//go:embed index.html
var indexHTML []byte

// hostKubectl runs the host's kubectl against a cluster's merged
// kubeconfig context. This works for every distro (kubeadm keeps its
// kubeconfig at /etc/kubernetes/admin.conf, k3s at
// /etc/rancher/k3s/k3s.yaml, but the merged host context kiac-<name>
// exists for both), unlike exec'ing kubectl inside the control-plane
// VM. A timeout guards against a stopped cluster hanging the UI poll.
func hostKubectl(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	full := append([]string{"--context", "kiac-" + name}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	return string(out), err
}

type job struct {
	mu     sync.Mutex
	Status string `json:"status"` // running | done | failed
	Log    string `json:"log"`
}

type server struct {
	mgr  *cluster.Manager
	self string

	// kubectl runs one kubectl invocation for a cluster. Defaults to
	// hostKubectl; tests swap it in so kubectlConsole can be exercised
	// without a real kubectl binary or cluster on the machine.
	kubectl func(name string, args ...string) (string, error)

	mu      sync.Mutex
	jobs    map[string]*job
	busy    map[string]bool   // cluster names with an in-flight mutating job
	created map[string]string // cluster name -> creationTimestamp, stable for the cluster's lifetime
	next    int
}

func newServer() (*server, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &server{mgr: cluster.NewManager(), self: self, kubectl: hostKubectl,
		jobs: map[string]*job{}, busy: map[string]bool{}, created: map[string]string{}}, nil
}

func (s *server) routes() *http.ServeMux {
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
	mux.HandleFunc("GET /api/clusters/{name}/metrics", s.metrics)
	mux.HandleFunc("GET /api/clusters/{name}/addons", s.addons)
	mux.HandleFunc("POST /api/clusters/{name}/nodes/{node}/stop", s.nodeAction("stop"))
	mux.HandleFunc("POST /api/clusters/{name}/nodes/{node}/start", s.nodeAction("start"))
	mux.HandleFunc("POST /api/clusters/{name}/kubectl", s.kubectlConsole)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	return mux
}

// Serve starts the UI server on 127.0.0.1:port and blocks.
func Serve(port int) (string, http.Handler, net.Listener, error) {
	s, err := newServer()
	if err != nil {
		return "", nil, nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", nil, nil, err
	}
	url := fmt.Sprintf("http://%s", ln.Addr().String())
	return url, loopbackOnly(ln.Addr().String(), s.routes()), ln, nil
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
		"versions":          cluster.SupportedVersions(),
		"defaultVersion":    cluster.DefaultK8sVersion,
		"k3sVersions":       cluster.SupportedK3sVersions(),
		"defaultK3sVersion": cluster.DefaultK3sVersion,
		"cnis":              []string{"kindnet", "cilium", "none"},
		"distros":           []string{"kubeadm", "k3s"},
		"gpuImages":         cluster.SupportedGPUImages(),
		"defaultGPUImage":   cluster.DefaultGPUImage,
		"gpuDrivers":        cluster.SupportedGPUResourceDrivers(),
	})
}

type clusterInfo struct {
	Name    string     `json:"name"`
	Created string     `json:"created,omitempty"`
	Nodes   []nodeInfo `json:"nodes"`
}

type nodeInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Image  string `json:"image"`
	Role   string `json:"role"`
	IP     string `json:"ip,omitempty"`
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
		ci := clusterInfo{Name: n, Created: s.createdAt(n)}
		for _, nd := range nodes {
			ni := nodeInfo{Name: nd.Name, Status: nd.Status, Image: nd.Image, Role: nodeRole(nd.Name)}
			// IPs come from inside the VM; a stopped node has none.
			if strings.EqualFold(nd.Status, "running") {
				if ip, err := s.mgr.NodeIP(nd.Name); err == nil {
					ni.IP = ip
				}
			}
			ci.Nodes = append(ci.Nodes, ni)
		}
		out = append(out, ci)
	}
	s.pruneCreated(names)
	writeJSON(w, 200, out)
}

func nodeRole(node string) string {
	if strings.HasSuffix(node, "-control-plane") {
		return "control-plane"
	}
	if idx := strings.LastIndex(node, "-gpu-"); idx >= 0 && decimalIndex(node[idx+len("-gpu-"):]) {
		return "gpu-worker"
	}
	return "worker"
}

// createdAt caches the control-plane Node's creationTimestamp: it never
// changes while the cluster exists, and fetching it costs an exec into
// the node VM on every list poll otherwise.
func (s *server) createdAt(name string) string {
	s.mu.Lock()
	c, ok := s.created[name]
	s.mu.Unlock()
	if ok {
		return c
	}
	cp := cluster.ControlPlane(name)
	out, err := hostKubectl(name,
		"get", "node", cp, "-o", "jsonpath={.metadata.creationTimestamp}")
	if err != nil {
		return ""
	}
	ts := strings.TrimSpace(out)
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		return ""
	}
	s.mu.Lock()
	s.created[name] = ts
	s.mu.Unlock()
	return ts
}

// pruneCreated drops cache entries for clusters that no longer exist, so
// a deleted-and-recreated cluster never shows the old creation time.
func (s *server) pruneCreated(names []string) {
	live := map[string]bool{}
	for _, n := range names {
		live[n] = true
	}
	s.mu.Lock()
	for n := range s.created {
		if !live[n] {
			delete(s.created, n)
		}
	}
	s.mu.Unlock()
}

type nodeMetrics struct {
	Node   string `json:"node"`
	CPU    string `json:"cpu"`
	CPUPct int    `json:"cpuPct"`
	Mem    string `json:"mem"`
	MemPct int    `json:"memPct"`
}

// metrics shells `kubectl top nodes` inside the control plane. Any
// failure (cluster still booting, metrics-server not scraped yet, node
// stopped) is a ready=false payload rather than an HTTP error, so the
// UI degrades to plain node state instead of breaking.
func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	out, err := hostKubectl(name, "top", "nodes", "--no-headers")
	rows := []nodeMetrics{}
	if err == nil {
		rows = parseTopNodes(out)
	}
	writeJSON(w, 200, map[string]any{"ready": err == nil && len(rows) > 0, "nodes": rows})
}

// parseTopNodes parses `kubectl top nodes --no-headers` rows:
// NAME CPU(cores) CPU% MEMORY(bytes) MEMORY%. Rows carrying <unknown>
// (a node metrics-server has not scraped) are dropped, not errors.
func parseTopNodes(out string) []nodeMetrics {
	var rows []nodeMetrics
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || strings.Contains(line, "<unknown>") {
			continue
		}
		cpuPct, err1 := strconv.Atoi(strings.TrimSuffix(f[2], "%"))
		memPct, err2 := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
		if err1 != nil || err2 != nil {
			continue
		}
		rows = append(rows, nodeMetrics{Node: f[0], CPU: f[1], CPUPct: cpuPct, Mem: f[3], MemPct: memPct})
	}
	return rows
}

var (
	ipv4Re     = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	hostPortRe = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}:\d{1,5}$`)
)

// addons reports the LoadBalancer endpoints of the optional stacks. A
// missing namespace, missing Service, or still-pending EXTERNAL-IP all
// yield an empty string, which the UI reads as "hide the chip". Results
// are regexp-validated so nothing but an address ever reaches an href.
func (s *server) addons(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	res := map[string]string{"grafana": "", "gateway": ""}
	// Older k3s clusters may still have multiple ServiceLB IPs in
	// status; prefer the lb-primary node's address because addon pods
	// are pinned there and pod-local delivery avoids vmnet's slow
	// forwarded path.
	primary := ""
	if out, err := hostKubectl(name, "get", "nodes", "-l", "kiac.io/lb-primary=true",
		"-o", "jsonpath={.items[0].status.addresses[?(@.type==\"InternalIP\")].address}"); err == nil {
		for _, a := range strings.Fields(out) {
			if ipv4Re.MatchString(a) {
				primary = a
				break
			}
		}
	}
	pick := func(ips []string) string {
		var first string
		for _, ip := range ips {
			if !ipv4Re.MatchString(ip) {
				continue
			}
			if ip == primary {
				return ip
			}
			if first == "" {
				first = ip
			}
		}
		return first
	}
	if out, err := hostKubectl(name, "-n", "kiac-observability", "get", "svc", "grafana",
		"-o", "jsonpath={range .status.loadBalancer.ingress[*]}{.ip} {end}|{.spec.ports[0].port}"); err == nil {
		parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
		if len(parts) == 2 {
			if ip := pick(strings.Fields(parts[0])); ip != "" {
				if v := ip + ":" + strings.TrimSpace(parts[1]); hostPortRe.MatchString(v) {
					res["grafana"] = v
				}
			}
		}
	}
	if out, err := hostKubectl(name, "-n", "kiac-gateway", "get", "svc",
		"-o", "jsonpath={.items[*].status.loadBalancer.ingress[*].ip}"); err == nil {
		if ip := pick(strings.Fields(out)); ip != "" {
			res["gateway"] = ip
		}
	}
	writeJSON(w, 200, res)
}

// kubectlConsole runs one host kubectl command against the cluster's
// context for the UI's embedded console. Not a shell: the args array is
// passed verbatim to the kubectl binary, so nothing is interpolated.
// Same trust boundary as the rest of the UI (loopback-only, and the UI
// can already create and delete whole clusters); the output cap keeps a
// verbose command from ballooning the response, and hostKubectl's
// timeout kills long-runners like `logs -f`.
func (s *server) kubectlConsole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sanitizeName(name) {
		writeJSON(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	var req struct {
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Args) == 0 {
		writeJSON(w, 400, map[string]string{"error": "args required"})
		return
	}
	out, err := s.kubectl(name, req.Args...)
	const maxOut = 256 << 10
	if len(out) > maxOut {
		out = out[:maxOut] + "\n... (output truncated)"
	}
	writeJSON(w, 200, map[string]any{"output": out, "ok": err == nil})
}

type createReq struct {
	Name              string `json:"name"`
	Workers           int    `json:"workers"`
	GPUWorkers        int    `json:"gpuWorkers"`
	GPUImage          string `json:"gpuImage"`
	GPUDiskSize       string `json:"gpuDiskSize"`
	GPUResourceDriver string `json:"gpuResourceDriver"`
	K8sVersion        string `json:"k8sVersion"`
	Distro            string `json:"distro"`
	CNI               string `json:"cni"`
	CPUs              string `json:"cpus"`
	Memory            string `json:"memory"`
	CPMemory          string `json:"cpMemory"`
	NoLB              bool   `json:"noLB"`
	NoMetrics         bool   `json:"noMetrics"`
	NoStorage         bool   `json:"noStorage"`
	NoEdgeProxy       bool   `json:"noEdgeProxy"`
	Observability     bool   `json:"observability"`
	Gateway           bool   `json:"gateway"`
}

func sanitizeName(s string) bool { return cluster.ValidName(s) }

func (s *server) createCluster(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	args, err := createClusterArgs(req)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	id, err := s.startJob(req.Name, args)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]string{"job": id})
}

func createClusterArgs(req createReq) ([]string, error) {
	if !sanitizeName(req.Name) {
		return nil, fmt.Errorf("name must be lowercase letters, digits, and dashes")
	}
	if req.Workers < 0 || req.GPUWorkers < 0 {
		return nil, fmt.Errorf("worker counts must be zero or greater")
	}
	switch req.Distro {
	case "", "kubeadm", "k3s":
	default:
		return nil, fmt.Errorf("distro must be kubeadm or k3s")
	}
	if req.GPUResourceDriver != "" && req.GPUResourceDriver != "device-plugin" && req.GPUResourceDriver != "dra" {
		return nil, fmt.Errorf("GPU resource driver must be device-plugin or dra")
	}
	if req.Gateway && req.NoLB {
		return nil, fmt.Errorf("Gateway API needs the built-in LoadBalancer")
	}
	if req.GPUWorkers > 0 && req.Distro != "k3s" {
		switch req.CNI {
		case "", "kindnet", "cilium":
		default:
			return nil, fmt.Errorf("GPU kubeadm clusters support kindnet or cilium, not %q", req.CNI)
		}
	}
	args := []string{"create", "cluster", "--name", req.Name, "--workers", strconv.Itoa(req.Workers)}
	if req.K8sVersion != "" {
		args = append(args, "--k8s-version", req.K8sVersion)
	}
	if req.Distro == "k3s" {
		// The backend selects K3s networking; the CLI rejects --cni here.
		args = append(args, "--distro", "k3s")
	} else if req.CNI != "" {
		args = append(args, "--cni", req.CNI)
		if req.CNI == "cilium" && req.GPUWorkers == 0 {
			// Cilium needs the full kernel; 'full' downloads the
			// published sha-pinned build once and caches it.
			args = append(args, "--kernel", "full")
		}
	}
	if req.CPUs != "" {
		args = append(args, "--cpus", req.CPUs)
	}
	if req.Memory != "" {
		args = append(args, "--memory", req.Memory)
	}
	if req.CPMemory != "" {
		args = append(args, "--cp-memory", req.CPMemory)
	}
	if req.GPUWorkers > 0 {
		args = append(args, "--gpu-workers", strconv.Itoa(req.GPUWorkers))
		if req.GPUImage != "" {
			args = append(args, "--gpu-image", req.GPUImage)
		}
		if req.GPUDiskSize != "" {
			args = append(args, "--gpu-disk-size", req.GPUDiskSize)
		}
		if req.GPUResourceDriver != "" {
			args = append(args, "--gpu-resource-driver", req.GPUResourceDriver)
		}
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
	if req.NoEdgeProxy {
		args = append(args, "--no-edge-proxy")
	}
	if req.Observability {
		args = append(args, "--observability")
	}
	if req.Gateway {
		args = append(args, "--gateway")
	}
	return args, nil
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

// nodeAction shells out to `kiac <verb> node <node> --name <cluster>`
// through the same per-cluster job lock as create/delete, so a stop can
// never race a delete of the same cluster.
func (s *server) nodeAction(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, node := r.PathValue("name"), r.PathValue("node")
		if !sanitizeName(name) || !clusterNode(name, node) {
			writeJSON(w, 400, map[string]string{"error": "invalid cluster or node name"})
			return
		}
		id, err := s.startJob(name, []string{verb, "node", node, "--name", name})
		if err != nil {
			writeJSON(w, 409, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 202, map[string]string{"job": id})
	}
}

// clusterNode reports whether node names a member of cluster name, so a
// crafted path cannot aim stop/start at an arbitrary container.
func clusterNode(name, node string) bool {
	if node == cluster.ControlPlane(name) {
		return true
	}
	for _, marker := range []string{"worker-", "gpu-"} {
		if rest, ok := strings.CutPrefix(node, "kiac-"+name+"-"+marker); ok {
			return decimalIndex(rest)
		}
	}
	return false
}

func decimalIndex(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
