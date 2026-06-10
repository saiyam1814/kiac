package cmd

import (
	"fmt"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print kiac and runtime versions",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("kiac %s\n", Version)
		if v, err := runtime.New().Version(); err == nil {
			fmt.Printf("apple/container %s\n", v)
		}
	},
}
