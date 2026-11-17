//go:build !darwin && !linux

package nativegpu

import (
	"context"
	"os"
	"os/exec"
)

func managedCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
func ownedByCurrentUser(os.FileInfo) bool { return false }

func openNativeModel(path string) (*os.File, error) { return os.Open(path) }
