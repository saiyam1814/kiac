package cluster

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestVerifyHealthyClusterData(t *testing.T) {
	m := fakeVerificationManager(t)
	report, err := m.Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]VerificationStatus{
		"runtime.layout":        VerificationPass,
		"runtime.nodes":         VerificationPass,
		"kubernetes.api":        VerificationPass,
		"kubernetes.nodes":      VerificationPass,
		"kubernetes.pods":       VerificationPass,
		"kubernetes.dns":        VerificationPass,
		"storage.default-class": VerificationPass,
		"metrics.api":           VerificationSkip,
		"network.edge-proxy":    VerificationSkip,
		"network.load-balancer": VerificationSkip,
		"gateway.api":           VerificationSkip,
		"observability.stack":   VerificationSkip,
	}
	for id, status := range want {
		if got := verificationStatus(report, id); got != status {
			t.Errorf("check %s = %s, want %s", id, got, status)
		}
	}
	if report.Distro != "kubeadm" {
		t.Errorf("distro = %q, want kubeadm", report.Distro)
	}
	if report.SchemaVersion != 1 || report.CheckedAt == "" || report.Duration == "" {
		t.Errorf("report metadata incomplete: %+v", report)
	}
}

func TestVerifyReportsUnhealthyPod(t *testing.T) {
	t.Setenv("KIAC_TEST_POD_PHASE", "Pending")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := verificationStatus(report, "kubernetes.pods"); got != VerificationFail {
		t.Fatalf("pod check = %s, want fail", got)
	}
	if report.FailureCount() == 0 {
		t.Fatal("failed pod did not make report unhealthy")
	}
}

func TestDistroFromNodes(t *testing.T) {
	if got := distroFromNodes([]runtime.Info{{Image: "docker.io/rancher/k3s:v1.36.2-k3s1"}}); got != "k3s" {
		t.Errorf("k3s image detected as %q", got)
	}
	if got := distroFromNodes([]runtime.Info{{Image: "docker.io/kindest/node:v1.36.1"}}); got != "kubeadm" {
		t.Errorf("kindest image detected as %q", got)
	}
}

func TestEndpointHostname(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     string
	}{
		{"https://192.168.64.2:6443", "192.168.64.2"},
		{"https://[fd00:10:244::2]:6443", "fd00:10:244::2"},
		{"not a URL", ""},
	} {
		if got := endpointHostname(tc.endpoint); got != tc.want {
			t.Errorf("endpointHostname(%q) = %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}

func fakeVerificationManager(t *testing.T) *Manager {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
case "$*" in
  "ls -a --format json")
    printf '%s\n' '[{"configuration":{"id":"kiac-dev-control-plane","image":{"reference":"docker.io/kindest/node:v1.36.1"}},"status":{"state":"running","networks":[{"ipv4Address":"127.0.0.1/8"}]}}]'
    ;;
  *"get --raw=/readyz"*)
    printf 'ok\n'
    ;;
  *"get nodes -o json"*)
    printf '%s\n' '{"items":[{"metadata":{"name":"kiac-dev-control-plane"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}'
    ;;
  *"get pods -A -o json"*)
    phase="${KIAC_TEST_POD_PHASE:-Running}"
    printf '{"items":[{"metadata":{"name":"coredns","namespace":"kube-system"},"status":{"phase":"%s","containerStatuses":[{"ready":true}]}}]}\n' "$phase"
    ;;
  *"get endpointslice -n kube-system"*)
    printf '%s\n' '{"items":[{"endpoints":[{"conditions":{"ready":true}}]}]}'
    ;;
  *"get storageclass -o json"*)
    printf '%s\n' '{"items":[{"metadata":{"name":"standard","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}'
    ;;
  *"get apiservice v1beta1.metrics.k8s.io"*|*"get gateway kiac -n kiac-gateway"*|*"get namespace kiac-observability"*)
    ;;
  *"test -x /usr/local/bin/kiac-edge-proxy"*|*"test -x /usr/local/bin/kiac-lb.sh"*)
    exit 1
    ;;
  *)
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{rt: &runtime.Client{Bin: bin}}
}
