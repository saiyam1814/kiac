package cluster

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func registryTestCA(t *testing.T, isCA bool, usage ...x509.KeyUsage) []byte {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Kiac test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if len(usage) > 0 {
		cert.KeyUsage = usage[0]
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func registryTestConfig(t *testing.T) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, registryTestCA(t, true), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{RegistryCAs: RegistryCAs{"registry.example.test:5443": path}, WaitTimeout: time.Second}
}

func TestRegistryHostValidation(t *testing.T) {
	for _, host := range []string{"registry.example.test:5443", "localhost:5000", "127.0.0.1:5000", "[::1]:5000", "registry.example.test"} {
		if err := validateRegistryHost(host); err != nil {
			t.Errorf("%q: %v", host, err)
		}
	}
	for _, host := range []string{"", "https://registry.test", "registry.test/path", "../evil", "_default", "a..b", "a:0", "a:65536", "a:", "a:b", "a?x", "a#x", "user@a", "-bad.test", "a\nb", "UPPER.test", "[::1%lo0]:5000", "a\\b"} {
		if err := validateRegistryHost(host); err == nil {
			t.Errorf("accepted %q", host)
		}
	}
}

func TestRegistryCARejectsBadInputBeforeProvisioning(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("garbage"), registryTestCA(t, false), registryTestCA(t, true, x509.KeyUsageDigitalSignature), []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"), append(registryTestCA(t, true), []byte("trailing garbage")...), bytes.Repeat([]byte("a"), 1024*1024+1)} {
		path := filepath.Join(t.TempDir(), "bad.pem")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		cfg := Config{RegistryCAs: RegistryCAs{"registry.test": path}, GPUWorkers: 1}
		m := &Manager{} // nil backends prove validation does not contact runtime.
		if err := m.Create(cfg); err == nil {
			t.Fatal("kubeadm accepted invalid CA")
		}
		if err := m.CreateK3s(cfg); err == nil {
			t.Fatal("K3s accepted invalid CA")
		}
	}
}

func TestRegistryCAConfigAndFlagPrecedence(t *testing.T) {
	var flags RegistryCAs
	if err := flags.Set("registry.test:5000=/tmp/a=b.pem"); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("registry.test:5000=other.pem"); err == nil {
		t.Fatal("duplicate allowed")
	}
	fc := FileConfig{RegistryCAs: RegistryCAs{"registry.test": "/tmp/file.pem"}}
	var distro, version string
	cfg := Config{RegistryCAs: flags}
	if err := fc.Merge(&cfg, &distro, &version, func(s string) bool { return s == "registry-ca" }); err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryCAs["registry.test:5000"] != "/tmp/a=b.pem" {
		t.Fatal("CLI did not override YAML")
	}
	if err := fc.Merge(&cfg, &distro, &version, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	fc.RegistryCAs["registry.test"] = "changed"
	if cfg.RegistryCAs["registry.test"] != "/tmp/file.pem" {
		t.Fatal("config map aliased")
	}
}

type registryRuntime struct {
	runtime.HostRuntime
	config string
	files  map[string][]byte
	script string
	nodes  []string
}

func (r *registryRuntime) ExecTimeout(node string, timeout time.Duration, args ...string) (string, error) {
	if timeout <= 0 {
		return "", fmt.Errorf("unbounded deadline")
	}
	if len(args) == 1 && args[0] == "true" {
		return "", nil
	}
	if len(args) == 3 && args[2] == containerdRegistryDumpScript {
		return r.config, nil
	}
	if strings.Join(args, " ") == "cat /etc/containerd/config.toml" {
		return r.config, nil
	}
	return "", fmt.Errorf("unexpected command %v", args)
}

func (r *registryRuntime) ExecStdinTimeout(node string, timeout time.Duration, in io.Reader, args ...string) error {
	if timeout <= 0 {
		return fmt.Errorf("unbounded transfer")
	}
	r.nodes = append(r.nodes, node)
	r.script = args[len(args)-1]
	r.files = map[string][]byte{}
	tr := tar.NewReader(in)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		r.files[h.Name], err = io.ReadAll(tr)
		if err != nil {
			return err
		}
	}
	if config, ok := r.files["etc/containerd/config.toml"]; ok {
		r.config = string(config)
	}
	return nil
}

