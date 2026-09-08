package cluster

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestAPIServerIP(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "admin.conf server line",
			in:   "clusters:\n- cluster:\n    server: https://192.168.64.79:6443\n  name: kubernetes\n",
			want: "192.168.64.79",
		},
		{
			name: "compact yaml",
			in:   `{"server": "https://10.0.0.5:6443"}`,
			want: "10.0.0.5",
		},
		{name: "hostname endpoint is not an IP", in: "server: https://kiac-dev-control-plane:6443", wantErr: true},
		{name: "wrong port ignored", in: "server: https://192.168.64.79:8443", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := apiServerIP(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("apiServerIP(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("apiServerIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOrderNodes(t *testing.T) {
	infos := []runtime.Info{
		{Name: "kiac-dev-worker-10"},
		{Name: "kiac-dev-worker-2"},
		{Name: "kiac-dev-control-plane"},
		{Name: "kiac-dev-worker-1"},
		{Name: "kiac-dev-gpu-10"},
		{Name: "kiac-dev-gpu-2"},
	}
	cp, workers, err := orderNodes("dev", infos)
	if err != nil {
		t.Fatal(err)
	}
	if cp != "kiac-dev-control-plane" {
		t.Errorf("cp = %q", cp)
	}
	want := []string{"kiac-dev-worker-1", "kiac-dev-worker-2", "kiac-dev-worker-10", "kiac-dev-gpu-2", "kiac-dev-gpu-10"}
	if strings.Join(workers, ",") != strings.Join(want, ",") {
		t.Errorf("workers = %v, want %v (numeric order, not lexical)", workers, want)
	}
}

func TestOrderNodesRejectsUnknownOrMalformedNames(t *testing.T) {
	for _, bad := range []string{"kiac-dev-worker-x", "kiac-dev-worker-0", "kiac-dev-worker-01", "kiac-dev-other-1"} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := orderNodes("dev", []runtime.Info{{Name: "kiac-dev-control-plane"}, {Name: bad}})
			if err == nil || !strings.Contains(err.Error(), bad) {
				t.Fatalf("orderNodes error = %v, want error naming %q", err, bad)
			}
		})
	}
}

func TestOrderNodesMissingControlPlane(t *testing.T) {
	_, _, err := orderNodes("dev", []runtime.Info{{Name: "kiac-dev-worker-1"}})
	if err == nil || !strings.Contains(err.Error(), "control-plane") {
		t.Fatalf("err = %v, want missing control-plane error", err)
	}
}

func TestHealControlPlaneScript(t *testing.T) {
	s := healControlPlaneScript("192.168.64.79", "192.168.64.86")

	// The sed pattern must escape dots so 192.168.64.7 never matches
	// inside 192.168.64.79, and be word-bounded against suffix matches.
	if !strings.Contains(s, `s#\b192\.168\.64\.79\b#192.168.64.86#g`) {
		t.Errorf("script lacks the escaped, word-bounded sed:\n%s", s)
	}
	// admin.conf and super-admin.conf are the files the node entrypoint
	// never fixes; their presence here is the point of the script.
	for _, f := range []string{
		"/etc/kubernetes/admin.conf",
		"/etc/kubernetes/super-admin.conf",
		"/etc/kubernetes/manifests/etcd.yaml",
		"/etc/kubernetes/manifests/kube-apiserver.yaml",
		"/var/lib/kubelet/kubeadm-flags.env",
	} {
		if !strings.Contains(s, f) {
			t.Errorf("script does not rewrite %s", f)
		}
	}
	// Cert regen must not depend on /kind/kubeadm.conf: kiac's kubeadm
	// init never wrote it (that is what kills the first boot).
	if strings.Contains(s, "/kind/kubeadm.conf") {
		t.Errorf("script must not reference /kind/kubeadm.conf:\n%s", s)
	}
	if !strings.Contains(s, "kubeadm init phase certs apiserver --apiserver-advertise-address 192.168.64.86") {
		t.Errorf("script lacks flag-driven cert regeneration:\n%s", s)
	}
	if !strings.Contains(s, "systemctl restart kubelet") {
		t.Errorf("script never restarts the kubelet:\n%s", s)
	}
	// kiac-lb restart must be guarded: --no-lb clusters have no unit.
	if !strings.Contains(s, "systemctl is-enabled kiac-lb.service") {
		t.Errorf("script lacks the guarded kiac-lb restart:\n%s", s)
	}
}

