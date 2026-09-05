package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestWaitSystemdManagedAddonsWaitsOnlyForInstalledManagedResources(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  *"get deployments -A -o json"*)
    cat <<'EOF'
{"items":[
  {"metadata":{"namespace":"kube-system","name":"coredns"}},
  {"metadata":{"namespace":"kube-system","name":"metrics-server"}},
  {"metadata":{"namespace":"local-path-storage","name":"local-path-provisioner"}},
  {"metadata":{"namespace":"kiac-observability","name":"grafana"}},
  {"metadata":{"namespace":"default","name":"user-app"}}
]}
EOF
    ;;
  *"get apiservice v1beta1.metrics.k8s.io --ignore-not-found -o name"*)
    printf 'apiservice.apiregistration.k8s.io/v1beta1.metrics.k8s.io\n'
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	if err := manager.waitSystemdManagedAddons("kiac-dev-control-plane", "kubeadm", time.Minute); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(raw)
	for _, want := range []string{
		"wait --for=condition=Available deployment/coredns -n kube-system",
		"wait --for=condition=Available deployment/metrics-server -n kube-system",
		"wait --for=condition=Available deployment/local-path-provisioner -n local-path-storage",
		"wait --for=condition=Available deployment/grafana -n kiac-observability",
		"wait --for=condition=Available apiservice/v1beta1.metrics.k8s.io",
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("managed addon wait %q is missing:\n%s", want, commands)
		}
	}
	if strings.Contains(commands, "deployment/user-app") {
		t.Fatalf("resume waited for a user deployment:\n%s", commands)
	}
	if strings.Contains(commands, "deployment/prometheus") {
		t.Fatalf("resume waited for an absent optional deployment:\n%s", commands)
	}
}

func TestWaitSystemdManagedAddonsAllowsDisabledAddons(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_COMMAND_LOG"
case "$*" in
  *"get deployments -A -o json"*) printf '{"items":[]}\n' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIAC_TEST_COMMAND_LOG", logPath)
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	if err := manager.waitSystemdManagedAddons("kiac-dev-control-plane", "k3s", time.Minute); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if commands := string(raw); strings.Contains(commands, " wait ") {
		t.Fatalf("resume issued a wait for disabled addons:\n%s", commands)
	}
}

func TestWaitSystemdManagedAddonsRejectsInvalidInventory(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
case "$*" in
  *"get deployments -A -o json"*) printf 'not-json\n' ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{rt: &runtime.Client{Bin: bin}}
	err := manager.waitSystemdManagedAddons("kiac-dev-control-plane", "kubeadm", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "parsing Kubernetes deployment inventory") {
		t.Fatalf("error = %v", err)
	}
}

func TestKubectlTimeoutArgRoundsUp(t *testing.T) {
	for _, test := range []struct {
		timeout time.Duration
		want    string
	}{
		{timeout: 100 * time.Millisecond, want: "--timeout=1s"},
		{timeout: time.Second, want: "--timeout=1s"},
		{timeout: time.Second + time.Nanosecond, want: "--timeout=2s"},
	} {
		if got := kubectlTimeoutArg(test.timeout); got != test.want {
			t.Errorf("kubectlTimeoutArg(%s) = %q, want %q", test.timeout, got, test.want)
		}
	}
}
