package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// container CLI 0.x ls --format json shape (nested configuration object).
const lsV0 = `[{"status":"running","networks":[{"address":"192.168.64.2/24"}],"configuration":{"id":"kiac-dev-control-plane","image":{"reference":"docker.io/kindest/node:v1.34.0"}}},{"status":"running","configuration":{"id":"unrelated","image":{"reference":"nginx"}}}]`

// container CLI 1.x shape: top-level id, status is an object with state.
const lsV1 = `[{"id":"kiac-dev-worker-1","configuration":{"id":"kiac-dev-worker-1","image":{"reference":"docker.io/kindest/node:v1.34.0"}},"status":{"state":"stopped","networks":[{"ipv4Address":"192.168.65.13/24"}]}}]`

func TestParseListShapes(t *testing.T) {
	infos, err := parseList(lsV0, "kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "kiac-dev-control-plane" || infos[0].Status != "running" {
		t.Errorf("v0 shape parsed wrong: %+v", infos)
	}

	infos, err = parseList(lsV1, "kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "kiac-dev-worker-1" || infos[0].Status != "stopped" {
		t.Errorf("v1 shape parsed wrong: %+v", infos)
	}
}

func TestExecTimeout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := (&Client{Bin: bin}).ExecTimeout("node", 25*time.Millisecond, "true")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ExecTimeout error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ExecTimeout took %s", elapsed)
	}
}

func TestWaitReadyExecTimeout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := (&Client{Bin: bin}).WaitReady("node", time.Second)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("WaitReady took %s for timeout 1s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitReady error = %v, want context deadline", err)
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("WaitReady error = %v, want command error", err)
	}
	wantArgs := []string{"exec", "node", "systemctl", "is-active", "containerd"}
	if !slices.Equal(commandErr.Args, wantArgs) {
		t.Errorf("WaitReady command args = %q, want %q", commandErr.Args, wantArgs)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	firstErr := errors.New("first probe failed")
	commandErr := &CommandError{
		Tool:   "container",
		Args:   []string{"exec", "node", "systemctl", "is-active", "containerd"},
		Output: "containerd failed\n",
		Err:    errors.New("exit status 3"),
	}
	for _, tc := range []struct {
		name   string
		output string
		err    error
	}{
		{name: "failed_probe", output: commandErr.Output, err: commandErr},
		{name: "inactive_probe", output: "  inactive \n"},
		{name: "empty_probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const timeout = 2500 * time.Millisecond
				probes := 0
				started := time.Now()
				err := waitReady("node", timeout, func(string, time.Duration, ...string) (string, error) {
					probes++
					if probes == 1 {
						return "first probe failed\n", firstErr
					}
					return tc.output, tc.err
				})
				if elapsed := time.Since(started); elapsed != timeout {
					t.Errorf("WaitReady took %s, want %s", elapsed, timeout)
				}
				if probes != 2 {
					t.Errorf("WaitReady probes = %d, want 2", probes)
				}
				want := "node node did not become ready in " + timeout.String()
				if tc.err != nil {
					want = "node node did not become ready: " + tc.err.Error()
					if !errors.Is(err, tc.err) {
						t.Errorf("WaitReady error = %v, want wrapped last probe error %v", err, tc.err)
					}
				} else if out := strings.TrimSpace(tc.output); out != "" {
					want += ": " + out
				}
				if err == nil || err.Error() != want {
					t.Errorf("WaitReady error = %v, want %q", err, want)
				}
				if errors.Is(err, firstErr) {
					t.Errorf("WaitReady retained an earlier probe error: %v", err)
				}
			})
		})
	}
}

