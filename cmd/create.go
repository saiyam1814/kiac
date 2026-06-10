package cmd

import (
	"fmt"
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
	Short: "Create a Kubernetes cluster of microVM nodes",
	Example: `  kiac create cluster
  kiac create cluster --name dev --workers 2
  kiac create cluster --memory 8G --cpus 4`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		if createCfg.Workers < 0 {
			return fmt.Errorf("--workers must be >= 0")
		}
		return cluster.NewManager().Create(createCfg)
	},
}

func init() {
	f := createClusterCmd.Flags()
	f.StringVar(&createCfg.Name, "name", "kiac", "cluster name")
	f.IntVar(&createCfg.Workers, "workers", 0, "number of worker nodes (control plane is untainted when 0)")
	f.StringVar(&createCfg.Image, "image", cluster.DefaultNodeImage, "node image")
	f.StringVar(&createCfg.CPUs, "cpus", "4", "vCPUs per node VM")
	f.StringVar(&createCfg.Memory, "memory", "4G", "memory per node VM")
	f.BoolVar(&createCfg.NoMetrics, "no-metrics", false, "skip installing metrics-server")
	f.DurationVar(&createCfg.WaitTimeout, "wait", 5*time.Minute, "timeout for nodes to become ready")
	createCmd.AddCommand(createClusterCmd)
}
