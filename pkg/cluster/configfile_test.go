package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
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
distro: k3s
workers: 3
gpuWorkers: 2
gpuImage: fedora-44
gpuDiskSize: 24G
gpuResourceDriver: dra
k8sVersion: "1.34"
image: docker.io/kindest/node:v1.34.8
cni: none
dns: [192.168.64.1, 9.9.9.9]
mounts:
  - source: /Users/me/project files
    target: /workspace
    readOnly: true
cpus: "8"
memory: 8G
cpMemory: 10G
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
				if fc.Name != "demo" || fc.Distro != "k3s" || fc.Workers == nil || *fc.Workers != 3 {
					t.Errorf("name/distro/workers = %q/%q/%v", fc.Name, fc.Distro, fc.Workers)
				}
				if fc.K8sVersion != "1.34" || fc.Image != "docker.io/kindest/node:v1.34.8" {
					t.Errorf("k8sVersion/image = %q/%q", fc.K8sVersion, fc.Image)
				}
				if fc.GPUWorkers == nil || *fc.GPUWorkers != 2 || fc.GPUImage != "fedora-44" || fc.GPUDiskSize != "24G" || fc.GPUDriver != "dra" {
					t.Errorf("GPU config = %v/%q/%q/%q", fc.GPUWorkers, fc.GPUImage, fc.GPUDiskSize, fc.GPUDriver)
				}
				if fc.CPMemory != "10G" {
					t.Errorf("cpMemory = %q", fc.CPMemory)
				}
				if fc.CNI != "none" || fc.CPUs != "8" || fc.Memory != "8G" || fc.Wait != "10m" {
					t.Errorf("cni/cpus/memory/wait = %q/%q/%q/%q", fc.CNI, fc.CPUs, fc.Memory, fc.Wait)
				}
				if len(fc.DNS) != 2 || fc.DNS[0] != "192.168.64.1" || fc.DNS[1] != "9.9.9.9" {
					t.Errorf("dns = %q", fc.DNS)
				}
				if len(fc.Mounts) != 1 || fc.Mounts[0].Source != "/Users/me/project files" || fc.Mounts[0].Target != "/workspace" || !fc.Mounts[0].ReadOnly {
					t.Errorf("mounts = %+v", fc.Mounts)
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
			name:    "unknown mount key",
			yaml:    "mounts:\n  - source: /tmp/data\n    target: /data\n    mode: ro\n",
			wantErr: "mode",
		},
		{
			name:    "additional YAML document",
			yaml:    "name: demo\n---\nmounts: []\n",
			wantErr: "multiple YAML documents",
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
		GPUImage:    DefaultGPUImage,
		GPUDiskSize: "20G",
		GPUDriver:   "device-plugin",
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
		Name:        "demo",
		Workers:     intPtr(3),
		GPUWorkers:  intPtr(2),
		GPUImage:    "fedora-custom",
		GPUDiskSize: "24G",
		GPUDriver:   "dra",
		K8sVersion:  "1.34",
		Image:       "docker.io/kindest/node:v1.34.8",
		CNI:         "none",
		DNS:         []string{"192.168.64.1", "9.9.9.9"},
		Mounts:      runtime.Mounts{{Source: "/Users/me/project", Target: "/workspace", ReadOnly: true}},
		CPUs:        "8",
		Memory:      "8G",
		CPMemory:    "10G",
		Wait:        "10m",
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
				Name: "demo", Workers: 3, GPUWorkers: 2, GPUImage: "fedora-custom", GPUDiskSize: "24G", GPUDriver: "dra",
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				Mounts:    runtime.Mounts{{Source: "/Users/me/project", Target: "/workspace", ReadOnly: true}},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "10G",
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
				Name: "dev", GPUImage: DefaultGPUImage, GPUDiskSize: "20G", GPUDriver: "device-plugin", CNI: "kindnet", CPUs: "4", Memory: "2G", CPMemory: "4G",
				WaitTimeout: 5 * time.Minute,
			},
			wantVersion: DefaultK8sVersion,
		},
		{
			name:    "explicit flags override file",
			file:    full,
			changed: map[string]bool{"name": true, "workers": true, "gpu-workers": true, "gpu-image": true, "gpu-disk-size": true, "gpu-resource-driver": true, "k8s-version": true, "no-metrics": true, "wait": true},
			mutate: func(cfg *Config, v *string) {
				cfg.Name = "cli"
				cfg.Workers = 1
				cfg.GPUWorkers = 1
				cfg.GPUImage = "cli.raw"
				cfg.GPUDiskSize = "30G"
				cfg.GPUDriver = "device-plugin"
				*v = "1.36"
			},
			wantCfg: Config{
				Name: "cli", Workers: 1, GPUWorkers: 1, GPUImage: "cli.raw", GPUDiskSize: "30G", GPUDriver: "device-plugin",
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				Mounts:    runtime.Mounts{{Source: "/Users/me/project", Target: "/workspace", ReadOnly: true}},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "10G",
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
				Name: "demo", Workers: 3, GPUWorkers: 2, GPUImage: "fedora-custom", GPUDiskSize: "24G", GPUDriver: "dra",
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				Mounts:    runtime.Mounts{{Source: "/Users/me/project", Target: "/workspace", ReadOnly: true}},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "10G",
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
				Name: "demo", Workers: 3, GPUWorkers: 2, GPUImage: "fedora-custom", GPUDiskSize: "24G", GPUDriver: "dra",
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"10.0.0.53"},
				Mounts:    runtime.Mounts{{Source: "/Users/me/project", Target: "/workspace", ReadOnly: true}},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "10G",
				NoMetrics: true, NoStorage: true, NoLB: true, NoEdgeProxy: true,
				Observability: true, Gateway: true,
				WaitTimeout: 10 * time.Minute,
			},
			wantVersion: "1.34",
		},
		{
			name:    "explicit mount flags override file",
			file:    full,
			changed: map[string]bool{"mount": true},
			mutate: func(cfg *Config, v *string) {
				cfg.Mounts = runtime.Mounts{{Source: "/cli", Target: "/data"}}
			},
			wantCfg: Config{
				Name: "demo", Workers: 3, GPUWorkers: 2, GPUImage: "fedora-custom", GPUDiskSize: "24G", GPUDriver: "dra",
				Image:     "docker.io/kindest/node:v1.34.8",
				CNI:       "none",
				DNS:       []string{"192.168.64.1", "9.9.9.9"},
				Mounts:    runtime.Mounts{{Source: "/cli", Target: "/data"}},
				CPUs:      "8",
				Memory:    "8G",
				CPMemory:  "10G",
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
				Name: "demo", Workers: 0, GPUImage: DefaultGPUImage, GPUDiskSize: "20G", GPUDriver: "device-plugin", CNI: "kindnet", CPUs: "4", Memory: "2G", CPMemory: "4G",
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
			distro := "kubeadm"
			if c.mutate != nil {
				c.mutate(&cfg, &version)
			}
			changed := func(flag string) bool { return c.changed[flag] }
			err := c.file.Merge(&cfg, &distro, &version, changed)
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

func TestMergeDistroPrecedence(t *testing.T) {
	file := FileConfig{Distro: "k3s"}
	for _, tc := range []struct {
		name    string
		changed bool
		want    string
	}{
		{name: "config file selects distro", want: "k3s"},
		{name: "explicit CLI distro wins", changed: true, want: "kubeadm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, version := cliDefaults()
			distro := "kubeadm"
			if err := file.Merge(&cfg, &distro, &version, func(flag string) bool {
				return flag == "distro" && tc.changed
			}); err != nil {
				t.Fatal(err)
			}
			if distro != tc.want {
				t.Fatalf("distro = %q, want %q", distro, tc.want)
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
	distro := "kubeadm"
	if err := fc.Merge(&cfg, &distro, &version, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "dev" || cfg.Workers != 2 || version != "1.36" {
		t.Errorf("example merged to name=%q workers=%d version=%q", cfg.Name, cfg.Workers, version)
	}
	if cfg.NoMetrics || cfg.NoStorage || cfg.NoLB || cfg.Observability || cfg.Gateway {
		t.Errorf("example addons should match defaults, got %+v", cfg)
	}
}
