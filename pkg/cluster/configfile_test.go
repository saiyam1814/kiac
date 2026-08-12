package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigFile(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string // substring of the error, empty for success
		check   func(t *testing.T, fc *FileConfig)
	}{
		{
			name: "full config",
			yaml: `name: demo
workers: 3
k8sVersion: "1.34"
image: docker.io/kindest/node:v1.34.8
cni: none
dns: [192.168.64.1, 9.9.9.9]
cpus: "8"
memory: 8G
wait: 10m
addons:
  metrics: false
  storage: false
  loadBalancer: false
  edgeProxy: false
  observability: true
  gateway: true
`,
			check: func(t *testing.T, fc *FileConfig) {
				if fc.Name != "demo" || fc.Workers == nil || *fc.Workers != 3 {
					t.Errorf("name/workers = %q/%v", fc.Name, fc.Workers)
				}
				if fc.K8sVersion != "1.34" || fc.Image != "docker.io/kindest/node:v1.34.8" {
					t.Errorf("k8sVersion/image = %q/%q", fc.K8sVersion, fc.Image)
				}
				if fc.CNI != "none" || fc.CPUs != "8" || fc.Memory != "8G" || fc.Wait != "10m" {
					t.Errorf("cni/cpus/memory/wait = %q/%q/%q/%q", fc.CNI, fc.CPUs, fc.Memory, fc.Wait)
				}
				if len(fc.DNS) != 2 || fc.DNS[0] != "192.168.64.1" || fc.DNS[1] != "9.9.9.9" {
					t.Errorf("dns = %q", fc.DNS)
				}
				a := fc.Addons
				if a.Metrics == nil || *a.Metrics || a.Storage == nil || *a.Storage || a.LoadBalancer == nil || *a.LoadBalancer || a.EdgeProxy == nil || *a.EdgeProxy {
					t.Errorf("metrics/storage/loadBalancer/edgeProxy = %v/%v/%v/%v", a.Metrics, a.Storage, a.LoadBalancer, a.EdgeProxy)
				}
				if a.Observability == nil || !*a.Observability || a.Gateway == nil || !*a.Gateway {
					t.Errorf("observability/gateway = %v/%v", a.Observability, a.Gateway)
				}
			},
		},
		{
			name: "omitted keys stay unset",
			yaml: "name: demo\n",
			check: func(t *testing.T, fc *FileConfig) {
				if fc.Workers != nil {
					t.Errorf("workers = %v, want nil", fc.Workers)
				}
				if fc.Addons.Metrics != nil || fc.Addons.Gateway != nil {
					t.Errorf("addons = %+v, want all nil", fc.Addons)
				}
			},
		},
		{
			name: "empty file is all defaults",
			yaml: "",
			check: func(t *testing.T, fc *FileConfig) {
				if !reflect.DeepEqual(*fc, FileConfig{}) {
					t.Errorf("fc = %+v, want zero value", fc)
				}
			},
		},
		{
			name:    "unknown top-level key",
			yaml:    "nodes: 3\n",
			wantErr: "nodes",
		},
		{
			name:    "unknown addon key",
			yaml:    "addons:\n  metrics: true\n  dashboard: true\n",
			wantErr: "dashboard",
		},
		{
			name:    "old flag names are unknown keys",
			yaml:    "no-metrics: true\n",
			wantErr: "no-metrics",
		},
		{
			name:    "wrong type",
			yaml:    "workers: two\n",
			wantErr: "cannot unmarshal",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc, err := LoadConfigFile(writeConfig(t, c.yaml))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, fc)
		})
	}
}

