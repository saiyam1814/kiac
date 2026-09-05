package cmd

import (
	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var (
	stopNodeCluster  string
	startNodeCluster string
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a resource",
}

var stopNodeCmd = &cobra.Command{
	Use:   "node <node>",
	Short: "Stop a node VM to test real node failure",
	Long: `Stop one node's VM and let Kubernetes react: the node goes NotReady
and its pods reschedule. Accepts the short node name (worker-1, gpu-1,
control-plane) or the full container name (kiac-dev-worker-1).

Stopping the control plane of a single-node cluster is refused; on
multi-node clusters it proceeds with a warning (the API server goes
down until the node is started again).`,
	Example: `  kiac stop node worker-1
  kiac stop node worker-2 --name staging`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		return cluster.NewManager().StopNode(stopNodeCluster, args[0])
	},
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a resource",
}

var startNodeCmd = &cobra.Command{
	Use:   "node <node>",
	Short: "Start a previously stopped node VM",
	Example: `  kiac start node worker-1
	  kiac start node gpu-1 --name gpu-lab
  kiac start node worker-2 --name staging`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		return cluster.NewManager().StartNode(startNodeCluster, args[0])
	},
}

func init() {
	stopNodeCmd.Flags().StringVar(&stopNodeCluster, "name", "dev", "cluster name")
	startNodeCmd.Flags().StringVar(&startNodeCluster, "name", "dev", "cluster name")
	stopCmd.AddCommand(stopNodeCmd)
	startCmd.AddCommand(startNodeCmd)
	rootCmd.AddCommand(stopCmd, startCmd)
}
