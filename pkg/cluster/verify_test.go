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
		"gpu.runtime-class":     VerificationSkip,
		"gpu.device-plugin":     VerificationSkip,
		"gpu.nodes":             VerificationSkip,
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

func TestVerifyReportsMissingBuiltInGateway(t *testing.T) {
	t.Setenv("KIAC_TEST_GATEWAY_CRD", "true")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := verificationStatus(report, "gateway.api"); got != VerificationFail {
		t.Fatalf("Gateway check = %s, want fail when CRDs exist but the built-in Gateway is missing", got)
	}
}

func TestVerifyReportsHealthyMockGPU(t *testing.T) {
	t.Setenv("KIAC_TEST_GPU", "true")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gpu.runtime-class", "gpu.device-plugin", "gpu.nodes", "gpu.node.kiac-dev-control-plane"} {
		if got := verificationStatus(report, id); got != VerificationPass {
			t.Errorf("check %s = %s, want pass", id, got)
		}
	}
}

func TestVerifyReportsMissingMockGPUCapacity(t *testing.T) {
	t.Setenv("KIAC_TEST_GPU", "true")
	t.Setenv("KIAC_TEST_GPU_CAPACITY", "0")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gpu.nodes", "gpu.node.kiac-dev-control-plane"} {
		if got := verificationStatus(report, id); got != VerificationFail {
			t.Errorf("check %s = %s, want fail", id, got)
		}
	}
}

func TestVerifyReportsWrongGPURuntimeClass(t *testing.T) {
	t.Setenv("KIAC_TEST_GPU", "true")
	t.Setenv("KIAC_TEST_GPU_HANDLER", "nvidia")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := verificationStatus(report, "gpu.runtime-class"); got != VerificationFail {
		t.Errorf("gpu.runtime-class = %s, want fail", got)
	}
}

func TestVerifyReportsNodeThatLostGPULabel(t *testing.T) {
	t.Setenv("KIAC_TEST_GPU", "true")
	t.Setenv("KIAC_TEST_GPU_LABELS", "missing")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gpu.nodes", "gpu.node.kiac-dev-control-plane"} {
		if got := verificationStatus(report, id); got != VerificationFail {
			t.Errorf("check %s = %s, want fail", id, got)
		}
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
    if [ "${KIAC_TEST_GPU:-}" = true ] && [ "${KIAC_TEST_GPU_LABELS:-}" != missing ]; then
      capacity="${KIAC_TEST_GPU_CAPACITY:-1}"
      printf '{"items":[{"metadata":{"name":"kiac-dev-control-plane","labels":{"kiac.dev/gpu.present":"true","kiac.dev/gpu.product":"Mock","kiac.dev/gpu.memory":"0","kiac.dev/gpu.count":"1","kiac.dev/gpu.api":"mock"}},"status":{"conditions":[{"type":"Ready","status":"True"}],"capacity":{"kiac.dev/gpu":"%s"}}}]}\n' "$capacity"
    else
      printf '%s\n' '{"items":[{"metadata":{"name":"kiac-dev-control-plane"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}'
    fi
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
  *"get daemonset kiac-gpu-device-plugin -n kiac-gpu-system"*)
    if [ "${KIAC_TEST_GPU:-}" = true ]; then
      printf '%s\n' '{"status":{"desiredNumberScheduled":1,"numberReady":1,"numberAvailable":1}}'
    fi
    ;;
  *"get runtimeclass kiac-gpu --ignore-not-found -o json"*)
    if [ "${KIAC_TEST_GPU:-}" = true ]; then
      printf '{"handler":"%s"}\n' "${KIAC_TEST_GPU_HANDLER:-runc}"
    fi
    ;;
  *"get crd gateways.gateway.networking.k8s.io"*)
    if [ "${KIAC_TEST_GATEWAY_CRD:-}" = true ]; then
      printf '%s\n' 'customresourcedefinition.apiextensions.k8s.io/gateways.gateway.networking.k8s.io'
    fi
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
