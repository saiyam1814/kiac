package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// ExternalWorkerManager owns local agent VMs, never the remote cluster.
// Its VM namespace cannot match the kiac- prefix used by cluster operations.
type ExternalWorkerManager struct {
	manager   *Manager
	backend   runtime.NodeBackend
	root      string
	preflight func() error
	image     func() (string, error)
	artifacts func(string) (k3sGPUArtifacts, error)
	owned     func(ExternalWorkerState) error
}

func NewExternalWorkerManager() *ExternalWorkerManager {
	manager := NewManager()
	m := &ExternalWorkerManager{manager: manager, backend: manager.krunkit,
		preflight: manager.krunkit.Preflight,
		image:     func() (string, error) { return ResolveGPUImage(DefaultGPUImage) }, artifacts: ensureK3sGPUArtifacts}
	m.owned = func(s ExternalWorkerState) error {
		state, err := manager.krunkit.State(s.VMName)
		if err != nil {
			return err
		}
		if state.Name != s.VMName || state.NetworkID != s.NetworkID || state.Distro != "external-k3s" || state.K8sVersion != s.K8sVersion || state.GPU {
			return fmt.Errorf("runtime VM is not owned by this external worker")
		}
		return nil
	}
	return m
}

func workerFailure(stage string) error {
	// Never wrap raw runtime errors from credential transfers or agent startup:
	// a remote command can echo stdin, including tokens or private keys.
	return fmt.Errorf("external worker %s failed (private command output withheld)", stage)
}

func (m *ExternalWorkerManager) Join(cfg ExternalWorkerConfig) (result ExternalWorkerState, retErr error) {
	p, err := prepareExternalWorker(cfg)
	if err != nil {
		return result, err
	}
	unlock, err := m.lock()
	if err != nil {
		return result, err
	}
	defer unlock()
	path, _ := m.statePath(cfg.Name)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("external worker %q already exists or its inventory cannot be verified", cfg.Name)
	}
	if err := m.preflight(); err != nil {
		return result, err
	}
	image, err := m.image()
	if err != nil {
		return result, err
	}
	artifacts, err := m.artifacts(p.config.K8sVersion)
	if err != nil {
		return result, err
	}
	state, err := newExternalWorkerState(p)
	if err != nil {
		return result, err
	}
	if err := m.write(state); err != nil {
		return result, err
	}
	created := false
	defer func() {
		if retErr == nil {
			return
		}
		// Agent startup may already have registered a node-password remotely.
		// Preserve that identity so start can recover it without a second join.
		if state.Phase == "joining" || state.Phase == "ready" {
			retErr = errors.Join(retErr, fmt.Errorf("local worker retained; use kiac start worker %s to retry, or drain/remove its remote Node before kiac delete worker", state.Name))
			return
		}
		if created {
			if err := m.owned(state); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("local VM retained because ownership cannot be verified"))
				return
			}
			if err := m.backend.Remove(state.VMName); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("owned VM cleanup failed; inventory retained for delete worker"))
				return
			}
		}
		_ = os.Remove(path)
	}()
	if err := m.backend.RunDetached(runtime.RunOpts{Backend: runtime.BackendKrunkit, Name: state.VMName, Image: image,
		Distro: "external-k3s", K8sVersion: state.K8sVersion, CPUs: p.config.CPUs, Memory: p.config.Memory,
		DiskSize: p.config.DiskSize, NetworkID: state.NetworkID}); err != nil {
		return result, workerFailure("VM creation")
	}
	created = true
	if err := m.backend.WaitReady(state.VMName, p.config.WaitTimeout); err != nil {
		return result, workerFailure("SSH readiness")
	}
	managementIP, err := m.backend.IP(state.VMName)
	if err != nil {
		return result, workerFailure("management address discovery")
	}
	// This is an additional guard on top of the runtime's subnet-scoped report.
	// Never replace management routes or send the tunnel endpoint into itself.
	management, err := netip.ParseAddr(managementIP)
	if err != nil || !management.Is4() || managementIP == state.NodeIP {
		return result, fmt.Errorf("management address must be distinct from the advertised WireGuard address")
	}
	managementSubnet := netip.PrefixFrom(management, 24).Masked()
	nodeIP, _ := netip.ParseAddr(state.NodeIP)
	if managementSubnet.Contains(nodeIP) {
		return result, fmt.Errorf("advertised WireGuard address overlaps the VM management subnet")
	}
	for _, route := range p.wg.AllowedIPs {
		if route.Overlaps(managementSubnet) {
			return result, fmt.Errorf("WireGuard routes overlap the VM management subnet")
		}
	}
	if err := m.manager.provisionK3sGPUNode(state.VMName, artifacts, false, p.config.WaitTimeout); err != nil {
		return result, workerFailure("K3s provisioning")
	}
	if _, err := m.backend.ExecTimeout(state.VMName, max(p.config.WaitTimeout, 10*time.Minute)+15*time.Second,
		"timeout", "--signal=TERM", "--kill-after=10s", fmt.Sprintf("%ds", int64(max(p.config.WaitTimeout, 10*time.Minute)/time.Second)),
		"sh", "-euc", "dnf -y --setopt=ip_resolve=4 install wireguard-tools; command -v wg; command -v python3; modprobe wireguard; modprobe vxlan"); err != nil {
		return result, workerFailure("WireGuard and kernel preflight")
	}
	if err := m.installExternalWorker(&state, p); err != nil {
		return result, err
	}
	observation, err := m.wait(state, p.config.WaitTimeout)
	if err != nil {
		return result, err
	}
	state.NodeUID, state.Phase = observation.UID, "ready"
	if err := m.write(state); err != nil {
		return result, err
	}
	return state, nil
}

