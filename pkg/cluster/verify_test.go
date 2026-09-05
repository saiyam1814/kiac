package cluster

import (
	"os"
	"path/filepath"
	"strings"
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

func TestVerifyAcceptsInternalObservabilityWithoutLoadBalancer(t *testing.T) {
	t.Setenv("KIAC_TEST_OBSERVABILITY", "clusterip")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := verificationStatus(report, "observability.stack"); got != VerificationPass {
		t.Fatalf("observability check = %s, want pass", got)
	}
	for _, check := range report.Checks {
		if check.ID == "observability.stack" && !strings.Contains(check.Hint, "port-forward") {
			t.Fatalf("internal Grafana hint = %q, want port-forward command", check.Hint)
		}
	}
}

func TestVerifyRejectsPendingObservabilityLoadBalancer(t *testing.T) {
	t.Setenv("KIAC_TEST_OBSERVABILITY", "pending")
	report, err := fakeVerificationManager(t).Verify("dev", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := verificationStatus(report, "observability.stack"); got != VerificationFail {
		t.Fatalf("observability check = %s, want fail", got)
	}
}

func TestVerifyGPUDriverRejectsStaleSliceInPlaceOfExpectedNode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
case "$*" in
  *"get daemonset kiac-gpu-dra"*)
    printf '%s\n' '{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
    ;;
  *"get deviceclass gpu.kiac.dev"*)
    printf '%s\n' 'kiac.dev/gpu'
    ;;
  *"get resourceslices.resource.k8s.io"*)
    printf '%s\n' '{"items":[{"spec":{"driver":"gpu.kiac.dev","nodeName":"kiac-dev-gpu-1","devices":[{"name":"venus-0"}]}},{"spec":{"driver":"gpu.kiac.dev","nodeName":"kiac-dev-gpu-3","devices":[{"name":"venus-0"}]}}]}'
    ;;
  *)
    exit 1
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{rt: &runtime.Client{Bin: bin}}
	report := VerificationReport{Distro: "kubeadm"}
	mode, healthy, ready := m.verifyGPUDriver(&report, []runtime.Info{
		{Name: "kiac-dev-gpu-1"},
		{Name: "kiac-dev-gpu-2"},
	}, "kiac-dev-control-plane", 2*time.Second)
	if mode != "dra" || healthy {
		t.Fatalf("driver result = mode %q, healthy %v; want dra, false; report: %+v", mode, healthy, report)
	}
	if !ready["kiac-dev-gpu-1"] || ready["kiac-dev-gpu-2"] || ready["kiac-dev-gpu-3"] {
		t.Fatalf("ready nodes = %v; stale slice replaced an expected node", ready)
	}
	if got := verificationStatus(report, "gpu.device-plugin"); got != VerificationFail {
		t.Fatalf("GPU driver check = %s, want fail", got)
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
  *"get crd gateways.gateway.networking.k8s.io"*)
    if [ "${KIAC_TEST_GATEWAY_CRD:-}" = true ]; then
      printf '%s\n' 'customresourcedefinition.apiextensions.k8s.io/gateways.gateway.networking.k8s.io'
    fi
    ;;
  *"get namespace kiac-observability"*)
	if [ -n "${KIAC_TEST_OBSERVABILITY:-}" ]; then
	  printf '%s\n' 'namespace/kiac-observability'
	fi
	;;
  *"get service grafana -n kiac-observability"*)
	case "${KIAC_TEST_OBSERVABILITY:-}" in
	  clusterip) printf 'ClusterIP ' ;;
	  pending) printf 'LoadBalancer ' ;;
	esac
	;;
  *"get apiservice v1beta1.metrics.k8s.io"*|*"get gateway kiac -n kiac-gateway"*)
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
