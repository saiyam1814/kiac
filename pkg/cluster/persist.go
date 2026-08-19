package cluster

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

// This file implements `kiac resume`: bringing a cluster back after its
// VMs died with the container system service (host reboot, `container
// system stop`). apple/container persists a stopped VM's rootfs but
// hands it a fresh vmnet IP on every boot, so the control plane comes
// back on an address its cert, kubeconfigs, kube-proxy ConfigMap, and
// the workers' kubelets all disagree with. The investigation behind
// each healing step is in docs/design/persistent-clusters.md.

// Resume boots every node VM of a cluster and heals the state a new set
// of vmnet IPs invalidates. Safe to re-run: nodes already running are
// left alone and every healing step is a no-op once applied, so resume
// doubles as the retry after a partial failure.
func (m *Manager) Resume(name string, waitTimeout time.Duration) error {
	start := time.Now()
	if !ValidName(name) {
		return fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", name)
	}
	if waitTimeout <= 0 {
		waitTimeout = 5 * time.Minute
	}
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		return fmt.Errorf("no cluster named %q found", name)
	}
	if distroFromNodes(infos) == "k3s" {
		return m.resumeK3s(name, infos, waitTimeout, start)
	}
	cp, workers, err := orderNodes(name, infos)
	if err != nil {
		return err
	}

	if err := ui.Step("Preflight checks", func() error { return m.preflight() }); err != nil {
		return err
	}

	nodes := append([]string{cp}, workers...)
	running := map[string]bool{}
	for _, i := range infos {
		if strings.EqualFold(i.Status, "running") {
			running[i.Name] = true
		}
	}
	if err := ui.Step(fmt.Sprintf("Booting %d node VM(s)", len(nodes)), func() error {
		for _, n := range nodes {
			if running[n] {
				continue
			}
			if err := m.rt.Start(n); err != nil {
				return err
			}
		}
		// Nodes boot independently; readiness and the sysctl run
		// concurrently, mirroring Create.
		return inParallel(len(nodes), func(i int) error {
			n := nodes[i]
			if err := m.bootAndWait(n, waitTimeout); err != nil {
				return err
			}
			// ip_forward and NIC offloads are runtime state; the reboot
			// reset both.
			if _, err := m.rt.Exec(n, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
				return err
			}
			if _, err := m.rt.Exec(n, "sh", "-c", senderOffloadFix); err != nil {
				return err
			}
			// A dual-stack node's kubelet --node-ip pins the OLD v4 and v6
			// addresses; the reboot changed both, so re-pin from the
			// node's current addresses before the control-plane heal
			// restarts kubelet. A no-op on IPv4-only clusters (nothing was
			// pinned).
			return m.repinNodeIPIfNeeded(n)
		})
	}); err != nil {
		return err
	}

	newIP, err := m.rt.IP(cp)
	if err != nil {
		return err
	}
	// Both IPs are interpolated into shell run as root in the nodes, so
	// insist on the plain dotted-quad shape even though rt.IP validates.
	if !dottedQuadRe.MatchString(newIP) {
		return fmt.Errorf("control-plane IP %q is not an IPv4 address", newIP)
	}
	adminRaw, err := m.rt.Exec(cp, "cat", adminConf)
	if err != nil {
		return err
	}
	// admin.conf is the one file the node entrypoint's IP fixup never
	// rewrites, which makes it the durable record of the pre-reboot IP.
	oldIP, err := apiServerIP(adminRaw)
	if err != nil {
		// IPv6-only clusters point the endpoint at a bracketed v6 literal,
		// which the v4-based heal below cannot rewrite; fail with a clear
		// path instead of a confusing regex error. dual and ipv4 stay v4.
		if strings.Contains(adminRaw, "https://[") {
			return fmt.Errorf("resume does not yet support --ip-family ipv6 clusters (the control-plane endpoint is IPv6); recreate it with: kiac delete cluster --name %s, then kiac create cluster --name %s --ip-family ipv6 ...", name, name)
		}
		return err
	}

	if oldIP != newIP {
		ui.Infof("control-plane IP changed %s -> %s; healing certs and kubeconfigs", oldIP, newIP)
		if err := ui.Step("Healing control-plane files", func() error {
			_, err := m.rt.Exec(cp, "sh", "-euc", healControlPlaneScript(oldIP, newIP))
			return err
		}); err != nil {
			return err
		}
	}

	if len(workers) > 0 {
		if err := ui.Step("Pointing worker kubelets at the control plane", func() error {
			return inParallel(len(workers), func(i int) error {
				_, err := m.rt.Exec(workers[i], "sh", "-euc", healWorkerScript(newIP))
				return err
			})
		}); err != nil {
			return err
		}
	}

	if err := ui.Step("Waiting for the API server", func() error {
		return m.waitAPIServer(cp, waitTimeout)
	}); err != nil {
		return err
	}

	// kube-proxy pods mount their kubeconfig from a ConfigMap that still
	// names the old IP, so they crash-loop until it is rewritten. Runs
	// unconditionally: the script no-ops when the address is current,
	// which also repairs a previous resume that died before this step.
	if err := ui.Step("Re-pointing kube-proxy and cluster-info", func() error {
		var lastErr error
		for attempt := 0; attempt < 6; attempt++ {
			if _, lastErr = m.rt.Exec(cp, "sh", "-euc", healConfigMapsScript(newIP)); lastErr == nil {
				return nil
			}
			time.Sleep(5 * time.Second)
		}
		return lastErr
	}); err != nil {
		return err
	}

	// Not fatal: a --cni none cluster never reports Ready, and a slow
	// machine may just need longer; the VMs and the apiserver are up.
	if err := ui.Step("Waiting for nodes to be Ready", func() error {
		_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
			"wait", "--for=condition=Ready", "nodes", "--all",
			fmt.Sprintf("--timeout=%ds", int(waitTimeout.Seconds())))
		return err
	}); err != nil {
		ui.Infof("nodes not Ready yet; expected on --cni none clusters, otherwise watch: kubectl get nodes -w")
	}

	// LoadBalancer IPs need no step here: kiac-lb runs kubectl against
	// admin.conf on every pass, so once that file is healed its next
	// pass re-points any ingress IP pinned to a dead node address.

	var kubeconfigPath string
	if err := ui.Step("Writing kubeconfig", func() error {
		raw, err := m.rt.Exec(cp, "cat", adminConf)
		if err != nil {
			return err
		}
		kubeconfigPath, err = mergeKubeconfig(name, raw, newIP)
		return err
	}); err != nil {
		return err
	}

	// Resume is the documented recovery for a control-plane VM the Mac
	// could not reach, so confirming host access came back is the whole
	// point of reporting here.
	reachable := false
	_ = ui.Step("Checking API server reachability from your Mac", func() error {
		reachable = hostReachAPI(newIP, 10*time.Second)
		if !reachable {
			return fmt.Errorf("host cannot reach %s:6443", newIP)
		}
		return nil
	})

	ui.Successf("Cluster %q resumed in %s.", name, time.Since(start).Round(time.Second))
	ui.Infof("context kiac-%s updated in %s", name, kubeconfigPath)
	if reachable {
		ui.Hintf("kubectl get nodes")
	} else {
		warnAPIUnreachable(cp, name, newIP)
	}
	return nil
}