func TestHealWorkerScript(t *testing.T) {
	s := healWorkerScript("192.168.64.86")
	// The guard keeps the script idempotent; the sed matches any stale
	// address so a half-finished earlier resume stays repairable.
	if !strings.Contains(s, "grep -q 'server: https://192.168.64.86:6443'") {
		t.Errorf("script lacks the already-healed guard:\n%s", s)
	}
	if !strings.Contains(s, `s#(server: https://)[0-9.]+(:6443)#\1192.168.64.86\2#`) {
		t.Errorf("script lacks the any-old-IP sed:\n%s", s)
	}
	if !strings.Contains(s, "systemctl restart kubelet") {
		t.Errorf("script never restarts the kubelet:\n%s", s)
	}
}

func TestHealConfigMapsScript(t *testing.T) {
	s := healConfigMapsScript("192.168.64.86")
	for _, want := range []string{
		"-n kube-system get configmap kube-proxy",
		"rollout restart daemonset kube-proxy",
		"-n kube-public get configmap cluster-info",
		"https://192.168.64.86:6443",
		"--kubeconfig /etc/kubernetes/admin.conf",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

func TestResumeKubeadmHealsEdgeProxy(t *testing.T) {
	cp := ControlPlane("dev")
	nodes := []string{cp, worker("dev", 1), worker("dev", 2)}
	for _, test := range []struct {
		name      string
		adminIP   string
		installed []string
	}{
		{name: "changed control-plane IP", adminIP: "192.168.64.79", installed: nodes},
		{name: "retry with current admin.conf", adminIP: "192.168.64.86", installed: nodes},
		{name: "only installed helpers", adminIP: "192.168.64.79", installed: nodes[1:2]},
		{name: "no edge proxy", adminIP: "192.168.64.79"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := newResumeEdgeProxyRuntime(t, test.adminIP, test.installed...)
			manager := &Manager{rt: rt}
			if err := manager.Resume("dev", time.Second); !errors.Is(err, rt.stopAtExport) {
				t.Fatalf("Resume error = %v, want stop at kubeconfig export; events: %v", err, rt.events)
			}
			if got := slices.Contains(rt.events, "control-plane"); got != (test.adminIP != rt.currentIP) {
				t.Errorf("control-plane files healed = %v; events: %v", got, rt.events)
			}
			if len(rt.kubeconfigs) != len(test.installed) {
				t.Fatalf("Resume refreshed edge-proxy kubeconfigs on %d nodes, want %d; events: %v", len(rt.kubeconfigs), len(test.installed), rt.events)
			}
			for _, node := range nodes {
				if rt.proxies[node] == nil {
					for _, action := range []string{"write:", "restart:", "rules:"} {
						if slices.Contains(rt.events, action+node) {
							t.Errorf("Resume performed %s on node without edge proxy: %s", action, node)
						}
					}
					continue
				}
				previous := -1
				for _, event := range []string{"api", "configmaps", "probe:" + node, "rbac", "token", "write:" + node, "restart:" + node, "rules:" + node, "export"} {
					index := slices.Index(rt.events, event)
					if index <= previous {
						t.Fatalf("missing or out-of-order %q; events: %v", event, rt.events)
					}
					previous = index
				}
				for _, want := range []string{
					"server: https://" + rt.currentIP + ":6443",
					"current-context: kiac-dev-edge-proxy",
					"certificate-authority-data: Zm9v",
					"token: resume-test-api-token",
				} {
					if !strings.Contains(rt.kubeconfigs[node], want) {
						t.Errorf("edge-proxy kubeconfig on %s missing %q:\n%s", node, want, rt.kubeconfigs[node])
					}
				}
				for _, forbidden := range []string{"192.168.64.79", "client-certificate-data", "client-key-data", "kubernetes-admin"} {
					if strings.Contains(rt.kubeconfigs[node], forbidden) {
						t.Errorf("edge-proxy kubeconfig on %s contains %q", node, forbidden)
					}
				}
				for _, want := range []string{
					"mktemp " + edgeProxyKubeconfigPath + ".XXXXXX",
					`cat > "$tmp"`,
					`chmod 0600 "$tmp"`,
					`mv "$tmp" ` + edgeProxyKubeconfigPath,
				} {
					if !strings.Contains(rt.writeScripts[node], want) {
						t.Errorf("edge-proxy update on %s missing %q", node, want)
					}
				}
				marker := strings.Index(rt.writeScripts[node], "touch "+edgeProxyKubeconfigPath+".restart-required")
				if marker < 0 || marker >= strings.Index(rt.writeScripts[node], `mv "$tmp" `+edgeProxyKubeconfigPath) {
					t.Errorf("edge-proxy update on %s does not mark the pending restart before replacing credentials", node)
				}
				if strings.Contains(rt.writeScripts[node], edgeProxyTokenPath) || strings.Contains(rt.execScripts["restart:"+node], edgeProxyTokenPath) {
					t.Errorf("edge-proxy update rewrites the tunnel token on %s", node)
				}
			}
			if len(test.installed) == 0 && (slices.Contains(rt.events, "rbac") || slices.Contains(rt.events, "token")) {
				t.Errorf("Resume requested edge-proxy credentials with no installed helpers: %v", rt.events)
			}
		})
	}
}

func TestResumeKubeadmEdgeProxyFailureStopsBeforeExport(t *testing.T) {
	cp := ControlPlane("dev")
	for _, event := range []string{"rbac", "write:" + cp, "restart:" + cp, "rules:" + cp} {
		t.Run(event, func(t *testing.T) {
			rt := newResumeEdgeProxyRuntime(t, "192.168.64.79", cp, worker("dev", 1))
			rt.failAt = event
			manager := &Manager{rt: rt}
			if err := manager.Resume("dev", time.Second); !errors.Is(err, rt.healFailure) {
				t.Fatalf("Resume error = %v, want edge-proxy recovery failure; events: %v", err, rt.events)
			}
			if slices.Contains(rt.events, "export") {
				t.Fatalf("Resume exported kubeconfig despite edge-proxy recovery failure: %v", rt.events)
			}
		})
	}
}

func TestResumeKubeadmEdgeProxyReconcilesOnlyWhenNeeded(t *testing.T) {
	cp, sibling := ControlPlane("dev"), worker("dev", 1)
	for _, test := range []struct {
		name              string
		staleConfig       bool
		staleToken        bool
		inactive          bool
		missingPrerouting bool
		missingOutput     bool
		pendingRestart    bool
		wantWrite         bool
		wantRestart       bool
	}{
		{name: "current config and healthy proxy"},
		{name: "current config and inactive service", inactive: true, wantRestart: true},
		{name: "current config and missing PREROUTING", missingPrerouting: true, wantRestart: true},
		{name: "current config and missing OUTPUT", missingOutput: true, wantRestart: true},
		{name: "current config and pending restart", pendingRestart: true, wantRestart: true},
		{name: "stale endpoint and healthy service", staleConfig: true, wantWrite: true, wantRestart: true},
		{name: "stale token and healthy service", staleToken: true, wantWrite: true, wantRestart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := newResumeEdgeProxyRuntime(t, "192.168.64.86", cp, sibling)
			desired := strings.ReplaceAll(rt.proxies[cp].config, "192.168.64.79", rt.currentIP)
			state := rt.proxies[cp]
			if !test.staleConfig {
				state.config = desired
			}
			if test.staleToken {
				state.config = strings.ReplaceAll(state.config, "resume-test-api-token", "old-api-token")
			}
			state.active = !test.inactive
			state.prerouting = !test.missingPrerouting
			state.output = !test.missingOutput
			state.pendingRestart = test.pendingRestart
			rt.proxies[sibling].config = desired
			manager := &Manager{rt: rt}
			if err := manager.Resume("dev", time.Second); !errors.Is(err, rt.stopAtExport) {
				t.Fatalf("Resume error = %v, want stop at kubeconfig export; events: %v", err, rt.events)
			}
			for event, want := range map[string]bool{
				"write:" + cp:        test.wantWrite,
				"restart:" + cp:      test.wantRestart,
				"write:" + sibling:   false,
				"restart:" + sibling: false,
			} {
				if got := slices.Contains(rt.events, event); got != want {
					t.Errorf("Resume performed %s = %v, want %v; events: %v", event, got, want, rt.events)
				}
			}
			if state.config != desired || !state.active || !state.prerouting || !state.output || state.pendingRestart {
				t.Errorf("proxy was not fully reconciled; events: %v", rt.events)
			}
		})
	}
}

