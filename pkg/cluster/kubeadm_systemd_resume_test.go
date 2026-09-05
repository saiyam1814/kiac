package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestRefreshKubeadmSystemdPodsUsesInstalledDatapath(t *testing.T) {
	for _, test := range []struct {
		name      string
		installed string
	}{
		{name: "kindnet", installed: "kindnet"},
		{name: "cilium", installed: "cilium"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "container")
			logPath := filepath.Join(t.TempDir(), "commands.log")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  *"get daemonset ` + test.installed + ` --ignore-not-found -o name"*)
    printf 'daemonset.apps/` + test.installed + `\n'
    ;;
esac
`
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
			manager := &Manager{rt: &runtime.Client{Bin: bin}}
			if err := manager.refreshKubeadmSystemdPods("kiac-dev-control-plane", time.Minute); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			commands := string(raw)
			if !strings.Contains(commands, "rollout restart daemonset/"+test.installed) ||
				!strings.Contains(commands, "rollout status daemonset/"+test.installed) {
				t.Fatalf("installed datapath was not refreshed:\n%s", commands)
			}
			other := "kindnet"
			if test.installed == "kindnet" {
				other = "cilium"
			}
			if strings.Contains(commands, "rollout restart daemonset/"+other) {
				t.Fatalf("absent datapath was restarted:\n%s", commands)
			}
		})
	}
}

func TestRefreshKubeadmSystemdPodsRequiresDatapath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	err := manager.refreshKubeadmSystemdPods("kiac-dev-control-plane", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "neither kindnet nor Cilium") {
		t.Fatalf("error = %v", err)
	}
}

func TestRefreshOptionalNodeExporterWhenInstalled(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  *"get daemonset node-exporter --ignore-not-found -o name"*)
    printf 'daemonset.apps/node-exporter\n'
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	if err := manager.refreshOptionalNodeExporter("kiac-dev-control-plane", "k3s", time.Minute); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(raw)
	for _, want := range []string{
		"rollout restart daemonset/node-exporter",
		"rollout status daemonset/node-exporter --timeout=60s",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("node-exporter command %q is missing:\n%s", want, commands)
		}
	}
}