func TestWaitReadyRemainingDeadline(t *testing.T) {
	for _, tc := range []struct {
		name               string
		timeout            time.Duration
		firstProbeDuration time.Duration
		wantTimeouts       []time.Duration
	}{
		{
			name:               "short_wait",
			timeout:            3500 * time.Millisecond,
			firstProbeDuration: 250 * time.Millisecond,
			wantTimeouts:       []time.Duration{3500 * time.Millisecond, 1250 * time.Millisecond},
		},
		{
			name:               "probe_cap",
			timeout:            15 * time.Second,
			firstProbeDuration: 10 * time.Second,
			wantTimeouts:       []time.Duration{10 * time.Second, 3 * time.Second},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var timeouts []time.Duration
				var lastErr *CommandError
				started := time.Now()
				err := waitReady("node", tc.timeout, func(name string, timeout time.Duration, command ...string) (string, error) {
					timeouts = append(timeouts, timeout)
					if len(timeouts) == 1 {
						time.Sleep(tc.firstProbeDuration)
						return "first probe failed\n", errors.New("first probe failed")
					}
					ctx, cancel := context.WithTimeout(context.Background(), timeout)
					defer cancel()
					<-ctx.Done()
					lastErr = &CommandError{
						Tool:   "container",
						Args:   append([]string{"exec", name}, command...),
						Output: "last probe still booting\n",
						Err:    ctx.Err(),
					}
					return lastErr.Output, lastErr
				})
				if !slices.Equal(timeouts, tc.wantTimeouts) {
					t.Errorf("WaitReady probe timeouts = %v, want %v", timeouts, tc.wantTimeouts)
				}
				if elapsed := time.Since(started); elapsed != tc.timeout {
					t.Errorf("WaitReady took %s, want %s", elapsed, tc.timeout)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("WaitReady error = %v, want context deadline", err)
				}
				var commandErr *CommandError
				if !errors.As(err, &commandErr) || commandErr != lastErr {
					t.Fatalf("WaitReady error = %v, want last probe command error %v", err, lastErr)
				}
				if !strings.Contains(err.Error(), strings.TrimSpace(lastErr.Output)) {
					t.Errorf("WaitReady error = %v, want last probe output %q", err, lastErr.Output)
				}
			})
		})
	}
}

func TestWaitReadySuccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantProbes int
	}{
		{name: "immediate", wantProbes: 1},
		{name: "after_retry", wantProbes: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				probes := 0
				started := time.Now()
				err := waitReady("node", 4*time.Second, func(name string, timeout time.Duration, command ...string) (string, error) {
					probes++
					if name != "node" || !slices.Equal(command, []string{"systemctl", "is-active", "containerd"}) {
						t.Errorf("WaitReady probe = %q %q, want node systemctl is-active containerd", name, command)
					}
					if probes < tc.wantProbes {
						return "activating\n", errors.New("exit status 3")
					}
					return "  active \n", nil
				})
				if err != nil {
					t.Fatalf("WaitReady error = %v, want success", err)
				}
				if probes != tc.wantProbes {
					t.Errorf("WaitReady probes = %d, want %d", probes, tc.wantProbes)
				}
				if elapsed, want := time.Since(started), time.Duration(tc.wantProbes-1)*2*time.Second; elapsed != want {
					t.Errorf("WaitReady took %s, want %s", elapsed, want)
				}
			})
		})
	}
}

func TestWaitReadyNonPositiveTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				err := waitReady("node", timeout, func(string, time.Duration, ...string) (string, error) {
					t.Fatal("WaitReady probed with a nonpositive timeout")
					return "", nil
				})
				if elapsed := time.Since(started); elapsed != 0 {
					t.Errorf("WaitReady took %s for timeout %s", elapsed, timeout)
				}
				want := "node node did not become ready in " + timeout.String()
				if err == nil || err.Error() != want {
					t.Errorf("WaitReady error = %v, want %q", err, want)
				}
			})
		})
	}
}

