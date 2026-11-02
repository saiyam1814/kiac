package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/spf13/cobra"
)

func TestCreateRegistryTrustUsesResolvedDistroForMountValidation(t *testing.T) {
	for _, fromFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("config-file=%v", fromFile), func(t *testing.T) {
			oldCfg, oldConfigFile := createCfg, createConfigFile
			oldKernel, oldIPFamily, oldDistro := createKernel, createIPFamily, createDistro
			t.Cleanup(func() {
				createCfg, createConfigFile = oldCfg, oldConfigFile
				createKernel, createIPFamily, createDistro = oldKernel, oldIPFamily, oldDistro
			})
			dir := t.TempDir()
			createCfg = cluster.Config{Name: "dev", Distro: "kubeadm", WaitTimeout: time.Minute,
				RegistryCAs: cluster.RegistryCAs{"registry.test": filepath.Join(dir, "missing-ca.pem")},
				Mounts:      runtime.Mounts{{Source: dir, Target: "/etc/rancher/k3s/config.yaml.d"}},
			}
			createDistro, createIPFamily = "k3s", "ipv4"
			createConfigFile = ""
			createKernel = filepath.Join(dir, "missing-kernel")
			if fromFile {
				createDistro = "kubeadm"
				createConfigFile = filepath.Join(dir, "cluster.yaml")
				if err := os.WriteFile(createConfigFile, []byte("distro: k3s\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := createClusterCmd.RunE(&cobra.Command{}, nil)
			if err == nil || !strings.Contains(err.Error(), "K3s mount") {
				t.Fatalf("error = %v, want resolved-distro mount rejection before reading CA or resolving kernel", err)
			}
		})
	}
}
