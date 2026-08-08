package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var (
	verifyName    string
	verifyOutput  string
	verifyTimeout time.Duration
)

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Run read-only health checks",
}

var verifyClusterCmd = &cobra.Command{
	Use:     "cluster",
	Short:   "Verify a kiac cluster from its VMs through the host API path",
	Example: "  kiac verify cluster --name dev\n  kiac verify cluster --name dev -o json",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if verifyOutput != "text" && verifyOutput != "json" {
			return fmt.Errorf("unknown output format %q (supported: text, json)", verifyOutput)
		}
		report, err := cluster.NewManager().Verify(verifyName, verifyTimeout)
		if err != nil {
			return err
		}
		if verifyOutput == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(report); err != nil {
				return err
			}
		} else {
			ui.Banner(Version)
			fmt.Printf("\nCluster %s (%s)\n", report.Cluster, report.Distro)
			for _, check := range report.Checks {
				switch check.Status {
				case cluster.VerificationPass:
					ui.Check(true, check.Name, check.Detail)
				case cluster.VerificationFail:
					ui.Check(false, check.Name, check.Detail)
				case cluster.VerificationWarn:
					ui.Warnf("%s: %s", check.Name, check.Detail)
				case cluster.VerificationSkip:
					ui.Infof("skip %s: %s", check.Name, check.Detail)
				}
				if check.Hint != "" && check.Status != cluster.VerificationPass {
					ui.Hintf("%s", check.Hint)
				}
			}
			fmt.Printf("\nChecked in %s: %d failed, %d warning(s).\n",
				report.Duration, report.FailureCount(), verificationWarnings(report))
		}
		if failures := report.FailureCount(); failures > 0 {
			return fmt.Errorf("cluster %q failed %d verification check(s)", report.Cluster, failures)
		}
		return nil
	},
}

func verificationWarnings(report cluster.VerificationReport) int {
	n := 0
	for _, check := range report.Checks {
		if check.Status == cluster.VerificationWarn {
			n++
		}
	}
	return n
}

func init() {
	verifyClusterCmd.Flags().StringVar(&verifyName, "name", "dev", "cluster name")
	verifyClusterCmd.Flags().StringVarP(&verifyOutput, "output", "o", "text", "output format: text or json")
	verifyClusterCmd.Flags().DurationVar(&verifyTimeout, "timeout", 10*time.Second, "maximum duration of each node diagnostic command")
	verifyCmd.AddCommand(verifyClusterCmd)
}