func TestRegistryTrustUsesScopedSnapshotForBothDistros(t *testing.T) {
	for _, distro := range []string{"kubeadm", "k3s"} {
		t.Run(distro, func(t *testing.T) {
			cfg := registryTestConfig(t)
			if err := cfg.PrepareRegistryCAs(); err != nil {
				t.Fatal(err)
			}
			ca := cfg.registryCAs[0]
			if err := os.WriteFile(cfg.RegistryCAs[ca.host], []byte("changed after validation"), 0600); err != nil {
				t.Fatal(err)
			}
			rt := &registryRuntime{config: "version = 3\n[plugins.'io.containerd.cri.v1.images'.registry]\n config_path = ''\n"}
			m := &Manager{rt: rt}
			for _, node := range []string{"control-plane", "worker-1", "gpu-1"} {
				rt.config = "version = 3\n[plugins.'io.containerd.cri.v1.images'.registry]\n config_path = ''\n"
				if err := m.installRegistryTrust(node, distro, cfg); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(rt.files[strings.TrimPrefix(ca.path, "/")], ca.pem) {
					t.Fatal("CA snapshot changed between nodes")
				}
				if strings.Contains(rt.script, "skip_verify") || strings.Contains(rt.script, "insecure") {
					t.Fatal("TLS bypass configured")
				}
				if distro == "kubeadm" {
					trust := string(rt.files["etc/containerd/certs.d/"+ca.host+"/hosts.toml"])
					if !strings.Contains(trust, "https://"+ca.host) || !strings.Contains(trust, ca.path) {
						t.Fatal("unscoped or missing CA")
					}
					if !strings.Contains(rt.script, "systemctl restart containerd") {
						t.Fatal("daemon config not reloaded")
					}
					for _, namespace := range []string{ca.host, "registry.example.test_5443_"} {
						path := shQuote("/etc/containerd/certs.d/" + namespace)
						if !strings.Contains(rt.script, "test ! -e "+path+" && test ! -L "+path) {
							t.Fatalf("namespace %s can hide a dangling configuration symlink", namespace)
						}
					}
				} else {
					trust := string(rt.files["etc/rancher/k3s/registries.yaml"])
					if !strings.Contains(trust, ca.host) || !strings.Contains(trust, ca.path) {
						t.Fatal("K3s trust missing")
					}
					if !strings.HasSuffix(rt.script, ": > "+registryReadyPath+"\n") {
						t.Fatal("startup marker not last")
					}
				}
			}
			if len(rt.nodes) != 3 {
				t.Fatal("not every node configured")
			}
		})
	}
}

func TestRegistryContainerdConfigurationPreservesOtherSettings(t *testing.T) {
	for _, plugin := range []string{"io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images"} {
		version := 3
		if strings.Contains(plugin, "grpc") {
			version = 2
		}
		for _, quotes := range []string{"'", `"`} {
			input := fmt.Sprintf("version = %d\n[plugins.%s%s%s.registry]\n  config_path = ''\n[other]\n keep = true\n", version, quotes, plugin, quotes)
			out, err := containerdRegistryConfig(input)
			if err != nil || !strings.Contains(out, "/etc/containerd/certs.d") || !strings.Contains(out, "keep = true") {
				t.Fatalf("%s: %v", out, err)
			}
			unchanged, err := containerdRegistryConfig(out)
			if err != nil || unchanged != out {
				t.Fatal("configuration not idempotent")
			}
			if _, err := containerdRegistryConfig(strings.Replace(input, "config_path = ''", `config_path = "/custom"`, 1)); err == nil {
				t.Fatal("overwrote custom path")
			}
		}
	}
	if _, err := containerdRegistryConfig("version = 3\n"); err == nil {
		t.Fatal("unknown CRI layout accepted")
	}
}