func TestResumeKubeadmEdgeProxyRejectsProbeFailures(t *testing.T) {
	cp := ControlPlane("dev")
	for _, test := range []struct {
		name      string
		event     string
		transport bool
		output    string
	}{
		{name: "installation transport failure", event: "probe:" + cp, transport: true},
		{name: "absent node transport failure", event: "probe:" + worker("dev", 2), transport: true},
		{name: "state transport failure", event: "state:" + cp, transport: true},
		{name: "empty installation response", event: "probe:" + cp},
		{name: "unexpected installation response", event: "probe:" + cp, output: "sensitive-unexpected-output"},
		{name: "extra installation response", event: "probe:" + cp, output: "installed\nsensitive-unexpected-output"},
		{name: "empty state response", event: "state:" + cp},
		{name: "invalid digest", event: "state:" + cp, output: "sensitive-unexpected-output\nhealthy\n"},
		{name: "unexpected health response", event: "state:" + cp, output: strings.Repeat("a", 64) + "\nsensitive-unexpected-output\n"},
		{name: "extra state response", event: "state:" + cp, output: strings.Repeat("a", 64) + "\nhealthy\nsensitive-unexpected-output\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := newResumeEdgeProxyRuntime(t, "192.168.64.86", cp)
			if test.transport {
				rt.failAt = test.event
			} else {
				rt.outputs = map[string]string{test.event: test.output}
			}
			manager := &Manager{rt: rt}
			err := manager.Resume("dev", time.Second)
			if err == nil || errors.Is(err, rt.stopAtExport) {
				t.Fatalf("Resume ignored the probe failure: %v; events: %v", err, rt.events)
			}
			if test.transport && !errors.Is(err, rt.healFailure) {
				t.Errorf("Resume error = %v, want propagated transport failure", err)
			}
			if strings.Contains(err.Error(), "sensitive-unexpected-output") {
				t.Error("Resume included probe output in its error")
			}
			for _, event := range []string{"write:" + cp, "restart:" + cp, "export"} {
				if slices.Contains(rt.events, event) {
					t.Errorf("Resume performed %s despite probe failure; events: %v", event, rt.events)
				}
			}
		})
	}
}

