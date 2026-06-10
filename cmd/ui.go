package cmd

import (
	"fmt"
	"net/http"
	"os/exec"

	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/saiyam1814/kiac/pkg/webui"
	"github.com/spf13/cobra"
)

var uiPort int

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open a local console to create and manage clusters",
	RunE: func(cmd *cobra.Command, args []string) error {
		ui.Banner(Version)
		url, handler, ln, err := webui.Serve(uiPort)
		if err != nil {
			return err
		}
		fmt.Printf("\n  console: %s  (ctrl-c to stop)\n\n", url)
		_ = exec.Command("open", url).Start()
		return http.Serve(ln, handler)
	},
}

func init() {
	uiCmd.Flags().IntVar(&uiPort, "port", 5180, "port for the local console (0 picks a free one)")
	rootCmd.AddCommand(uiCmd)
}
