// Package cmd wires the kiac CLI. Commands stay thin; orchestration lives
// in pkg/cluster.
package cmd

import (
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

// Version is overridden at release time via -ldflags; the default keeps
// `go install` builds honest about which release line they track.
var Version = "v0.1.0"

var rootCmd = &cobra.Command{
	Use:   "kiac",
	Short: "Kubernetes in Apple Containers",
	Long: `kiac runs Kubernetes clusters where every node is its own lightweight
virtual machine on Apple silicon. Ordinary clusters use Apple's open-source
container runtime; opt-in GPU clusters use krunkit to expose the Apple GPU
through virtio-gpu and Venus.

Think kind, but each node boots in its own lightweight VM with hardware-grade
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
	rootCmd.AddCommand(createCmd, deleteCmd, getCmd, loadCmd, doctorCmd, gpuCmd, verifyCmd, supportCmd, versionCmd)
}
