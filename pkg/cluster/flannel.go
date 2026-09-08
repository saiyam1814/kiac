package cluster

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/ui"
)

// FlannelVersion is the pinned upstream flannel release embedded in the
// binary. Bump it together with the manifest: `make flannel-manifest
// FLANNEL_VERSION=vX.Y.Z` regenerates assets/flannel.yaml with digests
// and probes, then re-verify the delegate plugin set below.
const FlannelVersion = "v0.28.9"

// flannelManifest is upstream kube-flannel.yml at FlannelVersion with
// two local edits, both applied by internal/cmd/flannel-manifest so a
// version bump reproduces them: the @sha256 digests on the image
// references (mutable tags alone would let a retag change what every
// node runs as root), and the liveness/readiness probes upstream ships
// in its Documentation copy but not in the release asset, without which
// `kubectl rollout status` would mean Running rather than healthy. The
// probes keep upstream's healthz listener on 0.0.0.0:8081 (hostNetwork,
// so on every node IP): it serves only /healthz and /readyz status
// bodies, and pinning it to loopback would be a second deviation from
// upstream for no data exposure gained. The pod network is patched at
// apply time (see flannelManifestWithCIDR), not here, so the embedded
// bytes stay diffable against upstream.
//
//go:embed assets/flannel.yaml
var flannelManifest string

// flannelUpstreamCIDR is the pod network upstream ships in net-conf.json.
// It happens to equal kiac's kubeadm IPv4 pod CIDR, but the apply path
// still rewrites it from Config so the two can never drift apart.
const flannelUpstreamCIDR = `"Network": "10.244.0.0/16"`

// flannelManifestWithCIDR returns the embedded manifest with its pod
// network set to cidr. It fails loudly if the upstream marker is gone -
// a version bump that reshapes net-conf.json must be noticed, not
// silently applied with the wrong network.
func flannelManifestWithCIDR(cidr string) (string, error) {
	patched := strings.Replace(flannelManifest, flannelUpstreamCIDR, `"Network": "`+cidr+`"`, 1)
	if patched == flannelManifest && !strings.Contains(flannelManifest, `"Network": "`+cidr+`"`) {
		return "", fmt.Errorf("embedded flannel manifest %s has no %s marker; the manifest changed shape and needs review", FlannelVersion, flannelUpstreamCIDR)
	}
	return patched, nil
}

// flannelDelegatePlugins are the CNI binaries flannel's conflist needs
// but the kindest/node image does not ship. The conflist delegates to
// bridge (isDefaultGateway) with host-local IPAM and chains portmap; the
// node image already carries host-local, portmap, and loopback for
// kindnet, leaving only bridge. Bridge is safe here because flannel is
// gated on the full kernel, which enables br_netfilter; without it
// bridged same-node Service traffic would bypass iptables un-DNAT (the
// reason the k3s path avoids bridge on the stock kernel).
var flannelDelegatePlugins = []string{"./bridge"}