type workerObservation struct {
	Name, UID, OwnerID, NodeIP, KubeletVersion, APIVersion, PodNetwork, Backend string
	GuestBootID, NodeBootID                                                     string
	Ready, Tainted, IPv4Only                                                    bool
}

type workerObservationRejected struct{ reason string }

func (e *workerObservationRejected) Error() string { return e.reason }

func (m *ExternalWorkerManager) observe(state ExternalWorkerState, timeout time.Duration) (workerObservation, error) {
	var observation workerObservation
	if timeout <= 0 {
		return observation, fmt.Errorf("worker observation deadline expired")
	}
	raw, err := m.backend.ExecTimeout(state.VMName, min(timeout, 25*time.Second), "python3", "-c", externalWorkerObserveScript(state))
	if err != nil {
		return observation, workerFailure("authenticated node observation")
	}
	if len(raw) > 16*1024 || json.Unmarshal([]byte(raw), &observation) != nil {
		return observation, fmt.Errorf("invalid external worker readiness report")
	}
	if observation.Name != state.VMName || observation.UID == "" || observation.OwnerID != state.OwnerID || observation.NodeIP != state.NodeIP ||
		(state.NodeUID != "" && state.NodeUID != observation.UID) {
		return observation, &workerObservationRejected{"external Node identity or advertised IP does not match this worker"}
	}
	if normalizeK3sStateVersion(observation.KubeletVersion) != state.K8sVersion || normalizeK3sStateVersion(observation.APIVersion) != state.K8sVersion {
		return observation, &workerObservationRejected{"external API and worker must match the requested pinned K3s release"}
	}
	if observation.Backend != "vxlan" || observation.PodNetwork != state.PodCIDR || !observation.IPv4Only {
		return observation, &workerObservationRejected{"external worker alpha requires the existing Flannel VXLAN CNI and declared pod CIDR"}
	}
	if state.Phase == "joining" && !observation.Tainted {
		return observation, &workerObservationRejected{"new external worker must have kiac.dev/external-worker=true:NoSchedule taint"}
	}
	// Kubernetes may retain Ready=True from before a reboot. Only the
	// kubelet's report for this guest boot can satisfy current readiness.
	observation.Ready = observation.Ready && observation.GuestBootID != "" && observation.GuestBootID == observation.NodeBootID
	return observation, nil
}

