package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
)

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"dev": true, "my-cluster-2": true,
		"": false, "Dev": false, "a b": false, "a/b": false, "a;rm": false, "a..b": false,
	} {
		if got := cluster.ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The UI serves cluster-admin kubeconfigs, so cross-origin pages and
// DNS-rebinding hosts must be rejected outright.
func TestLoopbackOnly(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := loopbackOnly("127.0.0.1:5180", inner)

	cases := []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:5180", "", 200},                      // curl / same-origin GET
		{"localhost:5180", "", 200},                      // browser via localhost
		{"127.0.0.1:5180", "http://127.0.0.1:5180", 200}, // same-origin fetch
		{"127.0.0.1:5180", "http://localhost:5180", 200}, // localhost-origin fetch
		{"evil.example:5180", "", 403},                   // DNS rebinding
		{"127.0.0.1:5180", "https://evil.example", 403},  // cross-origin fetch
		{"127.0.0.1:5180", "http://127.0.0.1:9999", 403}, // wrong-port origin
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "http://placeholder/api/clusters", nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("host=%q origin=%q: got %d, want %d", c.host, c.origin, rec.Code, c.want)
		}
	}
}

// Serve must wrap every route, new endpoints included, in the loopback
// guard; a rebound Host reaching any API path would leak kubeconfigs.
func TestServeGuardsAllRoutes(t *testing.T) {
	_, handler, ln, err := Serve(0)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer ln.Close()

	for _, path := range []string{"/", "/api/meta", "/api/jobs/1",
		"/api/clusters/dev/metrics", "/api/clusters/dev/addons",
		"/api/clusters/dev/nodes/kiac-dev-worker-1/stop"} {
		req := httptest.NewRequest("GET", "http://placeholder"+path, nil)
		req.Host = "evil.example:5180"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("path %s with rebound host: got %d, want 403", path, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "http://placeholder/api/meta", nil)
	req.Host = ln.Addr().String()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("GET /api/meta with listener host: got %d, want 200", rec.Code)
	}
}