func TestRunDetachedUsesSupportedNodeSecurityFlags(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeContainerClient(t, "--cap-add\n--masked-path\n--read-only-path\n", argsFile)

	err := client.RunDetached(RunOpts{
		Name:   "kiac-test-control-plane",
		Image:  "example.invalid/node:v1",
		CPUs:   "2",
		Memory: "2G",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readArgs(t, argsFile)
	want := []string{
		"run", "-d", "--name", "kiac-test-control-plane",
		"--cap-add", "ALL",
		"--masked-path", "NONE", "--read-only-path", "NONE",
		"--cpus", "2", "--memory", "2G", "example.invalid/node:v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("run args = %q, want %q", got, want)
	}
}

func TestRunDetachedOmitsUnsupportedNodeSecurityFlags(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeContainerClient(t, "--cap-add\n", argsFile)

	if err := client.RunDetached(RunOpts{Name: "kiac-test", Image: "example.invalid/node:v1"}); err != nil {
		t.Fatal(err)
	}

	got := readArgs(t, argsFile)
	for _, unsupported := range []string{"--masked-path", "--read-only-path", "NONE"} {
		if slices.Contains(got, unsupported) {
			t.Fatalf("run args unexpectedly contain %q: %q", unsupported, got)
		}
	}
}

func TestRunDetachedPassesDNS(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeContainerClient(t, "--cap-add\n--masked-path\n--read-only-path\n", argsFile)

	err := client.RunDetached(RunOpts{
		Name:  "kiac-test-control-plane",
		Image: "example.invalid/node:v1",
		DNS:   []string{"192.168.64.1", "1.1.1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Join(readArgs(t, argsFile), " ")
	want := "--dns 192.168.64.1 --dns 1.1.1.1"
	if !strings.Contains(got, want) {
		t.Fatalf("run args = %q, want them to contain %q", got, want)
	}
}

func TestRunDetachedOmitsDNSWhenUnset(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeContainerClient(t, "--cap-add\n", argsFile)

	err := client.RunDetached(RunOpts{
		Name:  "kiac-test-control-plane",
		Image: "example.invalid/node:v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(readArgs(t, argsFile), " "); strings.Contains(got, "--dns") {
		t.Fatalf("run args = %q, want no --dns flags", got)
	}
}

func TestRunDetachedPassesMountsBeforeImage(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeContainerClient(t, "--cap-add\n", argsFile)

	err := client.RunDetached(RunOpts{
		Name:  "kiac-test-control-plane",
		Image: "example.invalid/node:v1",
		Mounts: []Mount{
			{Source: "/Users/me/project files", Target: "/workspace"},
			{Source: "/Users/me/data", Target: "/data", ReadOnly: true},
		},
		Args: []string{"server"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readArgLines(t, argsFile)
	want := []string{
		"run", "-d", "--name", "kiac-test-control-plane", "--cap-add", "ALL",
		"--mount", "type=bind,source=/Users/me/project files,target=/workspace",
		"--mount", "type=bind,source=/Users/me/data,target=/data,readonly",
		"example.invalid/node:v1", "server",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("run args = %q, want %q", got, want)
	}
}

func TestSystemStartSelectsKernelInstallMode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		installKernel bool
		wantFlag      string
	}{
		{name: "default kernel required", installKernel: true, wantFlag: "--enable-kernel-install"},
		{name: "custom kernel supplied", installKernel: false, wantFlag: "--disable-kernel-install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "args")
			client := fakeContainerClient(t, "", argsFile)

			if err := client.SystemStart(tc.installKernel); err != nil {
				t.Fatal(err)
			}

			got := readArgs(t, argsFile)
			want := []string{"system", "start", tc.wantFlag}
			if !slices.Equal(got, want) {
				t.Fatalf("system start args = %q, want %q", got, want)
			}
		})
	}
}

func TestValidateNodeRuntimeVersion(t *testing.T) {
	for _, version := range []string{"0.8.0", "1.0.0", "1.1.0", "1.2.1", "1.2.2", "2.0.0"} {
		if err := ValidateNodeRuntimeVersion(version); err != nil {
			t.Errorf("ValidateNodeRuntimeVersion(%q): %v", version, err)
		}
	}
	if err := ValidateNodeRuntimeVersion("v1.2.0"); err == nil || !strings.Contains(err.Error(), "1.2.1") {
		t.Fatalf("ValidateNodeRuntimeVersion(1.2.0) = %v, want upgrade error", err)
	}
}

func fakeContainerClient(t *testing.T, help, argsFile string) *Client {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "--help" ]; then
  printf '%s' "$KIAC_TEST_HELP"
  exit 0
fi
printf '%s\n' "$@" > "$KIAC_TEST_ARGS"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_HELP", help)
	t.Setenv("KIAC_TEST_ARGS", argsFile)
	return &Client{Bin: bin}
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func readArgLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}
