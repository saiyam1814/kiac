package cluster

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func supervisorCommand(t *testing.T, fake string) (*exec.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k3s"), []byte("#!/bin/sh\n"+fake), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "k3s.pid")
	script := strings.ReplaceAll(k3sSupervisor, "/var/run/kiac-k3s.pid", pidFile)
	script = strings.ReplaceAll(script, "mkdir -p /var/run", ":")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		// Also terminate a test child if a failed assertion prevents a normal stop.
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		cancel()
	})
	cmd := exec.CommandContext(ctx, "sh", "-c", script, "sh", "agent", "--node-name=with space")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "KIAC_TEST_DIR="+dir)
	cmd.WaitDelay = time.Second
	return cmd, dir
}

func TestK3sSupervisorRestartsFailureAndPreservesArguments(t *testing.T) {
	cmd, dir := supervisorCommand(t, `
if [ ! -e "$KIAC_TEST_DIR/first" ]; then
  touch "$KIAC_TEST_DIR/first"
  exit 1
fi
printf '%s\n' "$@" > "$KIAC_TEST_DIR/args"
exit 0
`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("supervisor: %v\n%s", err, out)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || string(args) != "agent\n--node-name=with space\n" {
		t.Fatalf("restarted child args = %q, err = %v", args, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "k3s.pid")); !os.IsNotExist(err) {
		t.Fatalf("PID file remains after clean exit: %v", err)
	}
}

func TestK3sSupervisorForwardsStopWithoutRestarting(t *testing.T) {
	cmd, dir := supervisorCommand(t, `
trap 'printf stopped > "$KIAC_TEST_DIR/stopped"; exit 0' TERM
printf started >> "$KIAC_TEST_DIR/started"
while :; do sleep 1 & wait $!; done
`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "stopped")); err != nil || string(raw) != "stopped" {
		t.Fatalf("child did not handle stop: %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "started")); err != nil || string(raw) != "started" {
		t.Fatalf("child restarted after stop: %q, %v", raw, err)
	}
}
