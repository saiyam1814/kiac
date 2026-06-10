package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var createCfg = cluster.Config{}

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a resource",
}

var createClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Create a Kubernetes cluster of lightweight-VM nodes",
	Example: `  kiac create cluster
  kiac create cluster --name dev --workers 2
  kiac create cluster --memory 8G --cpus 4`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		if createCfg.Workers < 0 {
			return fmt.Errorf("--workers must be >= 0")
		}
		if !cluster.ValidName(createCfg.Name) {
			return fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", createCfg.Name)
		}
		if createCfg.Image == "" {
			img, err := cluster.ResolveImage(k8sVersion)
			if err != nil {
				return err
			}
			createCfg.Image = img
		}
		return cluster.NewManager().Create(createCfg)
	},
}

var k8sVersion string

func init() {
	f := createClusterCmd.Flags()
	f.StringVar(&createCfg.Name, "name", "kiac", "cluster name")
	f.IntVar(&createCfg.Workers, "workers", 0, "number of worker nodes (control plane is untainted when 0)")
	f.StringVar(&k8sVersion, "k8s-version", cluster.DefaultK8sVersion,
		"Kubernetes version ("+strings.Join(cluster.SupportedVersions(), ", ")+")")
	f.StringVar(&createCfg.Image, "image", "", "node image (overrides --k8s-version)")
	f.StringVar(&createCfg.CNI, "cni", "kindnet", "pod network: kindnet, or none (custom kernels needed for flannel/calico/cilium)")
	f.StringVar(&createCfg.CPUs, "cpus", "4", "vCPUs per node VM")
	f.StringVar(&createCfg.Memory, "memory", "4G", "memory per node VM")
	f.BoolVar(&createCfg.NoMetrics, "no-metrics", false, "skip installing metrics-server")
	f.BoolVar(&createCfg.NoStorage, "no-storage", false, "skip installing the local-path default StorageClass")
	f.BoolVar(&createCfg.NoLB, "no-lb", false, "skip installing MetalLB (type: LoadBalancer support)")
	f.DurationVar(&createCfg.WaitTimeout, "wait", 5*time.Minute, "timeout for nodes to become ready")
	createCmd.AddCommand(createClusterCmd)
}