func TestResumeKubeadmEdgeProxyPartialRetry(t *testing.T) {
	cp, sibling := ControlPlane("dev"), worker("dev", 1)
	for _, failure := range []string{"write:" + cp, "restart:" + cp, "rules:" + cp} {
		t.Run(failure, func(t *testing.T) {
			rt := newResumeEdgeProxyRuntime(t, "192.168.64.79", cp, sibling)
			rt.failAt = failure
			manager := &Manager{rt: rt}
			if err := manager.Resume("dev", time.Second); !errors.Is(err, rt.healFailure) {
				t.Fatalf("first Resume error = %v, want injected failure; events: %v", err, rt.events)
			}
			if rt.adminIP != rt.currentIP {
				t.Fatal("first Resume did not heal admin.conf before the failure")
			}
			rt.resetEvents()
			if err := manager.Resume("dev", time.Second); !errors.Is(err, rt.stopAtExport) {
				t.Fatalf("retry error = %v, want stop at kubeconfig export; events: %v", err, rt.events)
			}
			for event, want := range map[string]bool{
				"control-plane":      false,
				"write:" + cp:        failure == "write:"+cp,
				"restart:" + cp:      true,
				"write:" + sibling:   false,
				"restart:" + sibling: false,
			} {
				if got := slices.Contains(rt.events, event); got != want {
					t.Errorf("retry performed %s = %v, want %v; events: %v", event, got, want, rt.events)
				}
			}
			rt.resetEvents()
			if err := manager.Resume("dev", time.Second); !errors.Is(err, rt.stopAtExport) {
				t.Fatalf("healthy retry error = %v; events: %v", err, rt.events)
			}
			for _, event := range rt.events {
				if strings.HasPrefix(event, "write:") || strings.HasPrefix(event, "restart:") {
					t.Errorf("healthy retry performed %s; events: %v", event, rt.events)
				}
			}
		})
	}
}

func TestHealEdgeProxySystemdScriptsSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	cp := ControlPlane("dev")
	rt := newResumeEdgeProxyRuntime(t, "192.168.64.86", cp)
	manager := &Manager{rt: rt}
	if err := manager.healEdgeProxySystemd(cp, "dev", adminConf, []string{cp}); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"installation": rt.execScripts["probe:"+cp],
		"state":        rt.execScripts["state:"+cp],
		"restart":      rt.execScripts["restart:"+cp],
		"write":        rt.writeScripts[cp],
	} {
		if script == "" {
			t.Fatalf("missing %s script", name)
		}
		command := exec.Command(sh, "-n")
		command.Stdin = strings.NewReader(script)
		if out, err := command.CombinedOutput(); err != nil {
			t.Errorf("sh -n rejected %s script: %v\n%s", name, err, out)
		}
	}
}

