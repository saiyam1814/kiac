package cluster

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"gopkg.in/yaml.v3"
)

func workerFixture(t *testing.T) (ExternalWorkerConfig, string, string) {
	t.Helper()
	dir := t.TempDir()
	token := "K10" + strings.Repeat("a", 64) + "::node:private-fixture-token"
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	wg := "[Interface]\nAddress = 10.250.76.2/32\nPrivateKey = " + key + "\n[Peer]\nPublicKey = " + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)) + "\nEndpoint = 192.168.249.2:51820\nAllowedIPs = 10.250.76.0/24, 10.42.0.0/16, 10.43.0.0/16\n"
	cfg := ExternalWorkerConfig{Name: "alpha", Server: "https://10.250.76.1:6443", NodeIP: "10.250.76.2", K8sVersion: "1.37", PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", TokenFile: filepath.Join(dir, "token"), WireGuardFile: filepath.Join(dir, "wg"), WaitTimeout: time.Second}
	for path, content := range map[string]string{cfg.TokenFile: token, cfg.WireGuardFile: wg} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, token, key
}

func TestExternalWorkerConfigRejectsUnsupportedInputsBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ExternalWorkerConfig)
	}{
		{"name", func(c *ExternalWorkerConfig) { c.Name = "../other" }},
		{"http", func(c *ExternalWorkerConfig) { c.Server = "http://10.250.76.1:6443" }},
		{"credentials", func(c *ExternalWorkerConfig) { c.Server = "https://user:secret@10.250.76.1:6443" }},
		{"dns", func(c *ExternalWorkerConfig) { c.Server = "https://cluster.example:6443" }},
		{"same IP", func(c *ExternalWorkerConfig) { c.NodeIP = "10.250.76.1" }},
		{"version", func(c *ExternalWorkerConfig) { c.K8sVersion = "1.31" }},
		{"overlap", func(c *ExternalWorkerConfig) { c.PodCIDR = c.ServiceCIDR }},
		{"node in pods", func(c *ExternalWorkerConfig) { c.PodCIDR = "10.250.76.0/24" }},
		{"noncanonical", func(c *ExternalWorkerConfig) { c.PodCIDR = "10.42.1.0/16" }},
		{"cpu", func(c *ExternalWorkerConfig) { c.CPUs = "0" }},
		{"memory", func(c *ExternalWorkerConfig) { c.Memory = "512M" }},
		{"fractional MiB", func(c *ExternalWorkerConfig) { c.Memory = "1048577K" }},
		{"disk", func(c *ExternalWorkerConfig) { c.DiskSize = "1G" }},
		{"overflow", func(c *ExternalWorkerConfig) { c.DiskSize = "999999999999999999T" }},
		{"wait", func(c *ExternalWorkerConfig) { c.WaitTimeout = 2 * time.Hour }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := workerFixture(t)
			tc.change(&cfg)
			root := filepath.Join(t.TempDir(), "must-not-exist")
			m := &ExternalWorkerManager{root: root, preflight: func() error { t.Fatal("preflight before validation"); return nil }}
			if _, err := m.Join(cfg); err == nil {
				t.Fatal("unsupported configuration accepted")
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation wrote inventory: %v", err)
			}
		})
	}
}

