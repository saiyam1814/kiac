package cluster

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestRedactSupportText(t *testing.T) {
	raw := `/Users/test/kiac
token: secret-token
"password": "hunter2"
client-key-data: c2VjcmV0
Authorization: Bearer bearer-secret
command --token=flag-secret
K10abcdef::server:k3s-secret
tunnel_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
https://alice:url-secret@example.test/path
-----BEGIN PRIVATE KEY-----
private-secret
-----END PRIVATE KEY-----
$HOME/.kiac/gpu-nodes/id_ed25519
IdentityFile /Users/test/.kiac/gpu-nodes/id_ed25519
ordinary diagnostic text
`
	got := redactSupportText(raw, "/Users/test")
	for _, secret := range []string{"secret-token", "hunter2", "c2VjcmV0", "bearer-secret", "flag-secret", "k3s-secret", strings.Repeat("a", 64), "url-secret", "private-secret", "id_ed25519", "/Users/test"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted output still contains %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"$HOME/kiac", "[REDACTED]", "ordinary diagnostic text"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted output missing %q:\n%s", want, got)
		}
	}
}

func TestSupportHasGPU(t *testing.T) {
	without := []runtime.Info{{Name: "kiac-dev-worker-1", Backend: runtime.BackendContainer}}
	with := []runtime.Info{{Name: "kiac-dev-gpu-1", Backend: runtime.BackendKrunkit, GPU: true}}
	if supportHasGPU(without) || !supportHasGPU(with) {
		t.Fatal("GPU support-bundle gating is incorrect")
	}
}

func TestWriteSupportArchive(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	files := []supportFile{{name: "z.txt", data: []byte("last\n")}, {name: "a/info.txt", data: []byte("first\n")}}
	if err := writeSupportArchive(out, "kiac-support-dev", now, files); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("archive mode = %o, want 600", got)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	contents := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		contents[header.Name] = string(raw)
		if header.Mode != 0o600 {
			t.Errorf("entry %s mode = %o, want 600", header.Name, header.Mode)
		}
	}
	wantNames := []string{"kiac-support-dev/a/info.txt", "kiac-support-dev/z.txt"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Errorf("archive names = %v, want %v", names, wantNames)
	}
	if contents[wantNames[0]] != "first\n" || contents[wantNames[1]] != "last\n" {
		t.Errorf("archive contents = %#v", contents)
	}
	if err := writeSupportArchive(out, "kiac-support-dev", now, files); err == nil {
		t.Fatal("archive writer overwrote an existing bundle")
	}
}

func TestSupportCollectorBoundsAndRejectsUnsafePath(t *testing.T) {
	c := &supportCollector{}
	c.addText("../secret.txt", "must not be added")
	c.addText("logs/large.txt", strings.Repeat("\u00e9", maxSupportFileBytes))
	if len(c.files) != 1 || c.files[0].name != "logs/large.txt" {
		t.Fatalf("collector files = %+v", c.files)
	}
	if len(c.files[0].data) > maxSupportFileBytes {
		t.Errorf("bounded file has %d bytes, limit %d", len(c.files[0].data), maxSupportFileBytes)
	}
	if !utf8.Valid(c.files[0].data) {
		t.Fatal("truncation split a UTF-8 rune")
	}
	if len(c.warnings) == 0 {
		t.Fatal("unsafe path did not produce a warning")
	}
}

func TestSupportCollectsEdgeProxyLogsForRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		distro  string
		backend string
		want    string
	}{
		{"kubeadm container", "kubeadm", runtime.BackendContainer, "SYSTEMD_EDGE_LOG"},
		{"kubeadm krunkit", "kubeadm", runtime.BackendKrunkit, "SYSTEMD_EDGE_LOG"},
		{"k3s container", "k3s", runtime.BackendContainer, "FILE_EDGE_LOG"},
		{"k3s krunkit", "k3s", runtime.BackendKrunkit, "SYSTEMD_EDGE_LOG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "container")
			script := `#!/bin/sh
case "$*" in
  *"journalctl --no-pager -n 500 -u kiac-edge-proxy"*) printf 'SYSTEMD_EDGE_LOG\n' ;;
  *"tail -500 /var/log/kiac-edge-proxy.log"*) printf 'FILE_EDGE_LOG\n' ;;
esac
`
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			collector := &supportCollector{}
			collector.collectNodes(&runtime.Client{Bin: bin}, []runtime.Info{{
				Name: "kiac-dev-worker-1", Status: "running", Backend: tc.backend,
			}}, tc.distro, time.Second)
			for _, file := range collector.files {
				if strings.HasSuffix(file.name, "/edge-proxy.log") {
					if !strings.Contains(string(file.data), tc.want) {
						t.Fatalf("expected %s in edge-proxy log, got:\n%s", tc.want, file.data)
					}
					return
				}
			}
			t.Fatal("edge-proxy log was not collected")
		})
	}
}

func TestSupportOutputPath(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 8, 12, 34, 56, 0, time.UTC)
	got, err := supportOutputPath("dev", now, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "kiac-support-dev-20260808T123456Z.tar.gz")
	if got != want {
		t.Errorf("output path = %q, want %q", got, want)
	}
}

func TestSupportCollectsPodNetworkLogs(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
case "$*" in
  *"get daemonset kube-flannel-ds -n kube-flannel"*) printf '{"status":{"desiredNumberScheduled":3,"numberReady":3}}\n' ;;
  *"get daemonsets,pods -n kube-flannel -l app=flannel"*) printf 'FLANNEL_PODS\n' ;;
  *"logs -n kube-flannel daemonset/kube-flannel-ds --all-pods=true --all-containers=true"*) printf 'FLANNEL_LOG\n' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	report := VerificationReport{}
	report.add(VerificationPass, "kubernetes.api", "Kubernetes API", "ok", "")
	collector := &supportCollector{}
	collector.collectKubernetes(&Manager{rt: &runtime.Client{Bin: bin}}, "kiac-dev-control-plane", "kubeadm", report, time.Second, false)
	got := map[string]string{}
	for _, file := range collector.files {
		got[file.name] = string(file.data)
	}
	if !strings.Contains(got["kubernetes/cni-flannel.txt"], "FLANNEL_PODS") {
		t.Fatalf("flannel pod listing not collected: %q", got["kubernetes/cni-flannel.txt"])
	}
	if !strings.Contains(got["kubernetes/cni-flannel.log"], "FLANNEL_LOG") {
		t.Fatalf("flannel logs not collected: %q", got["kubernetes/cni-flannel.log"])
	}
	for name := range got {
		if strings.HasPrefix(name, "kubernetes/cni-kindnet") || strings.HasPrefix(name, "kubernetes/cni-cilium") {
			t.Fatalf("collected %s for a flannel cluster", name)
		}
	}
}