type resumeEdgeProxyState struct {
	config         string
	active         bool
	prerouting     bool
	output         bool
	pendingRestart bool
}

type resumeEdgeProxyRuntime struct {
	runtime.HostRuntime
	mu           sync.Mutex
	infos        []runtime.Info
	adminIP      string
	currentIP    string
	proxies      map[string]*resumeEdgeProxyState
	kubeconfigs  map[string]string
	writeScripts map[string]string
	execScripts  map[string]string
	outputs      map[string]string
	events       []string
	nodesWaited  bool
	stopAtExport error
	failAt       string
	healFailure  error
}

func newResumeEdgeProxyRuntime(t *testing.T, adminIP string, installed ...string) *resumeEdgeProxyRuntime {
	t.Helper()
	t.Setenv("KUBECONFIG", t.TempDir())
	rt := &resumeEdgeProxyRuntime{
		adminIP:      adminIP,
		currentIP:    "192.168.64.86",
		proxies:      make(map[string]*resumeEdgeProxyState),
		kubeconfigs:  make(map[string]string),
		writeScripts: make(map[string]string),
		execScripts:  make(map[string]string),
		stopAtExport: errors.New("test stopped at kubeconfig export"),
		healFailure:  errors.New("test edge-proxy recovery failure"),
	}
	status := "stopped"
	if adminIP == rt.currentIP {
		status = "running"
	}
	for _, node := range []string{ControlPlane("dev"), worker("dev", 1), worker("dev", 2)} {
		rt.infos = append(rt.infos, runtime.Info{Name: node, Status: status, Backend: runtime.BackendContainer, Distro: "kubeadm"})
	}
	staleConfig, err := serviceAccountKubeconfig("dev", sampleAdminConf, "192.168.64.79", "resume-test-api-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range installed {
		rt.proxies[node] = &resumeEdgeProxyState{config: staleConfig, active: true, prerouting: true, output: true}
	}
	return rt
}

func (r *resumeEdgeProxyRuntime) resetEvents() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
	r.kubeconfigs = make(map[string]string)
	r.writeScripts = make(map[string]string)
	r.execScripts = make(map[string]string)
	r.nodesWaited = false
	r.failAt = ""
}

func (r *resumeEdgeProxyRuntime) Available() bool          { return true }
func (r *resumeEdgeProxyRuntime) Version() (string, error) { return "1.2.1", nil }
func (r *resumeEdgeProxyRuntime) SystemStart(installDefaultKernel bool) error {
	if installDefaultKernel {
		return errors.New("unexpected kernel installation")
	}
	return nil
}

func (r *resumeEdgeProxyRuntime) List(prefix string) ([]runtime.Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var infos []runtime.Info
	for _, info := range r.infos {
		if strings.HasPrefix(info.Name, prefix) {
			infos = append(infos, info)
		}
	}
	return infos, nil
}

func (r *resumeEdgeProxyRuntime) Start(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "start:"+name)
	for i := range r.infos {
		if r.infos[i].Name == name {
			r.infos[i].Status = "running"
		}
	}
	return nil
}

func (r *resumeEdgeProxyRuntime) IP(name string) (string, error) {
	if name != ControlPlane("dev") {
		return "", fmt.Errorf("unexpected IP lookup for %s", name)
	}
	return r.currentIP, nil
}

