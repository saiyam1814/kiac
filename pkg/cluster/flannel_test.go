package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestFlannelManifestIsPinnedUpstream(t *testing.T) {
	// The embedded manifest must reference only the pinned release's
	// images, so a version bump cannot half-happen.
	for _, line := range strings.Split(flannelManifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		image := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		tag, digest, ok := strings.Cut(image, "@")
		if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			t.Fatalf("image %q is not digest-pinned; a mutable tag alone would let a retag change what every node runs as root", image)
		}
		if strings.HasPrefix(tag, "ghcr.io/flannel-io/flannel:") && !strings.HasSuffix(tag, ":"+FlannelVersion) {
			t.Fatalf("flannel image %q is not pinned to %s", image, FlannelVersion)
		}
		if !strings.Contains(tag, ":v") {
			t.Fatalf("image %q is not version-pinned", image)
		}
	}
	if !strings.Contains(flannelManifest, `"Type": "vxlan"`) {
		t.Fatal("flannel manifest no longer configures the vxlan backend")
	}
}

func TestFlannelManifestWithCIDR(t *testing.T) {
	patched, err := flannelManifestWithCIDR(kubeadmPodCIDRv4)
	if err != nil {
		t.Fatalf("flannelManifestWithCIDR: %v", err)
	}
	if !strings.Contains(patched, `"Network": "`+kubeadmPodCIDRv4+`"`) {
		t.Fatalf("patched manifest does not carry the kiac pod CIDR %s", kubeadmPodCIDRv4)
	}
	if strings.Count(patched, `"Network": "`) != 1 {
		t.Fatal("patched manifest carries more than one Network entry")
	}

	patched, err = flannelManifestWithCIDR("10.9.0.0/16")
	if err != nil {
		t.Fatalf("flannelManifestWithCIDR with custom CIDR: %v", err)
	}
	if !strings.Contains(patched, `"Network": "10.9.0.0/16"`) {
		t.Fatal("custom CIDR was not patched into net-conf.json")
	}
	if strings.Contains(patched, flannelUpstreamCIDR) {
		t.Fatal("upstream CIDR survived the patch")
	}
}

func TestFlannelManifestWithCIDRRejectsReshapedManifest(t *testing.T) {
	saved := flannelManifest
	defer func() { flannelManifest = saved }()
	flannelManifest = strings.Replace(saved, flannelUpstreamCIDR, `"Network": "192.168.0.0/16"`, 1)
	if _, err := flannelManifestWithCIDR("10.9.0.0/16"); err == nil {
		t.Fatal("a manifest without the upstream Network marker must be refused, not applied unpatched")
	}
}

func TestFlannelDelegatePluginsAreDeclaredInConflist(t *testing.T) {
	// The install path streams exactly the delegate binaries the
	// conflist needs but the node image lacks. If upstream changes the
	// delegation (a new plugin type, or dropping bridge), this pins the
	// assumption so the bump is reviewed rather than silently broken.
	if !strings.Contains(flannelManifest, `"isDefaultGateway": true`) {
		t.Fatal("flannel conflist no longer delegates to the bridge plugin; update ensureFlannelDelegatePlugins")
	}
	if !strings.Contains(flannelManifest, `"type": "portmap"`) {
		t.Fatal("flannel conflist no longer chains portmap; update ensureFlannelDelegatePlugins")
	}
}

func TestWaitFlannelReadyReturnsWithoutWaitBudget(t *testing.T) {
	// --wait 0 must not block: kubectl rollout status --timeout=0s waits
	// forever, so a non-positive budget returns before touching the
	// runtime (the final nodes-Ready step still runs).
	m := NewManager()
	for _, timeout := range []time.Duration{0, -1 * time.Second} {
		if err := m.waitFlannelReady("kiac-x-control-plane", timeout); err != nil {
			t.Fatalf("waitFlannelReady(%s) = %v, want nil without hitting the runtime", timeout, err)
		}
	}
}

