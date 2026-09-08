package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/spf13/cobra"
)

func TestDefaultK8sVersionUsesDistroReleaseStream(t *testing.T) {
	if got := defaultK8sVersion("kubeadm"); got != cluster.DefaultK8sVersion {
		t.Fatalf("kubeadm default = %q, want %q", got, cluster.DefaultK8sVersion)
	}
	if got := defaultK8sVersion("k3s"); got != cluster.DefaultK3sVersion {
		t.Fatalf("k3s default = %q, want %q", got, cluster.DefaultK3sVersion)
	}
}

func TestFlagOnlyClusterCommandsRejectArgs(t *testing.T) {
	for _, command := range []*cobra.Command{createClusterCmd, deleteClusterCmd, getClustersCmd, getNodesCmd} {
		t.Run(command.CommandPath(), func(t *testing.T) {
			if err := command.ValidateArgs(nil); err != nil {
				t.Fatalf("ValidateArgs(nil): %v", err)
			}
			for _, args := range [][]string{{"prod"}, {"prod", "extra"}} {
				if err := command.ValidateArgs(args); err == nil {
					t.Errorf("ValidateArgs(%q) accepted unexpected positional arguments", args)
				}
			}
		})
	}
}

func TestCreateClusterWaitValidation(t *testing.T) {
	cases := []struct {
		name       string
		configWait string
		flagWait   string
		wantWait   time.Duration
		wantErr    string
	}{
		{name: "default wait", wantWait: 5 * time.Minute, wantErr: "kernel file"},
		{name: "positive flag", flagWait: "1ns", wantWait: time.Nanosecond, wantErr: "kernel file"},
		{name: "zero flag", flagWait: "0", wantErr: "--wait must be > 0"},
		{name: "negative flag", flagWait: "-1s", wantWait: -time.Second, wantErr: "--wait must be > 0"},
		{name: "positive config", configWait: "2m", wantWait: 2 * time.Minute, wantErr: "kernel file"},
		{name: "zero config", configWait: "0s", wantWait: 5 * time.Minute, wantErr: `invalid wait "0s" in config file (must be > 0)`},
		{name: "negative config", configWait: "-1s", wantWait: 5 * time.Minute, wantErr: `invalid wait "-1s" in config file (must be > 0)`},
		{name: "zero flag overrides positive config", configWait: "2m", flagWait: "0s", wantErr: "--wait must be > 0"},
		{name: "negative flag overrides positive config", configWait: "2m", flagWait: "-1s", wantWait: -time.Second, wantErr: "--wait must be > 0"},
		{name: "positive flag overrides zero config", configWait: "0s", flagWait: "3m", wantWait: 3 * time.Minute, wantErr: "kernel file"},
		{name: "positive flag overrides negative config", configWait: "-1s", flagWait: "3m", wantWait: 3 * time.Minute, wantErr: "kernel file"},
		{name: "positive flag overrides malformed config", configWait: "banana", flagWait: "3m", wantWait: 3 * time.Minute, wantErr: "kernel file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldCfg, oldConfigFile := createCfg, createConfigFile
			oldKernel, oldIPFamily := createKernel, createIPFamily
			t.Cleanup(func() {
				createCfg, createConfigFile = oldCfg, oldConfigFile
				createKernel, createIPFamily = oldKernel, oldIPFamily
			})
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			createCfg = cluster.Config{Name: "invalid name"}
			createConfigFile = ""
			createKernel = filepath.Join(dir, "missing-kernel")
			createIPFamily = "ipv4"
			command := &cobra.Command{}
			command.Flags().DurationVar(&createCfg.WaitTimeout, "wait", 5*time.Minute, "")
			if tc.configWait != "" {
				createConfigFile = filepath.Join(dir, "cluster.yaml")
				if err := os.WriteFile(createConfigFile, []byte("wait: "+tc.configWait+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.flagWait != "" {
				if err := command.ParseFlags([]string{"--wait=" + tc.flagWait}); err != nil {
					t.Fatal(err)
				}
			}
			err := createClusterCmd.RunE(command, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RunE error = %v, want substring %q", err, tc.wantErr)
			}
			if createCfg.WaitTimeout != tc.wantWait {
				t.Errorf("WaitTimeout = %s, want %s", createCfg.WaitTimeout, tc.wantWait)
			}
		})
	}
}

// TestCreateClusterCNIPrechecksFailFast covers the --cni mistakes users
// most often make. Every one must fail before a VM boots or a download
// starts, with a message that names the fix. PATH is emptied so the
// runtime lookup fails immediately instead of reaching a real
// `container` binary.
func TestCreateClusterCNIPrechecksFailFast(t *testing.T) {
	cases := []struct {
		name     string
		cfg      cluster.Config
		flags    []string
		config   string
		distro   string
		kernel   string // "" none, "file" a fake kernel image, else literal
		ipFamily string
		want     string
	}{
		{name: "flannel without kernel", flags: []string{"--cni=flannel"}, want: "--cni flannel needs the full node kernel"},
		{name: "config file flannel without kernel", config: "cni: flannel\n", want: "--cni flannel needs the full node kernel"},
		{name: "cilium without kernel", flags: []string{"--cni=cilium"}, want: "--cni cilium needs the full node kernel"},
		{name: "k3s rejects cni", distro: "k3s", flags: []string{"--cni=flannel"}, want: "cni selection applies to --distro kubeadm only"},
		{name: "gpu rejects custom kernel", cfg: cluster.Config{GPUWorkers: 1}, flags: []string{"--cni=flannel"}, kernel: "file", want: "--kernel applies to apple/container nodes"},
		{name: "gpu rejects flannel", cfg: cluster.Config{GPUWorkers: 1}, flags: []string{"--cni=flannel"}, want: "support --cni kindnet or cilium"},
		{name: "dual-stack rejects flannel", ipFamily: "dual", kernel: "file", flags: []string{"--cni=flannel"}, want: "does not support --cni flannel"},
		{name: "cni names are lowercase", kernel: "file", flags: []string{"--cni=Flannel"}, want: "unknown --cni"},
		{name: "calico fails before boot", kernel: "file", flags: []string{"--cni=calico"}, want: "calico needs kernel features"},
		{name: "typo fails before boot", kernel: "file", flags: []string{"--cni=flanel"}, want: "unknown --cni"},
		{name: "typo fails before the kernel download", kernel: "full", flags: []string{"--cni=flanel"}, want: "unknown --cni"},
		{name: "calico fails before the kernel download", kernel: "full", flags: []string{"--cni=calico"}, want: "calico needs kernel features"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldCfg, oldConfigFile := createCfg, createConfigFile
			oldKernel, oldIPFamily, oldDistro := createKernel, createIPFamily, createDistro
			t.Cleanup(func() {
				createCfg, createConfigFile = oldCfg, oldConfigFile
				createKernel, createIPFamily, createDistro = oldKernel, oldIPFamily, oldDistro
			})
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("PATH", dir)
			createCfg = tc.cfg
			createCfg.Name = "dev"
			createCfg.WaitTimeout = time.Minute
			createCfg.GPUImage = cluster.DefaultGPUImage
			createCfg.GPUDiskSize = "20G"
			createCfg.GPUDriver = "device-plugin"
			createConfigFile = ""
			createDistro = "kubeadm"
			if tc.distro != "" {
				createDistro = tc.distro
			}
			createIPFamily = "ipv4"
			if tc.ipFamily != "" {
				createIPFamily = tc.ipFamily
			}
			createKernel = tc.kernel
			if tc.kernel == "file" {
				createKernel = filepath.Join(dir, "Image")
				if err := os.WriteFile(createKernel, []byte("not a kernel"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.config != "" {
				createConfigFile = filepath.Join(dir, "cluster.yaml")
				if err := os.WriteFile(createConfigFile, []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := &cobra.Command{}
			command.Flags().StringVar(&createCfg.CNI, "cni", "kindnet", "")
			if err := command.ParseFlags(tc.flags); err != nil {
				t.Fatal(err)
			}
			err := createClusterCmd.RunE(command, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RunE error = %v, want substring %q", err, tc.want)
			}
			// No kernel or plugin archive may have been downloaded on the
			// way to the error (state directories and locks are fine).
			filepath.WalkDir(filepath.Join(dir, ".kiac"), func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if strings.Contains(path, "/kernels/") || strings.HasSuffix(path, ".tgz") {
					t.Errorf("download happened before the precheck failed: %s", path)
				}
				return nil
			})
		})
	}
}
