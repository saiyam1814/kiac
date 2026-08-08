package cmd

import (
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var (
	supportName    string
	supportOutput  string
	supportTimeout time.Duration
)

var supportCmd = &cobra.Command{
	Use:   "support",
	Short: "Collect troubleshooting information",
}

var supportBundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Write a redacted, bounded diagnostic archive",
	Long: `Write a redacted, bounded diagnostic archive for one cluster.

Collection is read-only. Kubeconfigs, Kubernetes Secrets, raw container
inspection, process environments, and application logs are excluded. Review
the archive before sharing it because names, IPs, events, and system logs remain.`,
	Example: "  kiac support bundle --name dev\n  kiac support bundle --name dev --output /tmp/dev-support.tar.gz",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		var result cluster.SupportBundleResult
		if err := ui.Step("Collecting redacted diagnostics", func() error {
			var err error
			result, err = cluster.NewManager().SupportBundle(supportName, cluster.SupportBundleOptions{
				Output:         supportOutput,
				KiacVersion:    Version,
				CommandTimeout: supportTimeout,
			})
			return err
		}); err != nil {
			return err
		}
		ui.Successf("Support bundle written to %s", result.Path)
		ui.Infof("%d redacted file(s), archive mode 0600", result.Files)
		if len(result.Warnings) > 0 {
			ui.Warnf("%d diagnostic command(s) could not be collected; their failures are recorded in the archive", len(result.Warnings))
		}
		ui.Hintf("review contents: tar -tzf %s", result.Path)
		return nil
	},
}

func init() {
	supportBundleCmd.Flags().StringVar(&supportName, "name", "dev", "cluster name")
	supportBundleCmd.Flags().StringVarP(&supportOutput, "output", "o", "", "archive path (default: timestamped file in the current directory)")
	supportBundleCmd.Flags().DurationVar(&supportTimeout, "timeout", 10*time.Second, "maximum duration of each diagnostic command")
	supportCmd.AddCommand(supportBundleCmd)
}
