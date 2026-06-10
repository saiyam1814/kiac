package main

import (
	"os"

	"github.com/saiyam1814/kiac/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