func (r *resumeEdgeProxyRuntime) Exec(name string, command ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := strings.Join(command, " ")
	var event, out string
	var err error
	switch {
	case cmd == "systemctl is-active containerd":
		return "active", nil
	case cmd == "sysctl -w net.ipv4.ip_forward=1",
		cmd == "sh -c "+senderOffloadFix,
		cmd == "sh -c cat /etc/default/kubelet 2>/dev/null || true":
		return "", nil
	case cmd == "cat "+adminConf:
		event = "admin"
		out = strings.ReplaceAll(sampleAdminConf, "kiac-dev-control-plane", r.adminIP)
		if r.nodesWaited {
			event, out, err = "export", "", r.stopAtExport
		}
	case cmd == "sh -euc "+healControlPlaneScript(r.adminIP, r.currentIP):
		event = "control-plane"
		r.adminIP = r.currentIP
	case cmd == "sh -euc "+healWorkerScript(r.currentIP):
		event = "worker:" + name
	case cmd == "kubectl --kubeconfig "+adminConf+" get --raw /readyz":
		event = "api"
	case cmd == "sh -euc "+healConfigMapsScript(r.currentIP):
		event = "configmaps"
	case cmd == "kubectl --kubeconfig "+adminConf+" wait --for=condition=Ready nodes --all --timeout=1s":
		event = "nodes-ready"
		r.nodesWaited = true
	case strings.Contains(cmd, "printf 'installed\\n'") && strings.Contains(cmd, "printf 'absent\\n'"):
		event = "probe:" + name
		for _, required := range []string{"[ -x " + edgeProxyNodePath + " ]", "[ -f " + edgeProxyKubeconfigPath + " ]", "[ -f " + edgeProxyTokenPath + " ]"} {
			if !strings.Contains(cmd, required) {
				return "", fmt.Errorf("installation probe missing %q", required)
			}
		}
		out = "absent\n"
		if r.proxies[name] != nil {
			out = "installed\n"
		}
	case strings.Contains(cmd, "sha256sum "+edgeProxyKubeconfigPath):
		event = "state:" + name
		for _, required := range []string{
			`PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux:$PATH"`,
			"command -v iptables-legacy || command -v iptables",
			"systemctl is-active --quiet kiac-edge-proxy.service",
			"-C PREROUTING -j KIAC-EDGE",
			"-C OUTPUT -j KIAC-EDGE-OUTPUT",
			"[ ! -f " + edgeProxyKubeconfigPath + ".restart-required ]",
		} {
			if !strings.Contains(cmd, required) {
				return "", fmt.Errorf("state probe missing %q", required)
			}
		}
		state := r.proxies[name]
		if state == nil {
			return "", fmt.Errorf("state probe on absent proxy %s", name)
		}
		health := "restart"
		if state.active && state.prerouting && state.output && !state.pendingRestart {
			health = "healthy"
		}
		out = fmt.Sprintf("%x\n%s\n", sha256.Sum256([]byte(state.config)), health)
	case cmd == "kubectl --kubeconfig "+adminConf+" get secret kiac-edge-proxy-token -n kube-system -o jsonpath={.data.token}":
		event = "token"
		out = base64.StdEncoding.EncodeToString([]byte("resume-test-api-token"))
	case strings.HasPrefix(cmd, "sh -euc systemctl restart kiac-edge-proxy.service\n"):
		event = "restart:" + name
	case strings.Contains(cmd, "-C PREROUTING -j KIAC-EDGE") && strings.Contains(cmd, "-C OUTPUT -j KIAC-EDGE-OUTPUT"):
		event = "rules:" + name
		state := r.proxies[name]
		if state == nil || !state.prerouting || !state.output {
			err = errors.New("edge-proxy rules missing")
		}
	default:
		return "", fmt.Errorf("unexpected exec on %s: %q", name, command)
	}
	r.events = append(r.events, event)
	if len(command) == 3 && command[0] == "sh" {
		r.execScripts[event] = command[2]
	}
	if event == r.failAt {
		return "", r.healFailure
	}
	if event == "restart:"+name {
		state := r.proxies[name]
		if state == nil {
			return "", fmt.Errorf("restart on absent proxy %s", name)
		}
		state.active = true
		state.prerouting = true
		state.output = r.failAt != "rules:"+name
		if strings.Contains(cmd, "rm -f "+edgeProxyKubeconfigPath+".restart-required") {
			state.pendingRestart = false
		}
	}
	if override, ok := r.outputs[event]; ok {
		out, err = override, nil
	}
	return out, err
}

func (r *resumeEdgeProxyRuntime) ExecStdin(name string, input io.Reader, command ...string) error {
	raw, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := strings.Join(command, " ")
	var event string
	switch {
	case name == ControlPlane("dev") && cmd == "kubectl --kubeconfig "+adminConf+" apply -f -" && string(raw) == edgeProxyRBAC:
		event = "rbac"
	case len(command) == 3 && command[0] == "sh" && strings.Contains(cmd, `mv "$tmp" `+edgeProxyKubeconfigPath):
		event = "write:" + name
	default:
		return fmt.Errorf("unexpected stdin exec on %s: %q", name, command)
	}
	r.events = append(r.events, event)
	if event == r.failAt {
		return r.healFailure
	}
	if event == "write:"+name {
		state := r.proxies[name]
		if state == nil {
			return fmt.Errorf("write on absent proxy %s", name)
		}
		state.config = string(raw)
		if strings.Contains(cmd, "touch "+edgeProxyKubeconfigPath+".restart-required") {
			state.pendingRestart = true
		}
		r.kubeconfigs[name] = string(raw)
		r.writeScripts[name] = command[2]
	}
	return nil
}