func TestInstallCNIRejectsFlannelWithoutKernel(t *testing.T) {
	m := NewManager()
	err := m.installFlannel("kiac-x-control-plane", Config{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "--kernel full") {
		t.Fatalf("installFlannel without a kernel = %v, want a --kernel full hint", err)
	}
}

func TestInstallCNIErrorsNameFlannel(t *testing.T) {
	m := NewManager()
	if err := m.installCNI("cp", Config{CNI: "calico"}); err == nil || !strings.Contains(err.Error(), "flannel") {
		t.Fatalf("calico rejection should point at flannel as an option, got: %v", err)
	}
	if err := m.installCNI("cp", Config{CNI: "wat"}); err == nil || !strings.Contains(err.Error(), "flannel") {
		t.Fatalf("unknown-CNI error should list flannel as supported, got: %v", err)
	}
}

// fakeFlannelContainer stands in for the `container` CLI. Every call is
// `exec <node> kubectl ...`, so the script dispatches on the kubectl
// subcommand; the test controls each branch through environment
// variables that exec.Command inherits from the test process.
func fakeFlannelContainer(t *testing.T) *Manager {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
case "$*" in
  *"rollout status"*)
    sleep "${KIAC_TEST_ROLLOUT_SLEEP:-0}"
    [ -n "${KIAC_TEST_ROLLOUT_FAIL:-}" ] && { echo "error: timed out waiting for the condition" >&2; exit 1; }
    echo "daemon set \"kube-flannel-ds\" successfully rolled out"
    ;;
  *"get pods"*)
    sleep "${KIAC_TEST_PODS_SLEEP:-0}"
    printf 'NAME                    READY   STATUS                  RESTARTS   AGE   NODE\n'
    printf 'kube-flannel-ds-abc12   0/1     Init:ImagePullBackOff   0          90s   kiac-x-worker-2\n'
    ;;
  *"get events"*)
    sleep "${KIAC_TEST_EVENTS_SLEEP:-0}"
    [ -n "${KIAC_TEST_EVENTS_FAIL:-}" ] && exit 1
    printf 'LAST SEEN   TYPE      REASON   OBJECT                      MESSAGE\n'
    printf '10s         Warning   Failed   pod/kube-flannel-ds-abc12   Failed to pull image "ghcr.io/flannel-io/flannel-cni-plugin:v1.8.0-flannel1"\n'
    ;;
  *" logs "*)
    sleep "${KIAC_TEST_LOGS_SLEEP:-0}"
    [ -n "${KIAC_TEST_LOGS_FAIL:-}" ] && { echo "[pod/kube-flannel-ds-abc12/install-cni] partial line before failure"; exit 1; }
    printf '[pod/kube-flannel-ds-abc12/kube-flannel] E0908 vxlan device already exists\n'
    ;;
  *)
    printf 'unexpected command: %s\n' "$*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"KIAC_TEST_ROLLOUT_SLEEP", "KIAC_TEST_ROLLOUT_FAIL", "KIAC_TEST_PODS_SLEEP",
		"KIAC_TEST_EVENTS_SLEEP", "KIAC_TEST_EVENTS_FAIL", "KIAC_TEST_LOGS_SLEEP", "KIAC_TEST_LOGS_FAIL"} {
		t.Setenv(v, "")
	}
	return &Manager{rt: &runtime.Client{Bin: bin}}
}

func TestWaitFlannelReadyBoundsStuckRollout(t *testing.T) {
	// kubectl's own --timeout cannot bound a wedged `container exec`.
	// A rollout that (as far as the host can see) hangs well past --wait
	// must be killed by the outer deadline and reported as a failure,
	// not silently waited out and then reported as success.
	m := fakeFlannelContainer(t)
	t.Setenv("KIAC_TEST_ROLLOUT_SLEEP", "10")
	wait := time.Second
	started := time.Now()
	err := m.waitFlannelReady("kiac-x-control-plane", wait)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("waitFlannelReady returned success for a rollout exec stuck well past --wait")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the outer exec deadline", err)
	}
	// The fake's sleep is a child of sh, so after the deadline kills sh
	// the orphan holds the pipe until runtime's pipeWaitDelay (500ms)
	// releases it; the 2s slack covers that plus process spawn.
	if elapsed < wait || elapsed > wait+flannelRolloutGrace+2*time.Second {
		t.Fatalf("waitFlannelReady returned after %s, want about %s (+grace %s)", elapsed, wait, flannelRolloutGrace)
	}
	for _, want := range []string{"did not become ready within 1s", "Init:ImagePullBackOff", "Failed to pull image", "vxlan device already exists"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestWaitFlannelReadyBoundsDiagnostics(t *testing.T) {
	// The diagnostic pass has its own small budget: a wedged API server
	// after a failed rollout must not turn the failure into a hang, and
	// the pod listing gathered before the budget ran out must survive.
	saved := flannelDiagnosticBudget
	flannelDiagnosticBudget = 500 * time.Millisecond
	defer func() { flannelDiagnosticBudget = saved }()

	m := fakeFlannelContainer(t)
	t.Setenv("KIAC_TEST_ROLLOUT_FAIL", "1")
	t.Setenv("KIAC_TEST_EVENTS_SLEEP", "10")
	t.Setenv("KIAC_TEST_LOGS_SLEEP", "10")
	started := time.Now()
	err := m.waitFlannelReady("kiac-x-control-plane", time.Second)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("waitFlannelReady returned success for a failed rollout")
	}
	if elapsed > flannelDiagnosticBudget+2*time.Second {
		t.Fatalf("diagnostics took %s, want them bounded by %s", elapsed, flannelDiagnosticBudget)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Init:ImagePullBackOff") {
		t.Errorf("pod listing gathered before the budget ran out was lost:\n%s", msg)
	}
	if !strings.Contains(msg, "events: failed") && !strings.Contains(msg, "events: skipped") {
		t.Errorf("stuck events request is not reported as bounded:\n%s", msg)
	}
	if !strings.Contains(msg, "logs: skipped") && !strings.Contains(msg, "logs: failed") {
		t.Errorf("logs after an exhausted budget are not reported as skipped:\n%s", msg)
	}
	if strings.Contains(msg, "vxlan device already exists") {
		t.Errorf("logs were fetched after the diagnostic budget was spent:\n%s", msg)
	}
}