func TestMetaHasDistroSpecificVersionDefaults(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost/api/meta", nil)
	rec := httptest.NewRecorder()
	(&server{}).meta(rec, req)

	var got struct {
		Versions          []string `json:"versions"`
		DefaultVersion    string   `json:"defaultVersion"`
		K3sVersions       []string `json:"k3sVersions"`
		DefaultK3sVersion string   `json:"defaultK3sVersion"`
		GPUResourceName   string   `json:"gpuResourceName"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Versions) == 0 || got.Versions[0] != got.DefaultVersion {
		t.Fatalf("kubeadm versions/default = %v/%q", got.Versions, got.DefaultVersion)
	}
	if len(got.K3sVersions) == 0 || got.K3sVersions[0] != got.DefaultK3sVersion {
		t.Fatalf("k3s versions/default = %v/%q", got.K3sVersions, got.DefaultK3sVersion)
	}
	if got.DefaultVersion == got.DefaultK3sVersion {
		t.Fatalf("expected distro defaults to differ while k3s 1.37 is unavailable; both are %q", got.DefaultVersion)
	}
	if got.GPUResourceName != cluster.GPUResourceName {
		t.Fatalf("gpuResourceName = %q, want %q", got.GPUResourceName, cluster.GPUResourceName)
	}
}

func TestCreateClusterArgsGPUAndDistro(t *testing.T) {
	args := createClusterArgs(createReq{
		Name: "gpu", Workers: 2, K8sVersion: "1.36", Distro: "k3s", CNI: "cilium",
		CPUs: "6", Memory: "8G", GPUMock: true,
	})
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" --distro k3s ", " --gpu-mock ", " --cpus 6 ", " --memory 8G "} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q do not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, " --cni ") {
		t.Errorf("k3s args unexpectedly contain --cni: %q", joined)
	}
}

func TestParseTopNodes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []nodeMetrics
	}{
		{"empty", "", nil},
		{"typical", "kiac-dev-control-plane   138m   3%    912Mi   23%\nkiac-dev-worker-1   52m   1%    501Mi   12%\n",
			[]nodeMetrics{
				{Node: "kiac-dev-control-plane", CPU: "138m", CPUPct: 3, Mem: "912Mi", MemPct: 23},
				{Node: "kiac-dev-worker-1", CPU: "52m", CPUPct: 1, Mem: "501Mi", MemPct: 12},
			}},
		{"unknown rows dropped", "kiac-dev-control-plane   138m   3%    912Mi   23%\nkiac-dev-worker-1   <unknown>   <unknown>   <unknown>   <unknown>\n",
			[]nodeMetrics{
				{Node: "kiac-dev-control-plane", CPU: "138m", CPUPct: 3, Mem: "912Mi", MemPct: 23},
			}},
		{"garbage dropped", "error: Metrics API not available\n", nil},
	}
	for _, c := range cases {
		got := parseTopNodes(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %d rows, want %d", c.name, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: row %d = %+v, want %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// clusterNode gates which container names stop/start may target: only
// members of the named cluster, never arbitrary containers.
func TestClusterNode(t *testing.T) {
	cases := []struct {
		cluster, node string
		want          bool
	}{
		{"dev", "kiac-dev-control-plane", true},
		{"dev", "kiac-dev-worker-1", true},
		{"dev", "kiac-dev-worker-12", true},
		{"dev", "kiac-other-worker-1", false},
		{"dev", "kiac-dev-worker-", false},
		{"dev", "kiac-dev-worker-1x", false},
		{"dev", "kiac-dev-worker-1 --flag", false},
		{"dev", "anything", false},
		{"dev", "", false},
		{"my-app", "kiac-my-app-control-plane", true},
		{"my-app", "kiac-my-app-worker-2", true},
		// "my" must not claim my-app's nodes via prefix confusion.
		{"my", "kiac-my-app-worker-2", false},
	}
	for _, c := range cases {
		if got := clusterNode(c.cluster, c.node); got != c.want {
			t.Errorf("clusterNode(%q, %q) = %v, want %v", c.cluster, c.node, got, c.want)
		}
	}
}

func testServer(t *testing.T) *server {
	t.Helper()
	// Jobs exec s.self; a no-op binary keeps them fast and side-effect free.
	noop, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no `true` binary on PATH")
	}
	return &server{mgr: cluster.NewManager(), self: noop,
		kubectl: func(name string, args ...string) (string, error) { return "", nil },
		jobs:    map[string]*job{}, busy: map[string]bool{}, created: map[string]string{}}
}

// setBusy marks name as busy under the server's lock, matching
// startJob's own locking contract instead of writing s.busy directly.
func setBusy(s *server, name string) {
	s.mu.Lock()
	s.busy[name] = true
	s.mu.Unlock()
}

// waitIdle blocks until no job holds the cluster's in-flight lock.
func waitIdle(t *testing.T, s *server, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		busy := s.busy[name]
		s.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster %q never went idle", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func doJSON(t *testing.T, h http.Handler, method, path string) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:5180"+path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

// doJSONBody is doJSON but with a JSON body, for POSTs that read a payload.
func doJSONBody(t *testing.T, h http.Handler, method, path string, payload any) (int, map[string]json.RawMessage) {
	t.Helper()
	var buf bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			t.Fatalf("encoding request payload: %v", err)
		}
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:5180"+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestNodeActionEndpoints(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	cases := []struct {
		path string
		want int
	}{
		{"/api/clusters/Dev/nodes/kiac-dev-worker-1/stop", 400},       // bad cluster name
		{"/api/clusters/dev/nodes/kiac-other-worker-1/stop", 400},     // node of another cluster
		{"/api/clusters/dev/nodes/kiac-dev-worker-1x/start", 400},     // mangled node name
		{"/api/clusters/dev/nodes/kiac-dev-worker-1/stop", 202},       // valid
		{"/api/clusters/dev/nodes/kiac-dev-control-plane/start", 202}, // valid
	}
	for _, c := range cases {
		// Jobs on "dev" finish quickly (self is `true`); wait out the
		// per-cluster lock so only the invalid cases can 4xx.
		if c.want == 202 {
			waitIdle(t, s, "dev")
		}
		code, body := doJSON(t, mux, "POST", c.path)
		if code != c.want {
			t.Errorf("POST %s: got %d (%v), want %d", c.path, code, body, c.want)
		}
		if c.want == 202 && body["job"] == "" {
			t.Errorf("POST %s: no job id in %v", c.path, body)
		}
	}
}

// One in-flight job per cluster: a node action must 409 while another
// operation on the same cluster is still running.
func TestNodeActionBusyCluster(t *testing.T) {
	s := testServer(t)
	s.busy["dev"] = true
	code, body := doJSON(t, s.routes(), "POST", "/api/clusters/dev/nodes/kiac-dev-worker-1/stop")
	if code != 409 {
		t.Fatalf("busy cluster: got %d (%v), want 409", code, body)
	}
}

func TestJobsEndpointContract(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	code, _ := doJSON(t, mux, "GET", "/api/jobs/999")
	if code != 404 {
		t.Fatalf("unknown job: got %d, want 404", code)
	}

	id, err := s.startJob("dev", []string{})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := doJSON(t, mux, "GET", "/api/jobs/"+id)
		if code != 200 {
			t.Fatalf("job %s: got %d, want 200", id, code)
		}
		if body["status"] != "running" {
			if body["status"] != "done" {
				t.Fatalf("job %s: status %q, want done", id, body["status"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Metrics and addons degrade to empty payloads when the cluster (or the
// container CLI itself) is unreachable; the UI hides bars and chips.
func TestMetricsAndAddonsDegrade(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	req := httptest.NewRequest("GET", "http://127.0.0.1:5180/api/clusters/zz-webui-test-nope/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("metrics: got %d, want 200", rec.Code)
	}
	var m struct {
		Ready bool          `json:"ready"`
		Nodes []nodeMetrics `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("metrics: bad JSON: %v", err)
	}
	if m.Ready || len(m.Nodes) != 0 {
		t.Errorf("metrics for missing cluster: got ready=%v nodes=%v, want not ready and empty", m.Ready, m.Nodes)
	}

	code, body := doJSON(t, mux, "GET", "/api/clusters/zz-webui-test-nope/addons")
	if code != 200 {
		t.Fatalf("addons: got %d, want 200", code)
	}
	if body["grafana"] != "" || body["gateway"] != "" {
		t.Errorf("addons for missing cluster: got %v, want empty chips", body)
	}

	code, _ = doJSON(t, mux, "GET", "/api/clusters/BAD_NAME/metrics")
	if code != 400 {
		t.Errorf("metrics with invalid name: got %d, want 400", code)
	}
	code, _ = doJSON(t, mux, "GET", "/api/clusters/BAD_NAME/addons")
	if code != 400 {
		t.Errorf("addons with invalid name: got %d, want 400", code)
	}
}

