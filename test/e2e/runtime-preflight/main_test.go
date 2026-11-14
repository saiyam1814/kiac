package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodStatus = `{"status":"running","client":{"version":"1.7.1","appName":"container"},"server":{"version":"1.7.1","appName":"container-apiserver"},"host":{"architecture":"arm64"},"paths":{"appRoot":"PRIVATE_PATH"},"resources":{"containersTotal":42}}`

func TestValidateStatusFixtures(t *testing.T) {
	host := hostInfo{"darwin", "arm64", "27.0.1"}
	for _, test := range []struct {
		name, input string
		ok          bool
	}{
		{"current schema", goodStatus, true},
		{"client skew", strings.Replace(goodStatus, `"version":"1.7.1"`, `"version":"1.6.1"`, 1), false},
		{"server skew", strings.Replace(goodStatus, `"server":{"version":"1.7.1"`, `"server":{"version":"1.6.1"`, 1), false},
		{"missing server", `{"status":"running","client":{"version":"1.7.1","appName":"container"}}`, false},
		{"unregistered", `{"status":"unregistered"}`, false},
		{"wrong host", strings.Replace(goodStatus, `"architecture":"arm64"`, `"architecture":"amd64"`, 1), false},
		{"wrong identity", strings.Replace(goodStatus, `container-apiserver`, `other-server`, 1), false},
		{"empty", "", false},
		{"invalid", "not JSON PRIVATE_PATH", false},
		{"multiple objects", goodStatus + "{}", false},
		{"oversized", strings.Repeat(" ", maxOutput) + goodStatus, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := validateStatus([]byte(test.input), "1.7.1", host)
			if (err == nil) != test.ok {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "PRIVATE_PATH") || strings.Contains(fmt.Sprint(err), "PRIVATE_PATH") {
				t.Fatal("runtime paths leaked into receipt/error")
			}
		})
	}
	oldStatus := strings.ReplaceAll(goodStatus, "1.7.1", "1.6.1")
	if _, err := validateStatus([]byte(oldStatus), "1.6.1", host); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightRejectsBeforeRuntimeCalls(t *testing.T) {
	for _, test := range []struct{ version, hostOS, arch string }{
		{"", "darwin", "arm64"}, {"1.7", "darwin", "arm64"}, {"1.7.1", "linux", "arm64"}, {"1.7.1", "darwin", "amd64"},
	} {
		_, err := preflight(context.Background(), test.version, test.hostOS, test.arch, func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("unsupported profile touched runtime")
			return nil, nil
		})
		if err == nil {
			t.Fatal("unsupported profile accepted")
		}
	}
	for _, version := range []string{"", "25.6.1", "26", "26.1.private", "26.1\nprivate"} {
		if err := validateHost(hostInfo{"darwin", "arm64", version}); err == nil {
			t.Fatalf("invalid host version %q accepted", version)
		}
	}
}