func TestRegistryK3sBootWaitsBeforeStartingAndPersists(t *testing.T) {
	cfg := registryTestConfig(t)
	_, args := k3sBoot(cfg, []string{"server"})
	if strings.Index(args[1], registryReadyPath) < 0 || strings.Index(args[1], registryReadyPath) > strings.Index(args[1], "exec sh") {
		t.Fatal("K3s can start before registry configuration")
	}
	if strings.Contains(args[1], cfg.RegistryCAs["registry.example.test:5443"]) {
		t.Fatal("host CA path leaked into persistent boot args")
	}
	if registryBootGate(Config{}) != "" {
		t.Fatal("default clusters changed")
	}
}

func TestRegistryRefusesNonRegularCAWithoutOpeningIt(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "ca.pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, t.TempDir()} {
		cfg := Config{RegistryCAs: RegistryCAs{"registry.test": path}}
		if err := cfg.PrepareRegistryCAs(); err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func TestRegistryContainerdActivePluginAndLegacyConflicts(t *testing.T) {
	old := "io.containerd.grpc.v1.cri"
	active := "io.containerd.cri.v1.images"
	mixed := fmt.Sprintf("version = 3\n[plugins.%q.registry]\nconfig_path = '/etc/containerd/certs.d'\n[plugins.%q.registry]\nconfig_path = '/custom'\n", old, active)
	if _, err := containerdRegistryConfig(mixed); err == nil {
		t.Fatal("stale plugin hid active custom path")
	}
	for _, v := range []int{2, 3, 4} {
		plugin := active
		if v == 2 {
			plugin = old
		}
		missing := fmt.Sprintf("version = %d\n[plugins.%q]\nsandbox_image = 'keep-me'\n", v, plugin)
		out, err := containerdRegistryConfig(missing)
		if err != nil || !strings.Contains(out, "keep-me") || !strings.Contains(out, "config_path") {
			t.Fatalf("missing table: %s %v", out, err)
		}
		for _, key := range []string{"mirrors", "configs", "auths"} {
			populated := fmt.Sprintf("version = %d\n[plugins.%q.registry.%s]\nregistry = { endpoint = 'https://custom' }\n", v, plugin, key)
			if _, err := containerdRegistryConfig(populated); err == nil {
				t.Fatalf("overrode legacy %s", key)
			}
			empty := fmt.Sprintf("version = %d\n[plugins.%q.registry.%s]\n", v, plugin, key)
			if _, err := containerdRegistryConfig(empty); err != nil {
				t.Fatalf("empty default %s: %v", key, err)
			}
		}
	}
}

func TestRegistryContainerdDumpSeparatesWarningsAndKeepsErrors(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "containerd")
	for _, failure := range []bool{false, true} {
		body := "#!/bin/sh\necho migration-warning >&2\nprintf 'version = 3\\n'\n"
		if failure {
			body += "echo configuration-failed >&2\nexit 1\n"
		}
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-ec", containerdRegistryDumpScript)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if failure {
			if err == nil || !strings.Contains(string(out), "configuration-failed") {
				t.Fatalf("lost config error: %s %v", out, err)
			}
		} else if err != nil || string(out) != "version = 3\n" {
			t.Fatalf("warning polluted TOML: %q %v", out, err)
		}
	}
}

func TestRegistryRejectsK3sRuntimeOverrides(t *testing.T) {
	for _, arg := range []string{"--private-registry=/other.yaml", "-private-registry", "--container-runtime-endpoint=unix:///other.sock", "-image-service-endpoint", "--docker", "--config=/other.yaml", "-c"} {
		for _, server := range []bool{true, false} {
			cfg := registryTestConfig(t)
			if server {
				cfg.K3sControlPlaneArgs = []string{arg}
			} else {
				cfg.K3sWorkerArgs = []string{arg}
			}
			if err := cfg.PrepareRegistryCAs(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("accepted override %s", arg)
			}
		}
	}
	cfg := registryTestConfig(t)
	cfg.K3sControlPlaneArgs = []string{"--tls-san", "api.test"}
	if err := cfg.PrepareRegistryCAs(); err != nil {
		t.Fatalf("unrelated K3s arg rejected: %v", err)
	}
}

