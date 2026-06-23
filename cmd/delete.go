package cmd

import (
	"fmt"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var deleteName string

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a resource",
}

var deleteClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Delete a cluster and its kubeconfig entries",
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		if !cluster.ValidName(deleteName) {
			return fmt.Errorf("invalid cluster name %q", deleteName)
		}
		return cluster.NewManager().Delete(deleteName)
	},
}

func init() {
	deleteClusterCmd.Flags().StringVar(&deleteName, "name", "dev", "cluster name")
	deleteCmd.AddCommand(deleteClusterCmd)
}
