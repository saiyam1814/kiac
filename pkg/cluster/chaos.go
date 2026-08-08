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
// It is safe to re-run on an already-running node: the boot is simply
// skipped. vmnet may hand the VM a different IP on restart; kubeadm
// re-registers it and kiac-lb (see lb.go) re-points stale ingress IPs.
// k3s also needs cluster-level endpoint and hostNetwork Pod healing, so
// its single-node start delegates to Resume; several stopped nodes must
// be resumed together explicitly.
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
	if !running && distroFromNodes(infos) == "k3s" {
		stopped := 0
		for _, info := range infos {
			if !strings.EqualFold(info.Status, "running") {
				stopped++
			}
		}
		if stopped > 1 {
			return fmt.Errorf("k3s cluster %q has %d stopped nodes; recover the topology together with: kiac resume cluster --name %s", name, stopped, name)
		}
		// A k3s node's new vmnet address can stale agent endpoints and
		// hostNetwork Pod metadata. The resume path starts the one stopped
		// VM and reconciles those cluster-level references before returning.
		return m.Resume(name, 5*time.Minute)
	}
	if running {
		ui.Infof("node %s is already running", target)
	} else if err := ui.Step(fmt.Sprintf("Starting node %s", target), func() error {
		return m.rt.Start(target)
	}); err != nil {
		return err
	}
	ui.Successf("Node %s started.", target)
	ui.Hintf("watch it rejoin: kubectl get nodes -w")
	ui.Hintf("LoadBalancer IPs re-point automatically if the node came back with a new address")
	return nil
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
