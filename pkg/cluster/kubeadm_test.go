package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestInitKubeadmUsesImageVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version, exit string
		wantErr             bool
	}{
		{name: "pinned release", version: "v1.37.0\n"},
		{name: "custom prerelease", version: "v1.38.0-rc.1+vendor.2"},
		{name: "empty output", wantErr: true},
		{name: "moving alias", version: "stable-1", wantErr: true},
		{name: "extra output", version: "v1.37.0 unexpected", wantErr: true},
		{name: "probe failed", version: "v1.37.0", exit: "2", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin, calls := filepath.Join(dir, "container"), filepath.Join(dir, "init-args")
			script := `#!/bin/sh
case "$*" in
  "exec kiac-dev-control-plane kubeadm version -o short")
    printf '%s' "$KIAC_TEST_NODE_VERSION"
    exit "${KIAC_TEST_VERSION_EXIT:-0}"
    ;;
  "exec kiac-dev-control-plane kubeadm init "*)
    printf '%s\n' "$@" > "$KIAC_TEST_INIT_ARGS"
    ;;
  *) exit 99 ;;
esac
`
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KIAC_TEST_NODE_VERSION", tc.version)
			t.Setenv("KIAC_TEST_VERSION_EXIT", tc.exit)
			t.Setenv("KIAC_TEST_INIT_ARGS", calls)
			manager := &Manager{rt: &runtime.Client{Bin: bin}}
			// A custom image can differ from the CLI's version default.
			err := manager.initKubeadm("kiac-dev-control-plane", Config{K8sVersion: "1.35", Image: "custom/node@sha256:abc", IPFamily: DualStack})
			if (err != nil) != tc.wantErr {
				t.Fatalf("initKubeadm error = %v, wantErr %v", err, tc.wantErr)
			}
			raw, readErr := os.ReadFile(calls)
			if tc.wantErr {
				if !os.IsNotExist(readErr) {
					t.Fatalf("init ran without a valid image version: %s", raw)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, want := range []string{
				"--kubernetes-version=" + strings.TrimSpace(tc.version),
				"--pod-network-cidr=10.244.0.0/16,fd00:10:244::/56",
				"--service-cidr=10.96.0.0/12,fd00:10:96::/112",
			} {
				if !strings.Contains(string(raw), "\n"+want+"\n") {
					t.Errorf("init is missing %q:\n%s", want, raw)
				}
			}
		})
	}
}