func TestLoadConfigFileMissing(t *testing.T) {
	if _, err := LoadConfigFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// cliDefaults mirrors the flag defaults in cmd/create.go.
func cliDefaults() (Config, string) {
	return Config{
		Name:        "dev",
		Workers:     0,
		CNI:         "kindnet",
		CPUs:        "4",
		Memory:      "2G",
		CPMemory:    "4G",
		WaitTimeout: 5 * time.Minute,
	}, DefaultK8sVersion
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

func TestMerge(t *testing.T) {
	full := FileConfig{
		Name:       "demo",
		Workers:    intPtr(3),
		K8sVersion: "1.34",
		Image:      "docker.io/kindest/node:v1.34.8",
		CNI:        "none",
		DNS:        []string{"192.168.64.1", "9.9.9.9"},
		CPUs:       "8",
		Memory:     "8G",
		Wait:       "10m",
		Addons: FileAddons{
			Metrics:       boolPtr(false),
			Storage:       boolPtr(false),
			LoadBalancer:  boolPtr(false),
			EdgeProxy:     boolPtr(false),
			Observability: boolPtr(true),
			Gateway:       boolPtr(true),
		},
	}

	cases := []struct {
		name        string
		file        FileConfig
		changed     map[string]bool // flags set on the command line
		mutate      func(cfg *Config, v *string)
		wantErr     string
		wantCfg     Config
		wantVersion string
	}{
		{
			name: "file fills everything when no flags set",
			file: full,
			wantCfg: Config{
				Name: "demo", Workers: 3,
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "4G",
				NoMetrics: true, NoStorage: true, NoLB: true, NoEdgeProxy: true,
				Observability: true, Gateway: true,
				WaitTimeout: 10 * time.Minute,
			},
			wantVersion: "1.34",
		},
		{
			name: "empty file keeps CLI defaults",
			file: FileConfig{},
			wantCfg: Config{
				Name: "dev", CNI: "kindnet", CPUs: "4", Memory: "2G", CPMemory: "4G",
				WaitTimeout: 5 * time.Minute,
			},
			wantVersion: DefaultK8sVersion,
		},
		{
			name:    "explicit flags override file",
			file:    full,
			changed: map[string]bool{"name": true, "workers": true, "k8s-version": true, "no-metrics": true, "wait": true},
			mutate: func(cfg *Config, v *string) {
				cfg.Name = "cli"
				cfg.Workers = 1
				*v = "1.36"
			},
			wantCfg: Config{
				Name: "cli", Workers: 1,
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "4G",
				NoMetrics: false, NoStorage: true, NoLB: true, NoEdgeProxy: true,
				Observability: true, Gateway: true,
				WaitTimeout: 5 * time.Minute,
			},
			wantVersion: "1.36",
		},
		{
			name:    "explicit addon flags override file toggles",
			file:    full,
			changed: map[string]bool{"observability": true, "gateway": true, "no-lb": true, "no-edge-proxy": true},
			wantCfg: Config{
				Name: "demo", Workers: 3,
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "4G",
				NoMetrics: true, NoStorage: true, NoLB: false, NoEdgeProxy: false,
				Observability: false, Gateway: false,
				WaitTimeout: 10 * time.Minute,
			},
			wantVersion: "1.34",
		},
		{
			name:    "explicit DNS flag overrides file",
			file:    full,
			changed: map[string]bool{"dns": true},
			mutate: func(cfg *Config, v *string) {
				cfg.DNS = []string{"10.0.0.53"}
			},
			wantCfg: Config{
				Name: "demo", Workers: 3,
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"10.0.0.53"},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "4G",
				NoMetrics: true, NoStorage: true, NoLB: true, NoEdgeProxy: true,
				Observability: true, Gateway: true,
				WaitTimeout: 10 * time.Minute,
			},
			wantVersion: "1.34",
		},
		{
			name: "explicit workers 0 in file wins over default",
			file: FileConfig{Workers: intPtr(0), Name: "demo"},
			wantCfg: Config{
				Name: "demo", Workers: 0, CNI: "kindnet", CPUs: "4", Memory: "2G", CPMemory: "4G",
				WaitTimeout: 5 * time.Minute,
			},
			wantVersion: DefaultK8sVersion,
		},
		{
			name:    "bad wait duration",
			file:    FileConfig{Wait: "banana"},
			wantErr: `invalid wait "banana"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, version := cliDefaults()
			if c.mutate != nil {
				c.mutate(&cfg, &version)
			}
			changed := func(flag string) bool { return c.changed[flag] }
			err := c.file.Merge(&cfg, &version, changed)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, c.wantCfg) {
				t.Errorf("cfg = %+v, want %+v", cfg, c.wantCfg)
			}
			if version != c.wantVersion {
				t.Errorf("k8sVersion = %q, want %q", version, c.wantVersion)
			}
		})
	}
}

// TestLoadAndMergeExample keeps examples/cluster.yaml honest: it must
// parse under KnownFields and merge cleanly onto the CLI defaults.
func TestLoadAndMergeExample(t *testing.T) {
	fc, err := LoadConfigFile("../../examples/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, version := cliDefaults()
	if err := fc.Merge(&cfg, &version, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "dev" || cfg.Workers != 2 || version != "1.36" {
		t.Errorf("example merged to name=%q workers=%d version=%q", cfg.Name, cfg.Workers, version)
	}
	if cfg.NoMetrics || cfg.NoStorage || cfg.NoLB || cfg.Observability || cfg.Gateway {
		t.Errorf("example addons should match defaults, got %+v", cfg)
	}
}