func TestRegistryDockerHubKeepsCanonicalEndpoint(t *testing.T) {
	cfg := registryTestConfig(t)
	path := cfg.RegistryCAs["registry.example.test:5443"]
	cfg.RegistryCAs = RegistryCAs{"docker.io": path}
	if err := cfg.PrepareRegistryCAs(); err != nil {
		t.Fatal(err)
	}
	rt := &registryRuntime{config: "version = 3\n[plugins.'io.containerd.cri.v1.images'.registry]\nconfig_path = '/etc/containerd/certs.d'\n"}
	if err := (&Manager{rt: rt}).installRegistryTrust("node", "kubeadm", cfg); err != nil {
		t.Fatal(err)
	}
	trust := string(rt.files["etc/containerd/certs.d/docker.io/hosts.toml"])
	if !strings.Contains(trust, "https://registry-1.docker.io") || strings.Contains(trust, "https://docker.io") {
		t.Fatalf("noncanonical endpoint: %s", trust)
	}
	if strings.Contains(rt.script, "systemctl restart") {
		t.Fatal("restarted daemon without a config change")
	}
}

type registryReviewEffectiveRuntime struct {
	registryRuntime
	before, after string
	uploaded      bool
}

func (r *registryReviewEffectiveRuntime) ExecTimeout(node string, timeout time.Duration, args ...string) (string, error) {
	if len(args) == 3 && args[2] == containerdRegistryDumpScript {
		if r.uploaded {
			return r.after, nil
		}
		return r.before, nil
	}
	return r.registryRuntime.ExecTimeout(node, timeout, args...)
}

func (r *registryReviewEffectiveRuntime) ExecStdinTimeout(node string, timeout time.Duration, in io.Reader, args ...string) error {
	if err := r.registryRuntime.ExecStdinTimeout(node, timeout, in, args...); err != nil {
		return err
	}
	r.uploaded = true
	return nil
}

func TestRegistryReviewRawEffectiveVersionsAndImportOverrides(t *testing.T) {
	raw := "version = 2\nimports = ['/etc/containerd/conf.d/*.toml']\n[plugins.'io.containerd.grpc.v1.cri']\nsandbox_image = 'preserve-me'\n"
	effective := func(path string) string {
		return fmt.Sprintf("version = 4\nimports = ['/etc/containerd/conf.d/*.toml']\n[plugins.'io.containerd.cri.v1.images'.registry]\nconfig_path = %q\n", path)
	}
	for _, tc := range []struct {
		name, before, after, wantError string
		wantUpload                     bool
	}{
		{"raw v2 effective v4", effective(""), effective("/etc/containerd/certs.d"), "", true},
		{"existing import custom path", effective("/custom"), effective("/custom"), "custom registry config_path", false},
		{"import prevents activation", effective(""), effective(""), "did not activate registry trust", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := registryTestConfig(t)
			if err := cfg.PrepareRegistryCAs(); err != nil {
				t.Fatal(err)
			}
			rt := &registryReviewEffectiveRuntime{registryRuntime: registryRuntime{config: raw}, before: tc.before, after: tc.after}
			err := (&Manager{rt: rt}).installRegistryTrust("review-node", "kubeadm", cfg)
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error = %v, want %s", err, tc.wantError)
			}
			if rt.uploaded != tc.wantUpload {
				t.Fatalf("uploaded=%v, want %v", rt.uploaded, tc.wantUpload)
			}
			if rt.uploaded {
				got := string(rt.files["etc/containerd/config.toml"])
				for _, want := range []string{"version = 2", "/etc/containerd/conf.d/*.toml", "preserve-me", "/etc/containerd/certs.d"} {
					if !strings.Contains(got, want) {
						t.Fatalf("raw setting %q lost: %s", want, got)
					}
				}
				if strings.Contains(got, "version = 4") {
					t.Fatal("effective config replaced raw version")
				}
				if !strings.Contains(rt.script, "mv -f") {
					t.Fatal("config replacement is not atomic")
				}
			}
		})
	}
}

