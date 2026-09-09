package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		if strings.HasPrefix(image, "ghcr.io/flannel-io/flannel:") && !strings.HasSuffix(image, ":"+FlannelVersion) {
			t.Fatalf("flannel image %q is not pinned to %s", image, FlannelVersion)
		}
		if !strings.Contains(image, ":v") {
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
		err := m.extractCNIPlugins("node", "/nonexistent.tgz", time.Minute, member)
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

// fakeFlannelInstallContainer stands in for `container` across the whole
// flannel install: bridge extraction on every node (ExecStdin), the
// manifest apply (ExecStdin, stdin saved to KIAC_TEST_MANIFEST), and the
// rollout wait. Every command line is appended to KIAC_TEST_LOG.
func fakeFlannelInstallContainer(t *testing.T) (*Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "container")
	log := filepath.Join(dir, "commands.log")
	manifest := filepath.Join(dir, "applied.yaml")
	script := `#!/bin/sh
echo "$*" >> "$KIAC_TEST_LOG"
case "$*" in
  *"tar -x -C /opt/cni/bin"*)
    cat > "$KIAC_TEST_LOG.$3.tar"
    if [ -n "${KIAC_TEST_FAIL_NODE:-}" ] && [ "$3" = "${KIAC_TEST_FAIL_NODE}" ]; then
      echo "tar: ./bridge: Not found in archive" >&2
      exit 2
    fi
    ;;
  *"apply -f -"*)
    cat > "$KIAC_TEST_MANIFEST"
    echo "namespace/kube-flannel created"
    ;;
  *"rollout status"*)
    echo 'daemon set "kube-flannel-ds" successfully rolled out'
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
	t.Setenv("KIAC_TEST_LOG", log)
	t.Setenv("KIAC_TEST_MANIFEST", manifest)
	t.Setenv("KIAC_TEST_FAIL_NODE", "")
	archive := writeFakeCNIArchive(t, filepath.Join(dir, "cni-plugins.tgz"), "./bridge", "./ptp", "./host-local")
	saved := resolveCNIPluginsArchive
	resolveCNIPluginsArchive = func() (string, error) { return archive, nil }
	t.Cleanup(func() { resolveCNIPluginsArchive = saved })
	return &Manager{rt: &runtime.Client{Bin: bin}}, log, manifest
}

func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestInstallFlannelStreamsBridgeAndAppliesPatchedManifest(t *testing.T) {
	m, log, manifest := fakeFlannelInstallContainer(t)
	cfg := Config{Name: "x", Workers: 2, Kernel: "/kernels/Image", WaitTimeout: time.Second}
	if err := m.installFlannel("kiac-x-control-plane", cfg); err != nil {
		t.Fatalf("installFlannel: %v", err)
	}
	lines := readLogLines(t, log)
	var tars, applies, rollouts int
	lastTar, firstApply := -1, -1
	for i, line := range lines {
		switch {
		case strings.Contains(line, "tar -x -C /opt/cni/bin"):
			tars++
			lastTar = i
			if strings.Contains(line, "./") {
				t.Errorf("member names must not appear on the node's command line: %s", line)
			}
		case strings.Contains(line, "apply -f -"):
			applies++
			if firstApply < 0 {
				firstApply = i
			}
		case strings.Contains(line, "rollout status daemonset/kube-flannel-ds"):
			rollouts++
		}
	}
	if tars != 3 {
		t.Errorf("bridge extracted on %d nodes, want control plane + 2 workers:\n%s", tars, strings.Join(lines, "\n"))
	}
	if applies != 1 || rollouts != 1 {
		t.Errorf("applies = %d, rollouts = %d, want 1 each:\n%s", applies, rollouts, strings.Join(lines, "\n"))
	}
	if firstApply < lastTar {
		t.Errorf("manifest applied before every node had the bridge plugin:\n%s", strings.Join(lines, "\n"))
	}
	for _, node := range []string{"kiac-x-control-plane", "kiac-x-worker-1", "kiac-x-worker-2"} {
		if !strings.Contains(strings.Join(lines, "\n"), "exec -i "+node+" /bin/sh") {
			t.Errorf("no extraction on %s", node)
		}
		// Each node received a tar holding only the bridge delegate.
		if got := tarMemberNames(t, log+"."+node+".tar"); len(got) != 1 || got[0] != "./bridge" {
			t.Errorf("%s received members %v, want only ./bridge", node, got)
		}
	}
	applied, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("applied manifest was not streamed to kubectl: %v", err)
	}
	if !strings.Contains(string(applied), `"Network": "`+kubeadmPodCIDRv4+`"`) {
		t.Error("applied manifest does not carry the kubeadm pod CIDR")
	}
	if !strings.Contains(string(applied), "ghcr.io/flannel-io/flannel:"+FlannelVersion) {
		t.Error("applied manifest lost the pinned flannel image")
	}
}

func TestInstallFlannelNamesFailingNode(t *testing.T) {
	// A bridge extraction that fails on one worker must name that node
	// and stop before the manifest is applied, since flannel pods on
	// that node would otherwise crash with a missing delegate plugin.
	m, log, manifest := fakeFlannelInstallContainer(t)
	t.Setenv("KIAC_TEST_FAIL_NODE", "kiac-x-worker-2")
	cfg := Config{Name: "x", Workers: 2, Kernel: "/kernels/Image", WaitTimeout: time.Second}
	err := m.installFlannel("kiac-x-control-plane", cfg)
	if err == nil {
		t.Fatal("installFlannel succeeded with a node missing the bridge plugin")
	}
	for _, want := range []string{"installing bridge CNI plugin on kiac-x-worker-2", "Not found in archive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(manifest); statErr == nil {
		t.Error("manifest was applied even though a node lacks the bridge plugin")
	}
	for _, line := range readLogLines(t, log) {
		if strings.Contains(line, "rollout status") {
			t.Error("rollout wait ran after the extraction failed")
		}
	}
}

func TestInstallFlannelFailsWhenArchiveUnavailable(t *testing.T) {
	// A missing or corrupt plugin archive (cache tampering, offline
	// first run) fails before any node is touched.
	m, log, _ := fakeFlannelInstallContainer(t)
	resolveCNIPluginsArchive = func() (string, error) { return "", errors.New("cni plugins archive: sha256 mismatch") }
	cfg := Config{Name: "x", Workers: 1, Kernel: "/kernels/Image", WaitTimeout: time.Second}
	err := m.installFlannel("kiac-x-control-plane", cfg)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want the archive failure", err)
	}
	if lines := readLogLines(t, log); len(lines) != 0 {
		t.Fatalf("nodes were touched before the archive was verified: %v", lines)
	}
}

func writeFakeCNIArchive(t *testing.T, path string, members ...string) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entries := append([]string{"./README.md"}, members...)
	for _, name := range entries {
		body := []byte("binary for " + name)
		mode := int64(0o755)
		if strings.HasSuffix(name, ".md") {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func tarMemberNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("no tar was streamed to %s: %v", path, err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("streamed payload is not a tar: %v", err)
		}
		if hdr.Mode&0o111 == 0 {
			t.Errorf("member %s lost its executable bit (mode %o)", hdr.Name, hdr.Mode)
		}
		names = append(names, hdr.Name)
	}
}

func TestSelectCNIPluginMembers(t *testing.T) {
	archive := writeFakeCNIArchive(t, filepath.Join(t.TempDir(), "plugins.tgz"), "./bridge", "./ptp", "./host-local", "./portmap", "./bandwidth", "./loopback")
	payload, err := selectCNIPluginMembers(archive, []string{"./bridge"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(payload))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "./bridge" || hdr.Mode != 0o755 {
		t.Fatalf("first member = %+v, %v; want ./bridge mode 0755", hdr, err)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != "binary for ./bridge" {
		t.Fatalf("member body = %q", body)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatal("payload carries more than the requested member")
	}

	// The k3s set selects five members and leaves the README behind.
	payload, err = selectCNIPluginMembers(archive, []string{"./loopback", "./ptp", "./host-local", "./portmap", "./bandwidth"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr = tar.NewReader(bytes.NewReader(payload))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		names = append(names, hdr.Name)
	}
	if len(names) != 5 || strings.Contains(strings.Join(names, " "), "README") {
		t.Fatalf("k3s selection = %v", names)
	}

	if _, err := selectCNIPluginMembers(archive, []string{"./bridge", "./calico"}); err == nil || !strings.Contains(err.Error(), `no member "./calico"`) {
		t.Fatalf("missing member must be reported before any node is touched, got: %v", err)
	}
	if _, err := selectCNIPluginMembers(filepath.Join(t.TempDir(), "missing.tgz"), []string{"./bridge"}); err == nil {
		t.Fatal("missing archive must fail")
	}
}

// recordingRuntime embeds a nil HostRuntime: any call outside the three
// methods below panics, so an unexpected runtime call cannot silently
// succeed. It records what the install streams and how it is bounded.
type recordingRuntime struct {
	runtime.HostRuntime
	mu        sync.Mutex
	transfers []recordedTransfer // ExecStdinTimeout
	execs     []recordedTransfer // ExecTimeout
	unbounded []string           // ExecStdin: the path the install must never take
}

type recordedTransfer struct {
	node    string
	timeout time.Duration
	command []string
	members []string // tar members when the payload was a tar
}

func (r *recordingRuntime) ExecStdinTimeout(node string, timeout time.Duration, in io.Reader, command ...string) error {
	payload, _ := io.ReadAll(in)
	var members []string
	tr := tar.NewReader(bytes.NewReader(payload))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		members = append(members, hdr.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transfers = append(r.transfers, recordedTransfer{node, timeout, command, members})
	return nil
}

func (r *recordingRuntime) ExecStdin(node string, in io.Reader, command ...string) error {
	_, _ = io.ReadAll(in)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unbounded = append(r.unbounded, node+": "+strings.Join(command, " "))
	return nil
}

func (r *recordingRuntime) ExecTimeout(node string, timeout time.Duration, command ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, recordedTransfer{node: node, timeout: timeout, command: command})
	return `daemon set "kube-flannel-ds" successfully rolled out`, nil
}

func stubCNIPluginsArchive(t *testing.T) {
	t.Helper()
	archive := writeFakeCNIArchive(t, filepath.Join(t.TempDir(), "plugins.tgz"), "./bridge", "./loopback", "./ptp", "./host-local", "./portmap", "./bandwidth")
	saved := resolveCNIPluginsArchive
	resolveCNIPluginsArchive = func() (string, error) { return archive, nil }
	t.Cleanup(func() { resolveCNIPluginsArchive = saved })
}

func TestInstallFlannelBoundsEveryTransfer(t *testing.T) {
	// The install must never take the unbounded exec, every transfer
	// must carry transferBudget(--wait), and the rollout wait --wait plus
	// its grace. A regression to ExecStdin or a dropped budget fails here.
	stubCNIPluginsArchive(t)
	for _, wait := range []time.Duration{time.Second, 2 * time.Hour} {
		rt := &recordingRuntime{}
		m := &Manager{rt: rt}
		if err := m.installFlannel("kiac-x-control-plane", Config{Name: "x", Workers: 2, Kernel: "/k/Image", WaitTimeout: wait}); err != nil {
			t.Fatal(err)
		}
		if len(rt.unbounded) != 0 {
			t.Fatalf("install used the unbounded ExecStdin: %v", rt.unbounded)
		}
		if len(rt.transfers) != 4 {
			t.Fatalf("%d transfers, want 3 extractions + 1 apply", len(rt.transfers))
		}
		for _, tr := range rt.transfers {
			if tr.timeout != transferBudget(wait) {
				t.Errorf("--wait %s: %s bounded by %s, want %s", wait, tr.node, tr.timeout, transferBudget(wait))
			}
			if len(tr.members) > 0 && (len(tr.members) != 1 || tr.members[0] != "./bridge") {
				t.Errorf("%s received members %v, want only ./bridge", tr.node, tr.members)
			}
		}
		if len(rt.execs) != 1 || rt.execs[0].timeout != max(wait, time.Second)+flannelRolloutGrace {
			t.Errorf("--wait %s: rollout execs = %+v", wait, rt.execs)
		}
	}
}

func TestEnsureK3sCNIPluginsIsBounded(t *testing.T) {
	stubCNIPluginsArchive(t)
	for _, wait := range []time.Duration{30 * time.Second, 3 * time.Hour} {
		rt := &recordingRuntime{}
		if err := (&Manager{rt: rt}).ensureK3sCNIPlugins("kiac-x-worker-1", wait); err != nil {
			t.Fatal(err)
		}
		if len(rt.unbounded) != 0 || len(rt.transfers) != 1 {
			t.Fatalf("unbounded=%v transfers=%+v", rt.unbounded, rt.transfers)
		}
		tr := rt.transfers[0]
		if tr.node != "kiac-x-worker-1" || tr.timeout != transferBudget(wait) {
			t.Errorf("bounded by %s on %s, want %s", tr.timeout, tr.node, transferBudget(wait))
		}
		want := []string{"./loopback", "./ptp", "./host-local", "./portmap", "./bandwidth"}
		if strings.Join(tr.members, " ") != strings.Join(want, " ") {
			t.Errorf("k3s payload members = %v, want %v", tr.members, want)
		}
	}
}