func (m *ExternalWorkerManager) wait(state ExternalWorkerState, timeout time.Duration) (workerObservation, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		observation, err := m.observe(state, time.Until(deadline))
		if err == nil && observation.Ready {
			return observation, nil
		}
		var rejected *workerObservationRejected
		if errors.As(err, &rejected) {
			return observation, err
		}
		last = err
		time.Sleep(min(time.Second, max(time.Until(deadline), 0)))
	}
	if last != nil {
		return workerObservation{}, fmt.Errorf("external worker did not become Ready: %w", last)
	}
	return workerObservation{}, fmt.Errorf("external worker did not become Ready before --wait expired")
}

func (m *ExternalWorkerManager) Workers() ([]ExternalWorkerInfo, error) {
	states, err := m.states()
	if err != nil {
		return nil, err
	}
	rows, err := m.backend.List("kiacx-worker-")
	if err != nil {
		return nil, err
	}
	var infos []ExternalWorkerInfo
	for _, state := range states {
		info := ExternalWorkerInfo{ExternalWorkerState: state, VMStatus: "missing"}
		for _, row := range rows {
			if row.Name != state.VMName {
				continue
			}
			if err := m.owned(state); err != nil {
				return nil, err
			}
			info.VMStatus, info.ManagementIP = row.Status, row.IP
			if strings.EqualFold(row.Status, "running") && state.Phase == "ready" {
				if observation, err := m.observe(state, 25*time.Second); err == nil {
					info.Ready, info.ObservedAt = observation.Ready, time.Now().UTC()
				}
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (m *ExternalWorkerManager) Start(name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if timeout > time.Hour {
		return fmt.Errorf("worker --wait must not exceed one hour")
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := m.read(name)
	if err != nil {
		return err
	}
	if state.Phase != "ready" && state.Phase != "joining" {
		return fmt.Errorf("worker creation is incomplete; delete the owned local worker and retry join")
	}
	if err := m.owned(state); err != nil {
		return err
	}
	if err := m.backend.Start(state.VMName); err != nil {
		return workerFailure("VM start")
	}
	if err := m.backend.WaitReady(state.VMName, timeout); err != nil {
		return workerFailure("SSH readiness")
	}
	if state.Phase == "joining" {
		if err := m.startExternalWorkerAgent(state); err != nil {
			return err
		}
	}
	observation, err := m.wait(state, timeout)
	if err != nil {
		return err
	}
	state.NodeUID, state.Phase = observation.UID, "ready"
	return m.write(state)
}

func (m *ExternalWorkerManager) Stop(name string) error {
	return m.localAction(name, false)
}

// Delete removes local resources only. Draining and deleting the external Node
// requires the operator's external-cluster credentials and is intentionally not attempted.
func (m *ExternalWorkerManager) Delete(name string) error {
	return m.localAction(name, true)
}

func (m *ExternalWorkerManager) localAction(name string, remove bool) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := m.read(name)
	if err != nil {
		return err
	}
	if err := m.owned(state); err != nil {
		if !remove || !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if remove {
		if err := m.backend.Remove(state.VMName); err != nil {
			return workerFailure("owned local VM deletion")
		}
	} else if err := m.backend.Stop(state.VMName); err != nil {
		return workerFailure("VM stop")
	}
	if remove {
		path, _ := m.statePath(name)
		return os.Remove(path)
	}
	return nil
}

func (m *ExternalWorkerManager) Logs(name string) (string, error) {
	state, err := m.read(name)
	if err != nil {
		return "", err
	}
	if err := m.owned(state); err != nil {
		return "", err
	}
	logs, err := m.backend.ExecTimeout(state.VMName, 10*time.Second, "python3", "-c", externalWorkerLogsScript)
	if err != nil {
		return "", workerFailure("log retrieval")
	}
	return logs, nil
}