// bootAndWait waits for a just-started node, restarting it once if the
// VM dies during boot. That death is expected on the first control-plane
// boot after an IP change: the kindest/node entrypoint regenerates the
// apiserver cert with `--config /kind/kubeadm.conf`, a file kind's own
// provisioning writes but kiac's flag-driven `kubeadm init` does not, so
// under the entrypoint's `set -o errexit` it deletes the certs and dies.
// The second boot survives (the now-missing cert makes the entrypoint
// skip fix_certificate) and healControlPlaneScript regenerates the cert.
func (m *Manager) bootAndWait(n string, timeout time.Duration) error {
	died, err := m.waitNodeBoot(n, timeout)
	if err != nil {
		return err
	}
	if !died {
		return nil
	}
	ui.Infof("node %s died during boot (expected once after an IP change); booting it again", n)
	if err := m.rt.Start(n); err != nil {
		return err
	}
	died, err = m.waitNodeBoot(n, timeout)
	if err != nil {
		return err
	}
	if died {
		return fmt.Errorf("node %s died during boot twice; check: container logs %s", n, n)
	}
	return nil
}

// waitNodeBoot waits like runtime.WaitReady but also reports the VM
// dying mid-boot, so the caller can restart it instead of burning the
// whole timeout on a container that is already stopped.
func (m *Manager) waitNodeBoot(name string, timeout time.Duration) (died bool, err error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := m.rt.Exec(name, "systemctl", "is-active", "containerd")
		if err == nil && strings.TrimSpace(out) == "active" {
			return false, nil
		}
		lastErr = err
		if infos, lerr := m.rt.List(name); lerr == nil {
			for _, i := range infos {
				if i.Name == name && !strings.EqualFold(i.Status, "running") {
					return true, nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return false, fmt.Errorf("node %s did not become ready: %w", name, lastErr)
	}
	return false, fmt.Errorf("node %s did not become ready in %s", name, timeout)
}

// waitAPIServer polls /readyz through the control plane's own admin.conf
// until the (possibly just re-certified) apiserver serves again. The
// static pod can sit in crash-loop backoff from before the heal, so the
// deadline is the caller's full wait timeout.
func (m *Manager) waitAPIServer(cp string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf, "get", "--raw", "/readyz")
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("API server did not become ready in %s: %w", timeout, lastErr)
}

var (
	// Tolerates the JSON form kubectl can emit alongside kubeadm's YAML.
	apiServerIPRe = regexp.MustCompile(`server"?:\s*"?https://(\d+\.\d+\.\d+\.\d+):6443`)
	dottedQuadRe  = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
)

// apiServerIP extracts the apiserver's IPv4 endpoint from a kubeconfig.
func apiServerIP(kubeconfig string) (string, error) {
	m := apiServerIPRe.FindStringSubmatch(kubeconfig)
	if m == nil {
		return "", fmt.Errorf("no IPv4 apiserver endpoint found in kubeconfig")
	}
	return m[1], nil
}

// orderNodes splits a cluster's containers into the control plane and
// its workers sorted by index, the same order Create booted them in.
func orderNodes(name string, infos []runtime.Info) (string, []string, error) {
	cp := ControlPlane(name)
	found := false
	type wk struct {
		idx  int
		name string
	}
	var ws []wk
	for _, i := range infos {
		if i.Name == cp {
			found = true
			continue
		}
		rest := strings.TrimPrefix(i.Name, prefix(name)+"worker-")
		if rest == i.Name {
			continue
		}
		idx, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		ws = append(ws, wk{idx, i.Name})
	}
	if !found {
		return "", nil, fmt.Errorf("cluster %q has no control-plane container %s; it cannot be resumed, only recreated", name, cp)
	}
	sort.Slice(ws, func(a, b int) bool { return ws[a].idx < ws[b].idx })
	names := make([]string, len(ws))
	for i, w := range ws {
		names[i] = w.name
	}
	return cp, names, nil
}

// healControlPlaneScript rewrites every control-plane file that pins the
// old IP and regenerates the apiserver serving cert when needed. It is
// correct whether or not the node entrypoint's own IP fixup ran at boot:
// every sed is idempotent, admin.conf and super-admin.conf are covered
// here because the entrypoint never rewrites them, and the cert is
// regenerated only when the manifest was still stale or the cert file is
// gone (the entrypoint's failed fix_certificate deletes it, see
// bootAndWait). The cert regen deliberately avoids `--config
// /kind/kubeadm.conf` (kiac never wrote it) and passes the advertise
// address instead; kubeadm's default SANs then match the original init.
// The kubelet restart makes it re-read the sed'd manifests and confs no
// matter when they changed, and the crictl kick skips the up-to-5-minute
// crash-loop backoff a cert-less apiserver accumulated before the heal.
// kiac-lb reads admin.conf through kubectl on every pass, so healing the
// file is enough; the restart only clears a failed/backoff unit state
// (guarded: --no-lb clusters never installed the unit).
func healControlPlaneScript(oldIP, newIP string) string {
	old := strings.ReplaceAll(oldIP, ".", `\.`)
	return fmt.Sprintf(`files="/etc/kubernetes/admin.conf /etc/kubernetes/super-admin.conf /etc/kubernetes/controller-manager.conf /etc/kubernetes/scheduler.conf /etc/kubernetes/kubelet.conf /var/lib/kubelet/kubeadm-flags.env /etc/kubernetes/manifests/etcd.yaml /etc/kubernetes/manifests/kube-apiserver.yaml /etc/kubernetes/manifests/kube-controller-manager.yaml /etc/kubernetes/manifests/kube-scheduler.yaml"
need_cert=no
if [ ! -f /etc/kubernetes/pki/apiserver.crt ]; then need_cert=yes; fi
if grep -q '\b%[1]s\b' /etc/kubernetes/manifests/kube-apiserver.yaml; then need_cert=yes; fi
for f in $files; do
  if [ -f "$f" ]; then sed -i 's#\b%[1]s\b#%[2]s#g' "$f"; fi
done
if [ "$need_cert" = yes ]; then
  rm -f /etc/kubernetes/pki/apiserver.crt /etc/kubernetes/pki/apiserver.key
  kubeadm init phase certs apiserver --apiserver-advertise-address %[2]s
fi
systemctl restart kubelet
if [ "$need_cert" = yes ] && command -v crictl >/dev/null 2>&1; then
  crictl rm -f $(crictl ps -a --name kube-apiserver -q) >/dev/null 2>&1 || true
fi
if systemctl is-enabled kiac-lb.service >/dev/null 2>&1; then
  systemctl restart kiac-lb.service
fi`, old, newIP)
}

// repinNodeIPIfNeeded rewrites a node's kubelet --node-ip after a reboot
// changed its vmnet addresses. It only acts on nodes that were created
// with a pinned node-ip (dual-stack or ipv6), detected from the existing
// /etc/default/kubelet, so IPv4-only clusters are untouched. The file is
// rewritten from the node's CURRENT addresses rather than sed'd from an
// old value, which is simpler and correct regardless of how many boots
// happened; kubelet is restarted only when the value actually changed.
func (m *Manager) repinNodeIPIfNeeded(node string) error {
	out, err := m.rt.Exec(node, "sh", "-c", "cat /etc/default/kubelet 2>/dev/null || true")
	if err != nil {
		return err
	}
	fam, ok := pinnedFamily(out)
	if !ok {
		return nil
	}
	if _, err := m.rt.Exec(node, "sh", "-c", ipv6BootPrep); err != nil {
		return err
	}
	nodeIP, err := m.nodeIPArg(node, fam)
	if err != nil {
		return err
	}
	desired := "KUBELET_EXTRA_ARGS=--node-ip=" + nodeIP
	if strings.Contains(out, desired) {
		return nil // already current
	}
	if _, err := m.rt.Exec(node, "sh", "-c",
		fmt.Sprintf("printf '%s\\n' > /etc/default/kubelet", desired)); err != nil {
		return err
	}
	_, err = m.rt.Exec(node, "systemctl", "restart", "kubelet")
	return err
}

// pinnedFamily reads the --node-ip kiac pinned in /etc/default/kubelet
// and reports the family it encodes: a comma means dual-stack (v4,v6), a
// lone colon means ipv6-only. An IPv4-only cluster pins nothing, so the
// second return is false and the caller leaves the node alone.
func pinnedFamily(kubeletDefault string) (IPFamily, bool) {
	const key = "--node-ip="
	idx := strings.Index(kubeletDefault, key)
	if idx < 0 {
		return "", false
	}
	val := kubeletDefault[idx+len(key):]
	if i := strings.IndexAny(val, " \t\r\n"); i >= 0 {
		val = val[:i]
	}
	switch {
	case strings.Contains(val, ","):
		return DualStack, true
	case strings.Contains(val, ":"):
		return IPv6, true
	default:
		return "", false
	}
}

// healWorkerScript points a worker's kubelet at the control plane's
// current IP. It matches any previous address rather than one known old
// IP, so a resume that died between healing the control plane and the
// workers stays repairable. The kubelet only re-reads kubelet.conf on
// restart, hence the restart when the server line changed.
func healWorkerScript(cpIP string) string {
	return fmt.Sprintf(`if ! grep -q 'server: https://%[1]s:6443' /etc/kubernetes/kubelet.conf; then
  sed -i -E 's#(server: https://)[0-9.]+(:6443)#\1%[1]s\2#' /etc/kubernetes/kubelet.conf
  systemctl restart kubelet
fi`, cpIP)
}

// healConfigMapsScript re-points the kubeconfigs kubeadm stored in the
// cluster itself: kube-proxy mounts its ConfigMap kubeconfig (its pods
// crash-loop against the old IP until it changes and the daemonset
// restarts), and kube-public's cluster-info is what a future `kubeadm
// join` discovery reads. Both grep guards make the script a no-op when
// the address is already current.
func healConfigMapsScript(cpIP string) string {
	k := "kubectl --kubeconfig " + adminConf
	return fmt.Sprintf(`if ! %[1]s -n kube-system get configmap kube-proxy -o yaml | grep -q 'https://%[2]s:6443'; then
  %[1]s -n kube-system get configmap kube-proxy -o yaml | sed -E 's#https://[0-9.]+:6443#https://%[2]s:6443#g' | %[1]s apply -f -
  %[1]s -n kube-system rollout restart daemonset kube-proxy
fi
if ! %[1]s -n kube-public get configmap cluster-info -o yaml | grep -q 'https://%[2]s:6443'; then
  %[1]s -n kube-public get configmap cluster-info -o yaml | sed -E 's#https://[0-9.]+:6443#https://%[2]s:6443#g' | %[1]s apply -f -
fi`, k, cpIP)
}
