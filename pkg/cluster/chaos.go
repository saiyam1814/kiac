package cluster

import (
	"fmt"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
	"github.com/saiyam1814/kiac/pkg/ui"
)

// StopNode halts one node's VM so Kubernetes reacts to a real node
// failure: NotReady detection, eviction, rescheduling. The container
// and its state remain; StartNode boots it again.
func (m *Manager) StopNode(name, node string) error {
	infos, target, err := m.findNode(name, node)
	if err != nil {
		return err
	}
	if target == ControlPlane(name) {
		// A single-node cluster has no surviving node to observe the
		// failure, so stopping its control plane only strands the user.
		if len(infos) == 1 {
			return fmt.Errorf("refusing to stop %s: it is the only node of cluster %q; add workers to test node failure, or delete the cluster", target, name)
		}
		ui.Infof("stopping the control plane: the API server (and kubectl) is down until: kiac start node control-plane --name %s", name)
	}
	if err := ui.Step(fmt.Sprintf("Stopping node %s", target), func() error {
		return m.rt.Stop(target)
	}); err != nil {
		return err
	}
	ui.Successf("Node %s stopped.", target)
	ui.Hintf("watch: kubectl get nodes -w")
	ui.Hintf("kiac start node %s --name %s", strings.TrimPrefix(target, prefix(name)), name)
	return nil
}

// StartNode boots a previously stopped node VM back into the cluster.
// It is safe to re-run on an already-running node: the boot is skipped
// and only the MetalLB pool re-sync happens, which is the natural retry
// when the first sync raced MetalLB's own recovery from the outage.
func (m *Manager) StartNode(name, node string) error {
	infos, target, err := m.findNode(name, node)
	if err != nil {
		return err
	}
	running := false
	for _, i := range infos {
		if i.Name == target && strings.EqualFold(i.Status, "running") {
			running = true
		}
	}
	if running {
		ui.Infof("node %s is already running; re-syncing its MetalLB pool", target)
	} else if err := ui.Step(fmt.Sprintf("Starting node %s", target), func() error {
		return m.rt.Start(target)
	}); err != nil {
		return err
	}
	// vmnet may hand the VM a different IP on restart. The kubelet
	// re-registers itself with the new address, but the MetalLB
	// per-node pool pins the old one, leaving a stale VIP nothing
	// routes to. Failure only degrades LoadBalancer coverage.
	poolName := "kiac-ip-" + target
	if target == worker(name, 1) || (target == ControlPlane(name) && len(infos) == 1) {
		poolName = "kiac-primary"
	}
	if err := m.resyncNodePool(name, target, poolName); err != nil {
		ui.Infof("MetalLB pool not re-synced for %s: %v", target, err)
		ui.Hintf("safe to retry once the cluster settles: kiac start node %s --name %s", strings.TrimPrefix(target, prefix(name)), name)
	}
	ui.Successf("Node %s started.", target)
	ui.Hintf("watch it rejoin: kubectl get nodes -w")
	return nil
}

// resyncNodePool rewrites the restarted node's IPAddressPool with its
// current IP. A cluster without MetalLB, or a node outside the pool
// (the control plane when workers exist), is detected by the pool
// object's absence and skipped silently.
func (m *Manager) resyncNodePool(name, target, poolName string) error {
	cp := ControlPlane(name)
	if err := m.rt.WaitReady(target, 2*time.Minute); err != nil {
		return err
	}
	ip, err := m.rt.IP(target)
	if err != nil {
		return err
	}
	out, err := m.rt.Exec(cp, "kubectl", "--kubeconfig", adminConf,
		"get", "ipaddresspool", poolName, "-n", "metallb-system", "-o", "name")
	if err != nil {
		if strings.Contains(out, "NotFound") || strings.Contains(out, "doesn't have a resource type") {
			return nil
		}
		return err
	}
	manifest := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: %[1]s
  namespace: metallb-system
spec:
  addresses: ["%[2]s/32"]
`, poolName, ip)
	// The MetalLB webhook itself may be recovering from the very outage
	// this command is healing (its pod can live on the restarted node),
	// so the apply retries while the cluster settles.
	var lastErr error
	for attempt := 0; attempt < 24; attempt++ {
		lastErr = m.rt.ExecStdin(cp, strings.NewReader(manifest),
			"kubectl", "--kubeconfig", adminConf, "apply", "-f", "-")
		if lastErr == nil {
			ui.Infof("MetalLB pool for %s now %s/32", target, ip)
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return lastErr
}

// findNode lists the cluster's node containers and resolves node
// against them, so stop/start never touch containers outside the
// named cluster.
func (m *Manager) findNode(name, node string) ([]runtime.Info, string, error) {
	if !ValidName(name) {
		return nil, "", fmt.Errorf("invalid cluster name %q", name)
	}
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return nil, "", err
	}
	if len(infos) == 0 {
		return nil, "", fmt.Errorf("no cluster named %q found", name)
	}
	target, err := resolveNode(infos, name, node)
	if err != nil {
		return nil, "", err
	}
	return infos, target, nil
}

// resolveNode accepts the short node name ("worker-1", "control-plane")
// or the full container name ("kiac-dev-worker-1") and returns the
// container name, listing the valid choices when nothing matches.
func resolveNode(infos []runtime.Info, name, node string) (string, error) {
	full := node
	if !strings.HasPrefix(node, prefix(name)) {
		full = prefix(name) + node
	}
	var available []string
	for _, i := range infos {
		if i.Name == full {
			return full, nil
		}
		available = append(available, strings.TrimPrefix(i.Name, prefix(name)))
	}
	return "", fmt.Errorf("no node %q in cluster %q (nodes: %s)", node, name, strings.Join(available, ", "))
}