func TestRegistryReviewV4RawConfiguration(t *testing.T) {
	input := "version = 4\nimports = ['/etc/containerd/conf.d/*.toml']\n[plugins.'io.containerd.cri.v1.images']\nsnapshotter = 'overlayfs'\n"
	out, err := containerdRegistryConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"version = 4", "/etc/containerd/conf.d/*.toml", "overlayfs", "/etc/containerd/certs.d"} {
		if !strings.Contains(out, want) {
			t.Fatalf("setting %q lost: %s", want, out)
		}
	}
	checked, err := containerdRegistryConfig(out)
	if err != nil || checked != out {
		t.Fatalf("v4 config is not idempotent: %v", err)
	}
}

func TestRegistryK3sRejectsConfigurationMountsBeforeProvisioning(t *testing.T) {
	for _, target := range []string{
		"/", "/etc", "/etc/", "/etc/rancher", "/etc/rancher/k3s",
		"/etc/rancher/k3s/config.yaml.d", "/etc/rancher/k3s/data",
		"/etc/rancher/k3s/../k3s/config.yaml.d", "//etc//rancher/k3s/",
	} {
		t.Run(target, func(t *testing.T) {
			cfg := registryTestConfig(t)
			// The direct creation methods must set their distro even when
			// callers omit it, before accessing any backend or create asset.
			cfg.Mounts = runtime.Mounts{{Source: t.TempDir(), Target: target}}
			for _, create := range []func(Config) error{(&Manager{}).CreateK3s, (&Manager{}).createK3sGPU} {
				err := create(cfg)
				if err == nil || !strings.Contains(err.Error(), "K3s mount") {
					t.Fatalf("target %q: error = %v, want early configuration-mount rejection", target, err)
				}
			}
		})
	}
}