func TestPreflightUsesExactStatusCommandAndAllowlistedReceipt(t *testing.T) {
	var commands []string
	result, err := preflight(context.Background(), "1.7.1", "darwin", "arm64", func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "sw_vers" {
			return []byte("26.5\n"), nil
		}
		return []byte(goodStatus), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(commands, "|") != "sw_vers -productVersion|container system status --format json" {
		t.Fatalf("unexpected calls %v", commands)
	}
	encoded, _ := json.Marshal(result)
	if string(encoded) != `{"clientVersion":"1.7.1","serverVersion":"1.7.1","os":"darwin","architecture":"arm64","osVersion":"26.5"}` {
		t.Fatalf("receipt=%s", encoded)
	}
}

func TestBoundedCommand(t *testing.T) {
	if os.Getenv("KIAC_PREFLIGHT_TEST_CHILD") != "" {
		switch os.Getenv("KIAC_PREFLIGHT_TEST_CHILD") {
		case "large":
			for i := 0; i < 1000; i++ {
				fmt.Print(strings.Repeat("x", 8192))
			}
			time.Sleep(time.Minute)
		case "hang":
			time.Sleep(time.Minute)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"large", "hang"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KIAC_PREFLIGHT_TEST_CHILD", mode)
			deadline := 100 * time.Millisecond
			if mode == "large" {
				deadline = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			started := time.Now()
			_, err := runCommand(ctx, os.Args[0], "-test.run=^TestBoundedCommand$")
			if err == nil {
				t.Fatal("unbounded helper accepted")
			}
			if mode == "large" && !errors.Is(err, errOutputLimit) {
				t.Fatalf("did not reject streaming output by size: %v", err)
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("command deadline was not enforced")
			}
		})
	}
}

func TestVacancyChecksProtectExistingResources(t *testing.T) {
	for _, data := range []string{`[{"name":"ci-compat-test-k3s"}]`, `null`, `[{}]`, `bad`} {
		if err := validateClusterAbsent([]byte(data), "ci-compat-test-k3s"); err == nil {
			t.Fatalf("unsafe cluster inventory accepted: %s", data)
		}
	}
	if err := validateClusterAbsent([]byte(`[{"name":"other"}]`), "ci-compat-test-k3s"); err != nil {
		t.Fatal(err)
	}
	if err := validateClusterAbsent([]byte(`[]`), "ci-compat-test-k3s"); err != nil {
		t.Fatal(err)
	}
	if err := validateNodeNamesAbsent([]byte("unrelated\nkiac-ci-compat-test-k3s-worker-3\n"), "ci-compat-test-k3s"); err == nil {
		t.Fatal("orphaned existing node accepted")
	}
	for _, image := range []string{"kiac-e2e-compat:test", "docker.io/library/kiac-e2e-compat:test", "not a valid image"} {
		if err := validateImageAbsent([]byte(image), "kiac-e2e-compat:test"); err == nil {
			t.Fatalf("existing or unreadable image accepted: %s", image)
		}
	}
	if err := validateImageAbsent([]byte("busybox:1.37.0\n"), "kiac-e2e-compat:test"); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedStateRequiresEveryExactOwnedVM(t *testing.T) {
	const states = `[{"configuration":{"id":"cp"},"status":{"state":"stopped"}},{"configuration":{"id":"worker"},"status":{"state":"stopped"}}]`
	for _, test := range []struct {
		name, input string
		ok          bool
	}{
		{"both stopped", states, true},
		{"worker still running", strings.Replace(states, `"state":"stopped"`, `"state":"running"`, 1), false},
		{"missing worker", `[{"configuration":{"id":"cp"},"status":{"state":"stopped"}}]`, false},
		{"duplicate cp", strings.Replace(states, `"id":"worker"`, `"id":"cp"`, 1), false},
		{"unrelated VM", strings.Replace(states, `"id":"worker"`, `"id":"other"`, 1), false},
		{"malformed", `bad PRIVATE_PATH`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateStopped([]byte(test.input), []string{"cp", "worker"}); (err == nil) != test.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHarnessPreflightFailurePreservesExistingState(t *testing.T) {
	// Exercise the actual trap with fake tools: rejecting the runtime must
	// not delete a prior kubeconfig/state file or call a mutating command.
	dir := t.TempDir()
	state := filepath.Join(dir, "prior.clusters")
	kubeconfig := filepath.Join(dir, "kiac-e2e-prior.kubeconfig")
	for _, name := range []string{state, kubeconfig} {
		if err := os.WriteFile(name, []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "container", "kubectl"} {
		body := "#!/bin/sh\nexit 1\n"
		if name != "go" {
			body = "#!/bin/sh\necho unexpected-runtime-call >&2\nexit 99\n"
		}
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "../compat.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KIAC_EXPECTED_CONTAINER_VERSION=1.7.1", "KIAC_E2E_STATE_FILE="+state, "KIAC_E2E_KUBECONFIG="+kubeconfig)
	output, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(output), "unexpected-runtime-call") {
		t.Fatalf("err=%v output=%s", err, output)
	}
	for _, name := range []string{state, kubeconfig} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != "preserve" {
			t.Fatalf("prior state changed: %s: %v", name, err)
		}
	}
}

func TestHarnessRejectsDanglingStateSymlinkBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	target := filepath.Join(dir, "missing-target")
	if err := os.Symlink(target, state); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "kubectl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "../compat.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KIAC_EXPECTED_CONTAINER_VERSION=1.7.1", "KIAC_E2E_STATE_FILE="+state, "KIAC_E2E_KUBECONFIG="+filepath.Join(dir, "kiac-e2e-new.kubeconfig"))
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("symlink accepted: %s", output)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("followed dangling symlink: %v", err)
	}
	if link, err := os.Readlink(state); err != nil || link != target {
		t.Fatalf("symlink changed: %v", err)
	}
}

func TestCancellationCleanupKeepsFailedOwnership(t *testing.T) {
	for _, mode := range []string{"delete-fails", "vm-remains", "unexpected-name", "clean"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(dir, "clusters")
			name := "ci-compat-fixture-k3s"
			if mode == "unexpected-name" {
				name = "unrelated-user-cluster"
			}
			if err := os.WriteFile(state, []byte(name+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			kiacScript := "#!/bin/sh\nexit 0\n"
			if mode == "delete-fails" {
				kiacScript = "#!/bin/sh\nexit 1\n"
			}
			if mode == "unexpected-name" {
				kiacScript = "#!/bin/sh\necho unexpected-delete >&2\nexit 99\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "kiac"), []byte(kiacScript), 0700); err != nil {
				t.Fatal(err)
			}
			containerScript := "#!/bin/sh\nexit 0\n"
			if mode == "vm-remains" {
				containerScript = "#!/bin/sh\necho kiac-ci-compat-fixture-k3s-worker-1\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "container"), []byte(containerScript), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "../compat-cleanup.sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "KIAC_BIN="+filepath.Join(bin, "kiac"), "KIAC_E2E_RUN_ID=fixture", "KIAC_E2E_STATE_FILE="+state)
			output, err := cmd.CombinedOutput()
			if mode == "clean" {
				if err != nil {
					t.Fatalf("clean failed: %v %s", err, output)
				}
				if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Fatal("clean ownership record remained")
				}
			} else {
				if err == nil {
					t.Fatal("failed cleanup reported success")
				}
				data, err := os.ReadFile(state)
				if err != nil || string(data) != name+"\n" {
					t.Fatalf("failed ownership lost: %v %s", err, data)
				}
				if strings.Contains(string(output), "unexpected-delete") {
					t.Fatal("unrelated record was deleted")
				}
			}
		})
	}
}