func TestExternalWorkerSecretFilesAndSecureTokens(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "fifo", "oversize", "short-token", "bad-hash", "bad-credentials"} {
		t.Run(kind, func(t *testing.T) {
			cfg, token, _ := workerFixture(t)
			switch kind {
			case "public":
				if err := os.Chmod(cfg.TokenFile, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				original := cfg.TokenFile
				cfg.TokenFile += "-link"
				if err := os.Symlink(original, cfg.TokenFile); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				cfg.TokenFile += "-fifo"
				if err := syscall.Mkfifo(cfg.TokenFile, 0o600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(cfg.TokenFile, bytes.Repeat([]byte("x"), 4097), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				content := "short-secret"
				if kind == "bad-hash" {
					content = strings.Replace(token, "aaa", "xxx", 1)
				}
				if kind == "bad-credentials" {
					content = token[:69] + "not-a-credential"
				}
				if err := os.WriteFile(cfg.TokenFile, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			if _, err := prepareExternalWorker(cfg); err == nil {
				t.Fatal("unsafe secret accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("private file validation blocked")
			}
		})
	}
}

func TestExternalWorkerWireGuardIsStrictAndDoesNotEchoSecrets(t *testing.T) {
	cfg, _, key := workerFixture(t)
	raw, _ := os.ReadFile(cfg.WireGuardFile)
	for _, bad := range []string{
		string(raw) + "PostUp = echo " + key + "\n",
		string(raw) + "[Peer]\nPublicKey = " + key + "\n",
		strings.Replace(string(raw), "192.168.249.2:51820", "dns.example:51820", 1),
		strings.Replace(string(raw), "10.250.76.0/24", "0.0.0.0/0", 1),
		strings.Replace(string(raw), "10.250.76.0/24", "192.168.249.0/24", 1),
		strings.Replace(string(raw), "10.250.76.2/32", "10.250.76.2/24", 1),
		strings.Replace(string(raw), "PrivateKey = "+key, key, 1),
	} {
		if _, err := parseWorkerWireGuard(bad); err == nil || strings.Contains(err.Error(), key) {
			t.Fatalf("unsafe WG accepted or secret leaked: %v", err)
		}
	}
	if err := os.WriteFile(cfg.WireGuardFile, []byte(strings.Replace(string(raw), ", 10.43.0.0/16", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareExternalWorker(cfg); err == nil {
		t.Fatal("missing service route accepted")
	}
}

type workerRuntime struct {
	runtime.HostRuntime
	opts                                                          runtime.RunOpts
	events                                                        []string
	transfers                                                     [][]byte
	commands                                                      [][]string
	removed                                                       []string
	management                                                    string
	failTransferAt                                                int
	staleBootReports, observations                                int
	failObserve, badUID, badOwner, failRemove, noTaint, dualStack bool
	rows                                                          []runtime.Info
}

func (r *workerRuntime) RunDetached(opts runtime.RunOpts) error {
	r.opts = opts
	r.events = append(r.events, "run")
	return nil
}
func (r *workerRuntime) WaitReady(name string, _ time.Duration) error {
	r.events = append(r.events, "ssh:"+name)
	return nil
}
func (r *workerRuntime) IP(string) (string, error) { return r.management, nil }
func (r *workerRuntime) ExecStdinTimeout(_ string, _ time.Duration, in io.Reader, args ...string) error {
	b, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	r.transfers = append(r.transfers, b)
	r.commands = append(r.commands, args)
	if r.failTransferAt == len(r.transfers) {
		return fmt.Errorf("unsafe command echoed %s", b)
	}
	return nil
}
func (r *workerRuntime) ExecTimeout(_ string, _ time.Duration, args ...string) (string, error) {
	r.commands = append(r.commands, args)
	if len(args) > 0 && args[0] == "python3" {
		if r.failObserve {
			return "", fmt.Errorf("unsafe private-fixture-token output")
		}
		uid := "fixture-node-uid"
		if r.badUID {
			uid = "replaced-node-uid"
		}
		owner := strings.TrimPrefix(r.opts.NetworkID, "ext-")
		if r.badOwner {
			owner = "foreign"
		}
		r.observations++
		nodeBootID := "current-boot"
		if r.staleBootReports > 0 {
			r.staleBootReports--
			nodeBootID = "previous-boot"
		}
		b, _ := json.Marshal(workerObservation{Name: r.opts.Name, UID: uid, OwnerID: owner, NodeIP: "10.250.76.2", KubeletVersion: "v1.37.0+k3s1", APIVersion: "v1.37.0+k3s1", PodNetwork: "10.42.0.0/16", Backend: "vxlan", Ready: true, Tainted: !r.noTaint, IPv4Only: !r.dualStack, GuestBootID: "current-boot", NodeBootID: nodeBootID})
		return string(b), nil
	}
	return "", nil
}
func (r *workerRuntime) List(string) ([]runtime.Info, error) { return r.rows, nil }
func (r *workerRuntime) Start(name string) error {
	r.events = append(r.events, "start:"+name)
	return nil
}
func (r *workerRuntime) Stop(name string) error {
	r.events = append(r.events, "stop:"+name)
	return nil
}
func (r *workerRuntime) Remove(names ...string) error {
	r.removed = append(r.removed, names...)
	if r.failRemove {
		return errors.New("remove failed")
	}
	return nil
}

func workerManagerFixture(t *testing.T) (*ExternalWorkerManager, *workerRuntime) {
	t.Helper()
	r := &workerRuntime{management: "192.168.230.2"}
	artifact := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(artifact, []byte("pinned fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &ExternalWorkerManager{manager: &Manager{rt: r}, backend: r, root: t.TempDir(), preflight: func() error { return nil }, image: func() (string, error) { return "verified-fedora", nil }, artifacts: func(string) (k3sGPUArtifacts, error) {
		return k3sGPUArtifacts{Binary: artifact, SELinux: artifact}, nil
	}}
	m.owned = func(s ExternalWorkerState) error {
		if s.VMName != r.opts.Name || s.NetworkID != r.opts.NetworkID {
			return errors.New("foreign runtime")
		}
		return nil
	}
	return m, r
}

func TestExternalWorkerJoinOwnsOnlyOneVMAndPersistsNoCredentials(t *testing.T) {
	cfg, token, key := workerFixture(t)
	m, r := workerManagerFixture(t)
	s, err := m.Join(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Phase != "ready" || s.NodeUID != "fixture-node-uid" || s.NodeIP == r.management || strings.HasPrefix(s.VMName, "kiac-") {
		t.Fatalf("bad ready inventory: %+v", s)
	}
	if r.opts.Backend != runtime.BackendKrunkit || r.opts.GPU || r.opts.Distro != "external-k3s" || r.opts.NetworkID != s.NetworkID {
		t.Fatalf("wrong VM ownership opts: %+v", r.opts)
	}
	path, _ := m.statePath(cfg.Name)
	raw, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte(key)) || st.Mode().Perm() != 0o600 {
		t.Fatal("inventory exposed credentials")
	}
	for _, args := range r.commands {
		if strings.Contains(strings.Join(args, " "), token) || strings.Contains(strings.Join(args, " "), key) {
			t.Fatal("credential in command arguments")
		}
	}
	if len(r.transfers) != 3 {
		t.Fatalf("transfers=%d want 2 verified artifacts plus private tar", len(r.transfers))
	}
	if _, err := m.Join(cfg); err == nil {
		t.Fatal("existing worker overwritten")
	}
	if len(r.events) != 2 {
		t.Fatalf("duplicate join touched runtime: %v", r.events)
	}
	if err := m.Stop(cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(cfg.Name, time.Second); err != nil {
		t.Fatal(err)
	}
	r.badUID = true
	if err := m.Start(cfg.Name, time.Second); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("replaced remote Node accepted: %v", err)
	}
	if err := m.Delete(cfg.Name); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.removed, []string{s.VMName}) {
		t.Fatalf("deletion scope=%v", r.removed)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inventory retained after delete")
	}
}

func TestExternalWorkerFailureRedactionAndCleanupOwnership(t *testing.T) {
	for _, stage := range []string{"private-transfer", "observation", "management-route", "foreign-ownership", "remove-failure", "untainted", "dual-stack"} {
		t.Run(stage, func(t *testing.T) {
			cfg, token, key := workerFixture(t)
			cfg.WaitTimeout = time.Millisecond
			m, r := workerManagerFixture(t)
			switch stage {
			case "private-transfer":
				r.failTransferAt = 3
			case "observation":
				r.failObserve = true
			case "management-route":
				r.management = "10.42.0.2"
			case "foreign-ownership":
				r.badOwner = true
				m.owned = func(ExternalWorkerState) error { return errors.New("foreign runtime") }
			case "remove-failure":
				r.failTransferAt = 3
				r.failRemove = true
			case "untainted":
				r.noTaint = true
			case "dual-stack":
				r.dualStack = true
			}
			_, err := m.Join(cfg)
			if err == nil {
				t.Fatal("failure accepted")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "private-fixture-token") {
				t.Fatalf("error leaked a secret: %v", err)
			}
			path, _ := m.statePath(cfg.Name)
			_, statErr := os.Stat(path)
			retained := stage == "foreign-ownership" || stage == "remove-failure" || stage == "observation" || stage == "untainted" || stage == "dual-stack"
			if retained != (statErr == nil) {
				t.Fatalf("inventory retained=%v want=%v (%v)", statErr == nil, retained, err)
			}
			if stage == "foreign-ownership" && len(r.removed) != 0 {
				t.Fatal("removed foreign VM")
			}
			for _, name := range r.removed {
				if name != r.opts.Name {
					t.Fatal("cleanup escaped exact VM")
				}
			}
		})
	}
}

func TestExternalWorkerVPNNodeCannotClaimManagementSubnetWithNarrowRoutes(t *testing.T) {
	cfg, _, _ := workerFixture(t)
	cfg.NodeIP = "192.168.230.3"
	wg, err := os.ReadFile(cfg.WireGuardFile)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.ReplaceAll(string(wg), "Address = 10.250.76.2/32", "Address = 192.168.230.3/32")
	raw = strings.ReplaceAll(raw, "10.250.76.0/24", "10.250.76.1/32")
	if err := os.WriteFile(cfg.WireGuardFile, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	m, r := workerManagerFixture(t)
	if _, err := m.Join(cfg); err == nil || !strings.Contains(err.Error(), "management subnet") {
		t.Fatalf("VPN address claimed management subnet: %v", err)
	}
	if len(r.transfers) != 0 || !reflect.DeepEqual(r.removed, []string{r.opts.Name}) {
		t.Fatal("overlap was checked after provisioning or escaped cleanup scope")
	}
}

func TestExternalWorkerPostStartupTimeoutRecoversOriginalIdentity(t *testing.T) {
	cfg, _, _ := workerFixture(t)
	cfg.WaitTimeout = time.Millisecond
	m, r := workerManagerFixture(t)
	r.failObserve = true
	if _, err := m.Join(cfg); err == nil || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("timeout did not retain identity: %v", err)
	}
	state, err := m.read(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "joining" || state.NodeUID != "" || len(r.removed) != 0 {
		t.Fatalf("incomplete join lost identity: %+v removed=%v", state, r.removed)
	}
	transfers := len(r.transfers)
	name := r.opts.Name
	// Retry needs neither the original host files nor another VM/bootstrap.
	if err := os.Remove(cfg.TokenFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.WireGuardFile); err != nil {
		t.Fatal(err)
	}
	r.failObserve = false
	if err := m.Start(cfg.Name, time.Second); err != nil {
		t.Fatal(err)
	}
	ready, err := m.read(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Phase != "ready" || ready.NodeUID != "fixture-node-uid" || ready.OwnerID != state.OwnerID || ready.VMName != name || len(r.transfers) != transfers {
		t.Fatalf("recovery rejoined instead of resuming: %+v", ready)
	}
	// A deliberate operator untaint is respected after the first successful join.
	r.noTaint = true
	if err := m.Start(cfg.Name, time.Second); err != nil {
		t.Fatalf("ready worker required operator taint to be restored: %v", err)
	}
}

func TestExternalWorkerReadinessRequiresCurrentGuestBoot(t *testing.T) {
	cfg, _, _ := workerFixture(t)
	m, r := workerManagerFixture(t)
	state, err := m.Join(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.rows = []runtime.Info{{Name: state.VMName, Status: "running", IP: r.management}}
	r.staleBootReports = 1
	workers, err := m.Workers()
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Ready {
		t.Fatal("get workers reported old-boot Ready as current")
	}
	r.staleBootReports = 1
	before := r.observations
	if err := m.Start(cfg.Name, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if r.observations-before != 2 {
		t.Fatal("start did not wait for current boot Node report")
	}
	ready, err := m.read(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if ready.NodeUID != state.NodeUID {
		t.Fatal("boot freshness changed remote Node identity")
	}
}

func TestExternalWorkerInventoryRejectsTampering(t *testing.T) {
	cfg, _, _ := workerFixture(t)
	m, r := workerManagerFixture(t)
	s, err := m.Join(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "0", strings.Repeat("a", 30), strings.Repeat("A", 32)} {
		tampered := s
		tampered.OwnerID = id
		if err := m.write(tampered); err != nil {
			t.Fatal(err)
		}
		if err := m.Delete(cfg.Name); err == nil {
			t.Fatal("tampered inventory accepted")
		}
	}
	if len(r.removed) != 0 {
		t.Fatal("tampered inventory removed VM")
	}
}

func TestExternalWorkerPrivateArchiveUsesFixedPathsAndTokenFile(t *testing.T) {
	cfg, token, key := workerFixture(t)
	p, err := prepareExternalWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newExternalWorkerState(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := externalWorkerArchive(s, p)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "/") || h.Typeflag != tar.TypeReg {
			t.Fatal("unsafe tar path")
		}
		content, _ := io.ReadAll(tr)
		files[h.Name] = string(content)
		want := int64(0o600)
		if h.Name == "network.sh" {
			want = 0o700
		}
		if h.Mode != want {
			t.Fatalf("mode of %s = %o", h.Name, h.Mode)
		}
	}
	if len(files) != 7 || strings.TrimSpace(files["token"]) != token || !strings.Contains(files["wireguard.conf"], key) {
		t.Fatal("private archive omitted credentials")
	}
	var agent map[string]any
	if err := yaml.Unmarshal([]byte(files["k3s.yaml"]), &agent); err != nil {
		t.Fatal(err)
	}
	if agent["token"] != nil || agent["token-file"] != externalWorkerGuestDir+"/token" || agent["flannel-iface"] != externalWireGuardInterface || agent["node-ip"] != cfg.NodeIP {
		t.Fatalf("bad agent config: %v", agent)
	}
	if !reflect.DeepEqual(agent["node-taint"], []any{"kiac.dev/external-worker=true:NoSchedule"}) {
		t.Fatalf("default scheduling taint missing: %v", agent["node-taint"])
	}
	if !strings.Contains(files["k3s.service"], "Requires=kiac-worker-network.service") {
		t.Fatal("agent not gated by VPN")
	}
}

func TestExternalWorkerGeneratedNetworkScriptCleansFailedRoute(t *testing.T) {
	cfg, _, _ := workerFixture(t)
	p, err := prepareExternalWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			script := filepath.Join(dir, "network.sh")
			if err := os.WriteFile(script, []byte(externalWorkerNetworkScript(p.wg)), 0o700); err != nil {
				t.Fatal(err)
			}
			fake := `#!/bin/sh
printf '%s %s\n' "$(basename "$0")" "$*" >> "$CALLS"
if [ "$FAIL_ROUTE" = true ] && [ "$1 $2 $3" = '-4 route add' ]; then exit 1; fi
`
			for _, name := range []string{"ip", "wg"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(fake), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", script, "up")
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "CALLS="+log, "FAIL_ROUTE="+fmt.Sprint(fail))
			out, err := cmd.CombinedOutput()
			if (err != nil) != fail {
				t.Fatalf("network result=%v %s", err, out)
			}
			calls, _ := os.ReadFile(log)
			if strings.Contains(string(calls), "link delete kiacwg0") != fail {
				t.Fatalf("failure cleanup wrong: %s", calls)
			}
			if !fail && !strings.Contains(string(calls), "ip -4 route add 10.43.0.0/16 dev kiacwg0") {
				t.Fatalf("service route missing: %s", calls)
			}
		})
	}
}

func TestExternalWorkerObservationExecutesWithOwnNodeCredentials(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cfg, _, _ := workerFixture(t)
	p, err := prepareExternalWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newExternalWorkerState(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "k3s")
	calls := filepath.Join(dir, "calls")
	flannel := filepath.Join(dir, "flannel.json")
	node := map[string]any{"metadata": map[string]any{"name": state.VMName, "uid": "fixture-node-uid", "labels": map[string]string{"kiac.dev/worker-owner": state.OwnerID}}, "status": map[string]any{"addresses": []map[string]string{{"type": "InternalIP", "address": state.NodeIP}}, "nodeInfo": map[string]string{"kubeletVersion": "v1.37.0+k3s1", "bootID": "current-boot"}, "conditions": []map[string]string{{"type": "Ready", "status": "True"}}}}
	node["spec"] = map[string]any{"taints": []map[string]string{{"key": "kiac.dev/external-worker", "value": "true", "effect": "NoSchedule"}}}
	nodeJSON, _ := json.Marshal(node)
	nodePath := filepath.Join(dir, "node.json")
	if err := os.WriteFile(nodePath, nodeJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flannel, []byte(`{"Network":"10.42.0.0/16","EnableIPv4":true,"EnableIPv6":false,"Backend":{"Type":"vxlan"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
printf '%s\n' "$*" >> "$WORKER_CALLS"
case "$*" in *'/version') printf '%s\n' '{"gitVersion":"v1.37.0+k3s1"}';; *) cat "$WORKER_NODE";; esac
`
	if err := os.WriteFile(fake, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(externalWorkerObserveScript(state), "/usr/local/bin/k3s", fake)
	script = strings.ReplaceAll(script, "/var/lib/rancher/k3s/agent/etc/flannel/net-conf.json", flannel)
	bootPath := filepath.Join(dir, "boot_id")
	if err := os.WriteFile(bootPath, []byte("current-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, "/proc/sys/kernel/random/boot_id", bootPath)
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(), "WORKER_CALLS="+calls, "WORKER_NODE="+nodePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("observer: %v %s", err, out)
	}
	var observation workerObservation
	if err := json.Unmarshal(out, &observation); err != nil {
		t.Fatal(err)
	}
	if observation.Name != state.VMName || observation.OwnerID != state.OwnerID || observation.NodeIP != state.NodeIP || !observation.Ready || !observation.Tainted || !observation.IPv4Only || observation.PodNetwork != "10.42.0.0/16" || observation.Backend != "vxlan" || observation.GuestBootID != "current-boot" || observation.NodeBootID != "current-boot" {
		t.Fatalf("wrong observation: %+v", observation)
	}
	rawCalls, _ := os.ReadFile(calls)
	if strings.Count(string(rawCalls), "--kubeconfig /var/lib/rancher/k3s/agent/kubelet.kubeconfig --request-timeout=8s") != 2 || !strings.Contains(string(rawCalls), "get node "+state.VMName+" -o json") {
		t.Fatalf("observer borrowed wrong identity: %s", rawCalls)
	}
}

func TestExternalWorkerLogRedactionRunsBeforeOutput(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cfg, token, key := workerFixture(t)
	dir := t.TempDir()
	journal := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(journal, []byte("#!/bin/sh\nprintf '%s\\n' \"$PRIVATE_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(externalWorkerLogsScript, "/etc/kiac/external-worker/token", cfg.TokenFile)
	script = strings.ReplaceAll(script, "/etc/kiac/external-worker/wireguard.conf", cfg.WireGuardFile)
	psk := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	wg, _ := os.ReadFile(cfg.WireGuardFile)
	if err := os.WriteFile(cfg.WireGuardFile, append(wg, []byte("PresharedKey = "+psk+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, credentials := range []string{"node:private-fixture-token", "server:private-fixture-token", "abcdef.0123456789abcdef"} {
		secret := "private-fixture-token"
		if strings.Contains(credentials, ".") {
			secret = "0123456789abcdef"
		}
		fullToken := token[:69] + credentials
		if err := os.WriteFile(cfg.TokenFile, []byte(fullToken), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "PRIVATE_LOG=network ready "+fullToken+" "+credentials+" authentication password="+secret+" PrivateKey="+key+" PresharedKey="+psk)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("logs: %v %s", err, out)
		}
		for _, value := range []string{fullToken, credentials, secret, key, psk} {
			if strings.Contains(string(out), value) {
				t.Fatal("guest log redaction leaked a saved credential component")
			}
		}
		if !strings.Contains(string(out), "network ready") {
			t.Fatalf("lost nonsecret diagnostics: %s", out)
		}
	}
	if err := os.Remove(cfg.TokenFile); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", script)
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "network ready") {
		t.Fatal("logs retrieved without private redaction inputs")
	}
}
