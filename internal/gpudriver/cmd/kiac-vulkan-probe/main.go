// The native Vulkan loader is deliberately a separate executable so that the
// kubelet agent and webhook retain their static BusyBox-compatible builds.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/saiyam1814/kiac/internal/gpudriver/vulkanprobe"
)

func main() {
	render := flag.String("render-device", "/dev/dri/renderD128", "configured Venus render device")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected probe arguments")
		os.Exit(1)
	}
	info, err := os.Stat(*render)
	if err == nil && info.Mode()&os.ModeCharDevice == 0 {
		err = fmt.Errorf("path is not a character device")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "configured Venus render device %s is unavailable: %v\n", *render, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := vulkanprobe.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Vulkan transfer probe: %v\n", err)
		os.Exit(1)
	}
}