func TestWaitFlannelReadyPreservesPartialDiagnostics(t *testing.T) {
	// A successful pod listing followed by a failing logs request used to
	// discard everything, because the shell chain's status was the last
	// command's. Each diagnostic now stands on its own: the listing (with
	// its init-container state) is kept, and whatever a failing command
	// printed before it died is kept too.
	m := fakeFlannelContainer(t)
	t.Setenv("KIAC_TEST_ROLLOUT_FAIL", "1")
	t.Setenv("KIAC_TEST_EVENTS_FAIL", "1")
	t.Setenv("KIAC_TEST_LOGS_FAIL", "1")
	err := m.waitFlannelReady("kiac-x-control-plane", time.Second)
	if err == nil {
		t.Fatal("waitFlannelReady returned success for a failed rollout")
	}
	msg := err.Error()
	for _, want := range []string{
		"did not become ready within 1s",
		"Init:ImagePullBackOff",
		"partial line before failure",
		"events: failed",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
}

func TestWaitFlannelReadySucceedsWithoutDiagnostics(t *testing.T) {
	m := fakeFlannelContainer(t)
	if err := m.waitFlannelReady("kiac-x-control-plane", time.Second); err != nil {
		t.Fatalf("waitFlannelReady on a rolled-out DaemonSet = %v, want nil", err)
	}
}

type flannelExecCall struct {
	timeout time.Duration
	args    []string
}

func TestWaitFlannelReadyBoundsEveryCommand(t *testing.T) {
	// Every command the wait issues carries all three bounds: an outer
	// exec deadline, kubectl's --request-timeout for single API calls,
	// and (for the rollout) kubectl's own --timeout equal to --wait.
	var calls []flannelExecCall
	exec := func(name string, timeout time.Duration, command ...string) (string, error) {
		calls = append(calls, flannelExecCall{timeout, command})
		if name != "kiac-x-control-plane" {
			t.Fatalf("command went to %q", name)
		}
		if strings.Contains(strings.Join(command, " "), "rollout status") {
			return "", errors.New("timed out")
		}
		return "", nil
	}
	wait := 90 * time.Second
	if err := waitFlannelRollout("kiac-x-control-plane", wait, exec); err == nil {
		t.Fatal("want an error for a timed-out rollout")
	}
	if len(calls) != 4 {
		t.Fatalf("got %d commands, want rollout + 3 diagnostics: %+v", len(calls), calls)
	}
	rollout := calls[0]
	if rollout.timeout != wait+flannelRolloutGrace {
		t.Errorf("rollout exec bound = %s, want --wait %s + grace %s", rollout.timeout, wait, flannelRolloutGrace)
	}
	joined := strings.Join(rollout.args, " ")
	for _, want := range []string{"--request-timeout=90s", "--timeout=90s", "rollout status daemonset/kube-flannel-ds"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rollout command lacks %q: %s", want, joined)
		}
	}
	for _, c := range calls[1:] {
		if c.timeout <= 0 || c.timeout > flannelDiagnosticBudget {
			t.Errorf("diagnostic exec bound = %s, want within (0, %s]", c.timeout, flannelDiagnosticBudget)
		}
		if !strings.Contains(strings.Join(c.args, " "), "--request-timeout=") {
			t.Errorf("diagnostic command has no --request-timeout: %v", c.args)
		}
	}
	logs := strings.Join(calls[3].args, " ")
	for _, want := range []string{"logs", "--all-containers", "--ignore-errors", "-l app=flannel"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs diagnostic lacks %q (init-container logs would be missed): %s", want, logs)
		}
	}
}