func TestRegistryConfigurationMountRestrictionScope(t *testing.T) {
	for _, tc := range []struct {
		name, distro, target string
		withCA               bool
	}{
		{"K3s project mount", "k3s", "/workspace", true},
		{"K3s sibling directory", "k3s", "/etc/rancher/k3s-backup", true},
		{"kubeadm configuration mount", "kubeadm", "/etc/rancher/k3s", true},
		{"K3s without registry CA", "k3s", "/etc/rancher/k3s", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := registryTestConfig(t)
			cfg.Distro = tc.distro
			cfg.Mounts = runtime.Mounts{{Source: t.TempDir(), Target: tc.target}}
			if !tc.withCA {
				cfg.RegistryCAs = nil
			}
			if err := cfg.PrepareRegistryCAs(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Cached certificate snapshots must not bypass a newly added mount check.
	cfg := registryTestConfig(t)
	cfg.Distro = "k3s"
	if err := cfg.PrepareRegistryCAs(); err != nil {
		t.Fatal(err)
	}
	cfg.Mounts = runtime.Mounts{{Source: t.TempDir(), Target: "/etc"}}
	if err := cfg.PrepareRegistryCAs(); err == nil {
		t.Fatal("prepared snapshot bypassed configuration-mount rejection")
	}
}

// Execute the actual generated archive-install script against a temporary guest
// tree. Only absolute guest destinations are remapped; archive member paths and
// their $tmp sources stay unchanged, so precondition/write ordering is tested.
type registryExecutingRuntime struct {
	registryRuntime
	root string
}

func (r *registryExecutingRuntime) ExecStdinTimeout(node string, timeout time.Duration, in io.Reader, args ...string) error {
	payload, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if err := r.registryRuntime.ExecStdinTimeout(node, timeout, bytes.NewReader(payload), args...); err != nil {
		return err
	}
	script := strings.ReplaceAll(r.script, "$tmp/etc/", "$tmp/__KIAC_ARCHIVE_ETC__/")
	script = strings.ReplaceAll(script, "/etc/", r.root+"/etc/")
	script = strings.ReplaceAll(script, "$tmp/__KIAC_ARCHIVE_ETC__/", "$tmp/etc/")
	script = strings.ReplaceAll(script, "/tmp/kiac-registry.", r.root+"/tmp/kiac-registry.")
	command := exec.Command("sh", "-ec", script)
	command.Stdin = bytes.NewReader(payload)
	out, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("guest install script: %w\n%s", err, out)
	}
	return nil
}

func TestRegistryK3sExistingConfigCannotOpenGateOrWriteTrust(t *testing.T) {
	for _, tc := range []struct {
		name, configPath, wantError string
		symlink                     bool
	}{
		{name: "clean configuration"},
		{name: "default configuration", configPath: "etc/rancher/k3s/config.yaml"},
		{name: "runtime override drop-in", configPath: "etc/rancher/k3s/config.yaml.d/10-runtime.yaml"},
		{name: "dangling default config symlink", configPath: "etc/rancher/k3s/config.yaml", symlink: true},
		{name: "dangling drop-in symlink", configPath: "etc/rancher/k3s/config.yaml.d/10-runtime.yaml", symlink: true},
		{name: "dangling registries symlink", configPath: "etc/rancher/k3s/registries.yaml", symlink: true, wantError: "registries.yaml already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := registryTestConfig(t)
			cfg.Distro = "k3s"
			if err := cfg.PrepareRegistryCAs(); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			for _, dir := range []string{"tmp", "etc/rancher/k3s"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.configPath != "" {
				config := filepath.Join(root, tc.configPath)
				if err := os.MkdirAll(filepath.Dir(config), 0755); err != nil {
					t.Fatal(err)
				}
				if tc.symlink {
					if err := os.Symlink(filepath.Join(root, "missing-config"), config); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(config, []byte("private-registry: /custom/registries.yaml\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			rt := &registryExecutingRuntime{root: root}
			err := (&Manager{rt: rt}).installRegistryTrust("node", "k3s", cfg)
			wantRejection := tc.configPath != ""
			if wantRejection {
				wantError := tc.wantError
				if wantError == "" {
					wantError = "existing K3s config.yaml or YAML drop-in conflicts"
				}
				if err == nil || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("error = %v, want existing-config rejection", err)
				}
				for _, path := range []string{cfg.registryCAs[0].path, "/etc/rancher/k3s/registries.yaml", registryReadyPath} {
					if strings.TrimPrefix(path, "/") == tc.configPath {
						continue // This preexisting path must be preserved below.
					}
					if _, err := os.Lstat(filepath.Join(root, strings.TrimPrefix(path, "/"))); !os.IsNotExist(err) {
						t.Fatalf("rejected configuration wrote %s: %v", path, err)
					}
				}
				if _, err := os.Lstat(filepath.Join(root, tc.configPath)); err != nil {
					t.Fatalf("existing configuration was changed: %v", err)
				}
				if tc.symlink {
					if target, err := os.Readlink(filepath.Join(root, tc.configPath)); err != nil || target != filepath.Join(root, "missing-config") {
						t.Fatalf("existing configuration symlink was overwritten: target=%q, error=%v", target, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ca, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(cfg.registryCAs[0].path, "/")))
			if err != nil || !bytes.Equal(ca, cfg.registryCAs[0].pem) {
				t.Fatalf("clean configuration did not install CA snapshot: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(registryReadyPath, "/"))); err != nil {
				t.Fatalf("clean configuration did not open boot gate: %v", err)
			}
		})
	}
}