func TestKubectlConsole(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	code, _ := doJSONBody(t, mux, "POST", "/api/clusters/BAD_NAME/kubectl", map[string]any{"args": []string{"get", "nodes"}})
	if code != 400 {
		t.Errorf("bad name: got %d, want 400", code)
	}

	code, _ = doJSONBody(t, mux, "POST", "/api/clusters/dev/kubectl", nil)
	if code != 400 {
		t.Errorf("no body: got %d, want 400", code)
	}

	code, _ = doJSONBody(t, mux, "POST", "/api/clusters/dev/kubectl", map[string]any{"args": []string{}})
	if code != 400 {
		t.Errorf("empty args: got %d, want 400", code)
	}

	// Fake the runner so this exercises the actual happy path (output
	// and ok:true) instead of whatever kubectl happens to be on PATH.
	s.kubectl = func(name string, args ...string) (string, error) {
		if name != "dev" || len(args) != 2 || args[0] != "get" || args[1] != "nodes" {
			t.Fatalf("kubectl called with name=%q args=%v", name, args)
		}
		return "NAME                     STATUS\nkiac-dev-control-plane   Ready\n", nil
	}
	code, body := doJSONBody(t, mux, "POST", "/api/clusters/dev/kubectl", map[string]any{"args": []string{"get", "nodes"}})
	if code != 200 {
		t.Fatalf("valid: got %d, want 200", code)
	}
	var output string
	if err := json.Unmarshal(body["output"], &output); err != nil {
		t.Fatalf("decoding output: %v", err)
	}
	if !strings.Contains(output, "kiac-dev-control-plane") {
		t.Errorf("output = %q, want fake kubectl output", output)
	}
	var ok bool
	if err := json.Unmarshal(body["ok"], &ok); err != nil {
		t.Fatalf("decoding ok: %v", err)
	}
	if !ok {
		t.Error("ok = false, want true on a successful kubectl call")
	}
}

