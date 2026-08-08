package cmd

import (
	"fmt"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var (
	resumeName string
	resumeWait time.Duration
)

var resumeCmd = &cobra.Command{
	Use:   "resume cluster",
	Short: "Boot a stopped cluster's VMs and heal it after a host reboot",
	Long: `Boot a stopped cluster's VMs and heal it after a host reboot.

A reboot (or 'container system stop') halts every node VM, and vmnet
hands out fresh IPs on the next boot. resume restarts the VMs and heals
the distro-specific state: kubeadm certificates and configs, or k3s
agent endpoints and node-addressed pods. It also refreshes the host
kubeconfig and networking helpers, then waits for current Ready nodes.
Safe to re-run; a running cluster is a no-op.`,
	Example: `  kiac resume cluster --name dev`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "cluster" {
			return fmt.Errorf("unknown resource %q (supported: cluster)", args[0])
		}
		ui.Banner(Version)
		return cluster.NewManager().Resume(resumeName, resumeWait)
	},
}

func init() {
	resumeCmd.Flags().StringVar(&resumeName, "name", "dev", "cluster name")
	resumeCmd.Flags().DurationVar(&resumeWait, "wait", 5*time.Minute, "how long to wait for boot, API server, and node readiness")
	rootCmd.AddCommand(resumeCmd)
}
