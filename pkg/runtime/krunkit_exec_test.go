package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the public SSH path, including ownership checks and output pipes.
func sshTestClient(t *testing.T, script string) (*KrunkitClient, string) {
	t.Helper()
	vm := exec.Command("sleep", "60")
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Process.Kill(); _, _ = vm.Process.Wait() })
	dir := t.TempDir()
	c := &KrunkitClient{RootDir: t.TempDir(), SSHBin: writeExecutable(t, dir, "ssh", script), ARPBin: writeExecutable(t, dir, "arp", "#!/bin/sh\nexit 0\n")}
	started, err := c.processStartTime(vm.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	command, err := c.processCommand(vm.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	name := "kiac-test-gpu-1"
	if err := c.writeState(KrunkitNodeState{Schema: krunkitStateSchema, Name: name, PID: vm.Process.Pid, ProcessStart: started, ProcessCommandHash: processCommandHash(command), IP: "192.168.120.2", SSHUser: "fedora", Status: "running", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return c, name
}

func TestKrunkitExecTimeoutBoundsDescendantPipes(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(map[bool]string{false: "exec", true: "stdin"}[stdin], func(t *testing.T) {
			c, name := sshTestClient(t, "#!/bin/sh\nsleep 20 &\necho started\nwait\n")
			start := time.Now()
			var err error
			if stdin {
				err = c.ExecStdinTimeout(name, time.Second, strings.NewReader("input"), "cat")
			} else {
				_, err = c.ExecTimeout(name, time.Second, "true")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want deadline", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second+pipeWaitDelay+5*time.Second {
				t.Fatalf("SSH pipe defeated timeout: %s", elapsed)
			}
		})
	}
}

func TestKrunkitExecSuccessfulSSHWithDescendant(t *testing.T) {
	c, name := sshTestClient(t, "#!/bin/sh\nsleep 20 &\necho success\nexit 0\n")
	start := time.Now()
	out, err := c.ExecTimeout(name, 30*time.Second, "true")
	if err != nil || strings.TrimSpace(out) != "success" {
		t.Fatalf("output=%q error=%v", out, err)
	}
	if time.Since(start) > pipeWaitDelay+5*time.Second {
		t.Fatal("waited for SSH descendant")
	}
}

func TestKrunkitExecStdinAndKeepalives(t *testing.T) {
	dir := t.TempDir()
	// Capture only generated command arguments and synthetic input.
	args := filepath.Join(dir, "args")
	input := filepath.Join(dir, "input")
	c, name := sshTestClient(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+args+"'\ncat > '"+input+"'\n")
	if err := c.ExecStdinTimeout(name, 5*time.Second, strings.NewReader("payload"), "cat"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(input)
	if string(got) != "payload" {
		t.Fatalf("input=%q", got)
	}
	raw, _ := os.ReadFile(args)
	for _, want := range []string{"ServerAliveInterval=5", "ServerAliveCountMax=3", "ConnectTimeout=5", "IdentitiesOnly=yes"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing SSH option %s", want)
		}
	}
	// Nonzero exits must never be mistaken for a delayed successful pipe.
	c.SSHBin = writeExecutable(t, t.TempDir(), "ssh", "#!/bin/sh\nexit 7\n")
	if err := c.ExecStdinTimeout(name, 5*time.Second, io.NopCloser(strings.NewReader("")), "true"); err == nil {
		t.Fatal("accepted failing SSH")
	}
}
