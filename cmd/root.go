// Package cmd wires the kiac CLI. Commands stay thin; orchestration lives
// in pkg/cluster.
package cmd

import (
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

// Version is overridden at build time via -ldflags.
var Version = "v0.1.0-dev"

var rootCmd = &cobra.Command{
	Use:   "kiac",
	Short: "Kubernetes in Apple Containers",
	Long: `kiac runs Kubernetes clusters where every node is its own lightweight
virtual machine, powered by Apple's open-source container runtime
(apple/container) on Apple silicon.

Think kind, but each node boots in its own microVM with hardware-grade
isolation, direct node networking from your Mac, and metrics-server
working out of the box.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the CLI.
func Execute() error {
	err := rootCmd.Execute()
	if err != nil {
		_ = ui.Fail(err)
	}
	return err
}

func init() {
	rootCmd.AddCommand(createCmd, deleteCmd, getCmd, loadCmd, doctorCmd, versionCmd)
}