func TestWaitFlannelReadyFloorsSubSecondBudget(t *testing.T) {
	var calls []flannelExecCall
	exec := func(_ string, timeout time.Duration, command ...string) (string, error) {
		calls = append(calls, flannelExecCall{timeout, command})
		return "", nil
	}
	if err := waitFlannelRollout("cp", 200*time.Millisecond, exec); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("a successful rollout issued %d commands, want only the rollout (no diagnostics)", len(calls))
	}
	joined := strings.Join(calls[0].args, " ")
	if !strings.Contains(joined, "--timeout=1s") || !strings.Contains(joined, "--request-timeout=1s") {
		t.Fatalf("sub-second --wait must floor kubectl budgets at 1s, got: %s", joined)
	}
	// The outer bound must be derived from the floored 1s kubectl gets,
	// not the raw 200ms, or the grace is eaten by the rounding and the
	// exec is killed before kubectl's own timer can fire.
	if want := time.Second + flannelRolloutGrace; calls[0].timeout != want {
		t.Fatalf("outer exec bound = %s, want %s", calls[0].timeout, want)
	}
}

func TestWaitFlannelReadyLargeBudgetFormatting(t *testing.T) {
	var calls []flannelExecCall
	exec := func(_ string, timeout time.Duration, command ...string) (string, error) {
		calls = append(calls, flannelExecCall{timeout, command})
		return "", nil
	}
	if err := waitFlannelRollout("cp", 24*time.Hour, exec); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls[0].args, " ")
	if !strings.Contains(joined, "--timeout=86400s") || !strings.Contains(joined, "--request-timeout=86400s") {
		t.Fatalf("large --wait formatted wrong: %s", joined)
	}
	if calls[0].timeout != 24*time.Hour+flannelRolloutGrace {
		t.Fatalf("outer exec bound = %s", calls[0].timeout)
	}
}

func TestFlannelDiagnosticsSkipsAfterBudgetExhausted(t *testing.T) {
	// Deterministic version of the budget test: the events call sleeps
	// its whole (remaining) budget away, so logs must be skipped with the
	// explicit note and never issued. synctest makes the sleep free.
	synctest.Test(t, func(t *testing.T) {
		var issued []string
		exec := func(_ string, timeout time.Duration, command ...string) (string, error) {
			joined := strings.Join(command, " ")
			issued = append(issued, joined)
			switch {
			case strings.Contains(joined, "get pods"):
				return "kube-flannel-ds-abc12   0/1   Init:ImagePullBackOff", nil
			case strings.Contains(joined, "get events"):
				time.Sleep(timeout)
				return "", context.DeadlineExceeded
			}
			return "unexpected", nil
		}
		diag := flannelDiagnostics("cp", exec)
		if len(issued) != 2 {
			t.Fatalf("issued %d commands after the budget was spent, want pods + events only: %v", len(issued), issued)
		}
		for _, want := range []string{
			"pods:\nkube-flannel-ds-abc12   0/1   Init:ImagePullBackOff",
			"recent events: failed: context deadline exceeded",
			"logs: skipped, diagnostic budget of " + flannelDiagnosticBudget.String() + " exhausted",
		} {
			if !strings.Contains(diag, want) {
				t.Errorf("diagnostics lack %q:\n%s", want, diag)
			}
		}
	})
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc", 2); got != "b\nc" {
		t.Fatalf("lastLines = %q", got)
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Fatalf("lastLines short input = %q", got)
	}
}

func TestValidateCNIFailsBeforeBoot(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string // "" means accepted
	}{
		{Config{}, ""},
		{Config{CNI: "kindnet"}, ""},
		{Config{CNI: "none"}, ""},
		{Config{CNI: "flannel", Kernel: "/k/Image"}, ""},
		{Config{CNI: "flannel"}, "--kernel full"},
		{Config{CNI: "cilium"}, "--kernel full"},
		{Config{CNI: "calico", Kernel: "/k/Image"}, "--cni flannel"},
		{Config{CNI: "bogus"}, "unknown --cni"},
		{Config{CNI: "kindnet; rm -rf /"}, "unknown --cni"},
	}
	for _, c := range cases {
		err := validateCNI(c.cfg)
		if c.want == "" {
			if err != nil {
				t.Errorf("validateCNI(%+v) = %v, want accepted", c.cfg, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("validateCNI(%+v) = %v, want error containing %q", c.cfg, err, c.want)
		}
	}
}

func TestExtractCNIPluginsRefusesUnexpectedMembers(t *testing.T) {
	m := NewManager()
	for _, member := range []string{"bridge", "../bridge", "./bridge; reboot", "./a b", "./$(id)", ""} {
		err := m.extractCNIPlugins("node", "/nonexistent.tgz", member)
		if err == nil || !strings.Contains(err.Error(), "refusing CNI plugin archive member") {
			t.Errorf("member %q: err = %v, want refusal before any exec", member, err)
		}
	}
	if !cniPluginMember.MatchString("./bridge") || !cniPluginMember.MatchString("./host-local") {
		t.Fatal("legitimate members must pass the guard")
	}
	for _, member := range flannelDelegatePlugins {
		if !cniPluginMember.MatchString(member) {
			t.Fatalf("flannel delegate %q would be refused by the guard", member)
		}
	}
}