// installFlannel applies the embedded, version-pinned upstream flannel
// manifest. Flannel's VXLAN backend needs the full custom kernel (the
// stock node kernel has no VXLAN or br_netfilter), and the create path
// already refused to boot without one; this re-check keeps the
// invariant local. VXLAN carries cross-node pod traffic in node-IP
// -addressed packets, which take vmnet's fast path - the same reason
// Cilium runs its tunnel datapath here.
func (m *Manager) installFlannel(cp string, cfg Config) error {
	if cfg.Kernel == "" {
		return fmt.Errorf("--cni flannel needs the full node kernel: add --kernel full (or a --kernel path)")
	}
	nodes := []string{cp}
	for i := 1; i <= cfg.Workers; i++ {
		nodes = append(nodes, worker(cfg.Name, i))
	}
	return ui.Step(fmt.Sprintf("Installing CNI (flannel %s)", FlannelVersion), func() error {
		// Resolve the shared archive once before fanning out, so a cold
		// cache downloads it a single time instead of once per node.
		archive, err := resolveCNIPluginsArchive()
		if err != nil {
			return err
		}
		if err := inParallel(len(nodes), func(i int) error {
			if err := m.extractCNIPlugins(nodes[i], archive, cfg.WaitTimeout, flannelDelegatePlugins...); err != nil {
				return fmt.Errorf("installing bridge CNI plugin on %s: %w", nodes[i], err)
			}
			return nil
		}); err != nil {
			return err
		}
		manifest, err := flannelManifestWithCIDR(cfg.family().podCIDR(kubeadmPodCIDRv4, kubeadmPodCIDRv6))
		if err != nil {
			return err
		}
		// Bounded like the extraction: an apply that wedges must not hang
		// create past its budget.
		if err := m.rt.ExecStdinTimeout(cp, transferBudget(cfg.WaitTimeout), strings.NewReader(manifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-"); err != nil {
			return err
		}
		return m.waitFlannelReady(cp, cfg.WaitTimeout)
	})
}

// flannelRolloutGrace is how long past --wait the outer container exec
// may live. kubectl enforces --timeout itself and normally exits first
// with its own message; the grace only covers its startup (discovery,
// REST mapping) so a rollout that completes right at the budget is not
// cut off, while a wedged exec is still bounded instead of hanging.
const flannelRolloutGrace = time.Second

// flannelDiagnosticBudget bounds the whole post-timeout diagnostic pass
// (pod list, events, logs). It is separate from and much smaller than
// the rollout budget: --wait has already been spent by the time it
// runs, and a wedged API server must not turn a failed rollout into a
// hang. A var so tests can shrink it.
var flannelDiagnosticBudget = 10 * time.Second

// execFunc is the shape of runtime.Client.ExecTimeout: a bounded command
// inside a node, returning combined output even on failure.
type execFunc func(name string, timeout time.Duration, command ...string) (string, error)

// waitFlannelReady blocks until the kube-flannel DaemonSet is rolled
// out on every node, honoring the configured wait timeout. On timeout
// the error carries the pod list, recent events, and container logs,
// so a failure names the crashing pod instead of just "timed out".
func (m *Manager) waitFlannelReady(cp string, timeout time.Duration) error {
	return waitFlannelRollout(cp, timeout, m.rt.ExecTimeout)
}

// waitFlannelRollout is waitFlannelReady with the bounded exec injected,
// so the timing contract can be tested without a node VM.
func waitFlannelRollout(cp string, timeout time.Duration, exec execFunc) error {
	// `kubectl rollout status --timeout=0s` waits forever. The CLI and
	// config file reject a non-positive --wait before Create runs, so
	// this is a guard for direct callers, not a documented mode: skip
	// the bounded wait rather than block forever; the final nodes-Ready
	// step still runs. A sub-second budget would truncate to 0s, so
	// floor it at 1s to keep the fail-fast intent.
	if timeout <= 0 {
		return nil
	}
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	budget := time.Duration(seconds) * time.Second
	// Three nested bounds, innermost first: --request-timeout caps any
	// single API call, --timeout caps the rollout wait, and the outer
	// exec deadline caps the `container exec` itself so a wedged exec
	// session cannot outlive --wait. The outer bound is derived from the
	// same floored budget kubectl receives, so the grace is never eaten
	// by the rounding.
	_, err := exec(cp, budget+flannelRolloutGrace, "kubectl", "--kubeconfig", adminConf,
		fmt.Sprintf("--request-timeout=%ds", seconds),
		"-n", "kube-flannel", "rollout", "status", "daemonset/kube-flannel-ds",
		fmt.Sprintf("--timeout=%ds", seconds))
	if err == nil {
		return nil
	}
	diag := flannelDiagnostics(cp, exec)
	if diag == "" {
		return fmt.Errorf("flannel did not become ready within %s: %w", timeout, err)
	}
	return fmt.Errorf("flannel did not become ready within %s: %w\n%s", timeout, err, diag)
}

// flannelDiagnostics gathers what a failed rollout needs to be diagnosed
// before create tears the cluster down: the pod list (whose STATUS
// column names init-container failures such as Init:ImagePullBackOff),
// recent namespace events (which carry the pull or crash reason), and
// the tail of every container's logs, init containers included. Each
// command is bounded separately under one shared budget, and whatever
// output a command produced is kept even when it fails, so a broken
// logs request cannot discard a pod listing that already explains the
// failure.
func flannelDiagnostics(cp string, exec execFunc) string {
	kubectl := func(requestTimeout time.Duration, args ...string) []string {
		return append([]string{"kubectl", "--kubeconfig", adminConf,
			fmt.Sprintf("--request-timeout=%ds", max(1, int(requestTimeout.Seconds()))),
			"-n", "kube-flannel"}, args...)
	}
	sections := []struct {
		label string
		tail  int // trailing lines to keep; 0 keeps everything
		args  []string
	}{
		{"pods", 0, []string{"get", "pods", "-o", "wide"}},
		// creationTimestamp is set on every event; lastTimestamp is null
		// for events.k8s.io-recorded ones, which would sort first and be
		// the ones the tail drops.
		{"recent events", 20, []string{"get", "events", "--sort-by=.metadata.creationTimestamp"}},
		// Ten lines per container, capped overall so a large cluster
		// still yields a readable error.
		{"logs", 200, []string{"logs", "-l", "app=flannel", "--all-containers", "--prefix", "--ignore-errors", "--tail=10"}},
	}
	deadline := time.Now().Add(flannelDiagnosticBudget)
	var out []string
	for _, section := range sections {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			out = append(out, fmt.Sprintf("%s: skipped, diagnostic budget of %s exhausted", section.label, flannelDiagnosticBudget))
			continue
		}
		text, err := exec(cp, remaining, kubectl(remaining, section.args...)...)
		text = strings.TrimSpace(text)
		if section.tail > 0 {
			text = lastLines(text, section.tail)
		}
		switch {
		case text != "":
			out = append(out, section.label+":\n"+text)
		case err != nil:
			out = append(out, section.label+": "+diagnosticError(err))
		}
	}
	return strings.Join(out, "\n")
}

// lastLines keeps the trailing n lines of s, so a long listing stays
// readable inside an error message.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
