package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ExternalWorkerState contains public inventory only. Join credentials and
// WireGuard private keys are never written to this inventory.
type ExternalWorkerState struct {
	Schema      int       `json:"schema"`
	Name        string    `json:"name"`
	OwnerID     string    `json:"ownerID"`
	VMName      string    `json:"vmName"`
	NetworkID   string    `json:"networkID"`
	Server      string    `json:"server"`
	NodeIP      string    `json:"nodeIP"`
	K8sVersion  string    `json:"k8sVersion"`
	PodCIDR     string    `json:"podCIDR"`
	ServiceCIDR string    `json:"serviceCIDR"`
	CAHash      string    `json:"caHash"`
	NodeUID     string    `json:"nodeUID,omitempty"`
	Phase       string    `json:"phase"`
	Created     time.Time `json:"created"`
}

type ExternalWorkerInfo struct {
	ExternalWorkerState
	VMStatus     string    `json:"vmStatus"`
	ManagementIP string    `json:"managementIP,omitempty"`
	Ready        bool      `json:"ready"`
	ObservedAt   time.Time `json:"observedAt,omitempty"`
}

func externalVMName(name, id string) string { return "kiacx-worker-" + name + "-" + id[:12] }
func externalNetworkID(id string) string    { return "ext-" + id }

func (m *ExternalWorkerManager) stateRoot() (string, error) {
	if m.root != "" {
		return m.root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kiac", "workers"), nil
}

func (m *ExternalWorkerManager) statePath(name string) (string, error) {
	if !externalWorkerNameRE.MatchString(name) {
		return "", fmt.Errorf("invalid external worker name")
	}
	root, err := m.stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name+".json"), nil
}

func (m *ExternalWorkerManager) lock() (func(), error) {
	root, err := m.stateRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }, nil
}

func (m *ExternalWorkerManager) read(name string) (ExternalWorkerState, error) {
	var state ExternalWorkerState
	path, err := m.statePath(name)
	if err != nil {
		return state, err
	}
	raw, err := readWorkerSecret(path, 64*1024)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("invalid worker inventory")
	}
	id, err := hex.DecodeString(state.OwnerID)
	if state.Schema != 1 || state.Name != name || err != nil || len(id) != 16 || state.OwnerID != strings.ToLower(state.OwnerID) ||
		state.VMName != externalVMName(name, state.OwnerID) || state.NetworkID != externalNetworkID(state.OwnerID) ||
		(state.Phase != "creating" && state.Phase != "joining" && state.Phase != "ready") || (state.Phase == "ready" && state.NodeUID == "") {
		return state, fmt.Errorf("worker inventory ownership or schema does not match")
	}
	server, urlErr := url.Parse(state.Server)
	node, ipErr := netip.ParseAddr(state.NodeIP)
	pod, podErr := workerPrefix(state.PodCIDR)
	service, serviceErr := workerPrefix(state.ServiceCIDR)
	hash, hashErr := hex.DecodeString(state.CAHash)
	if urlErr != nil || server.Scheme != "https" || server.User != nil || server.Host == "" || server.RawQuery != "" || server.Fragment != "" || server.Path != "" ||
		ipErr != nil || !node.Is4() || !node.IsGlobalUnicast() || podErr != nil || serviceErr != nil || pod.Overlaps(service) || pod.Contains(node) || service.Contains(node) ||
		hashErr != nil || len(hash) != 32 || state.CAHash != strings.ToLower(state.CAHash) || k3sARM64Assets[state.K8sVersion].Release == "" || state.Created.IsZero() || len(state.NodeUID) > 128 {
		return state, fmt.Errorf("invalid worker inventory metadata")
	}
	return state, nil
}

func (m *ExternalWorkerManager) write(state ExternalWorkerState) error {
	path, err := m.statePath(state.Name)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".worker-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func newExternalWorkerState(p preparedExternalWorker) (ExternalWorkerState, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return ExternalWorkerState{}, err
	}
	owner := hex.EncodeToString(id)
	return ExternalWorkerState{Schema: 1, Name: p.config.Name, OwnerID: owner,
		VMName: externalVMName(p.config.Name, owner), NetworkID: externalNetworkID(owner),
		Server: p.config.Server, NodeIP: p.config.NodeIP, K8sVersion: p.config.K8sVersion,
		PodCIDR: p.config.PodCIDR, ServiceCIDR: p.config.ServiceCIDR, CAHash: p.caHash,
		Phase: "creating", Created: time.Now().UTC()}, nil
}

func (m *ExternalWorkerManager) states() ([]ExternalWorkerState, error) {
	root, err := m.stateRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var states []ExternalWorkerState
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		state, err := m.read(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}
