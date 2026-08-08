package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