// same shape as createCluster/deleteCluster: validate, job id, busy lock
func TestCreateClusterValidation(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	code, _ := doJSONBody(t, mux, "POST", "/api/clusters", map[string]any{"name": "Bad_Name"})
	if code != 400 {
		t.Errorf("bad name: got %d, want 400", code)
	}

	code, _ = doJSONBody(t, mux, "POST", "/api/clusters", map[string]any{"name": "dev", "distro": "openshift"})
	if code != 400 {
		t.Errorf("bad distro: got %d, want 400", code)
	}

	waitIdle(t, s, "create-ok")
	code, body := doJSONBody(t, mux, "POST", "/api/clusters", map[string]any{"name": "create-ok", "workers": 2})
	if code != 202 || body["job"] == nil {
		t.Errorf("valid: got %d %v, want 202 + job id", code, body)
	}

	setBusy(s, "create-busy")
	code, _ = doJSONBody(t, mux, "POST", "/api/clusters", map[string]any{"name": "create-busy"})
	if code != 409 {
		t.Errorf("busy: got %d, want 409", code)
	}
}

func TestDeleteCluster(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	code, _ := doJSON(t, mux, "DELETE", "/api/clusters/Bad_Name")
	if code != 400 {
		t.Errorf("bad name: got %d, want 400", code)
	}

	waitIdle(t, s, "delete-ok")
	code, body := doJSON(t, mux, "DELETE", "/api/clusters/delete-ok")
	if code != 202 || body["job"] == "" {
		t.Errorf("valid: got %d %v, want 202 + job id", code, body)
	}

	setBusy(s, "delete-busy")
	code, _ = doJSON(t, mux, "DELETE", "/api/clusters/delete-busy")
	if code != 409 {
		t.Errorf("busy: got %d, want 409", code)
	}
}

func TestKubeconfigEndpoint(t *testing.T) {
	s := testServer(t)
	mux := s.routes()

	code, _ := doJSON(t, mux, "GET", "/api/clusters/Bad_Name/kubeconfig")
	if code != 400 {
		t.Errorf("bad name: got %d, want 400", code)
	}

	// A missing cluster should be a 404 and a runtime/CLI failure a 500,
	// but Manager.Kubeconfig doesn't distinguish the two, and pkg/runtime
	// has no seam to fake a missing-vs-broken container CLI here. Not
	// asserting today's error path as if it were the real contract.
}

func TestNodeRole(t *testing.T) {
	if got := nodeRole("kiac-dev-control-plane"); got != "control-plane" {
		t.Errorf("got %q, want control-plane", got)
	}
	if got := nodeRole("kiac-dev-worker-1"); got != "worker" {
		t.Errorf("got %q, want worker", got)
	}
}

func TestPruneCreated(t *testing.T) {
	s := testServer(t)
	s.created = map[string]string{"dev": "t1", "gone": "t2"}
	s.pruneCreated([]string{"dev"})
	if _, ok := s.created["gone"]; ok {
		t.Error("gone should have been pruned")
	}
	if _, ok := s.created["dev"]; !ok {
		t.Error("dev should have survived")
	}
}
