package cmd

import (
	"fmt"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var loadName string

var loadCmd = &cobra.Command{
	Use:   "load",
	Short: "Load resources into a cluster",
}

var loadImageCmd = &cobra.Command{
	Use:   "image IMAGE [IMAGE...]",
	Short: "Load locally-built images into every node",
	Long: `Copies images from the apple/container local image store into the
containerd instance of every node VM, so pods can run them without
pushing to a registry. Build first with: container build -t myapp .`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		if err := cluster.NewManager().LoadImages(loadName, args); err != nil {
			return err
		}
		fmt.Println()
		ui.Hintf("set imagePullPolicy: IfNotPresent (or Never) in your pod spec")
		return nil
	},
}

func init() {
	loadImageCmd.Flags().StringVar(&loadName, "name", "kiac", "cluster name")
	loadCmd.AddCommand(loadImageCmd)
}
