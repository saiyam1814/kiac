package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const krunkitStateSchema = 1

// KrunkitNodeState is the durable inventory for a krunkit VM. It is public so
// cluster diagnostics can report the effective GPU memory window without
// scraping a process command line.
type KrunkitNodeState struct {
	Schema             int       `json:"schema"`
	Name               string    `json:"name"`
	Image              string    `json:"image"`
	Distro             string    `json:"distro"`
	K8sVersion         string    `json:"k8sVersion"`
	GPU                bool      `json:"gpu"`
	Disk               string    `json:"disk"`
	Seed               string    `json:"seed"`
	RestSocket         string    `json:"restSocket"`
	MAC                string    `json:"mac"`
	IP                 string    `json:"ip,omitempty"`
	CPUs               int       `json:"cpus"`
	MemoryMiB          int       `json:"memoryMiB"`
	GPUMemoryMiB       int       `json:"gpuMemoryMiB"`
	NetworkID          string    `json:"networkID"`
	NetworkCIDR        string    `json:"networkCIDR"`
	SSHUser            string    `json:"sshUser"`
	PID                int       `json:"pid,omitempty"`
	ProcessStart       string    `json:"processStart,omitempty"`
	ProcessCommandHash string    `json:"processCommandHash,omitempty"`
	Status             string    `json:"status"`
	Created            time.Time `json:"created"`
	Mounts             []Mount   `json:"mounts,omitempty"`
}

// lockStore serializes lifecycle and read-modify-write operations across both
// goroutines and separate kiac processes. Atomic renames protect file contents;
// this lock protects the decisions made from those contents.
func (c *KrunkitClient) lockStore() (func(), error) {
	c.mu.Lock()
	root, err := c.rootDir()
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		c.mu.Unlock()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		c.mu.Unlock()
	}, nil
}

type krunkitNetworkState struct {
	Schema  int    `json:"schema"`
	ID      string `json:"id"`
	CIDR    string `json:"cidr"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Netmask string `json:"netmask"`
}

func (c *KrunkitClient) rootDir() (string, error) {
	if c.RootDir != "" {
		return c.RootDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving krunkit state directory: %w", err)
	}
	return filepath.Join(home, ".kiac", "gpu-nodes"), nil
}

func (c *KrunkitClient) nodeDir(name string) (string, error) {
	if !validVMName(name) {
		return "", fmt.Errorf("invalid krunkit VM name %q", name)
	}
	root, err := c.rootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "nodes", name), nil
}

func (c *KrunkitClient) shortSocketPath(name string) (string, error) {
	if !validVMName(name) {
		return "", fmt.Errorf("invalid krunkit VM name %q", name)
	}
	root, err := c.rootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("kiac-krunkit-%08x.sock", fnv32(root+"\x00"+name))), nil
}

func (c *KrunkitClient) statePath(name string) (string, error) {
	dir, err := c.nodeDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

func (c *KrunkitClient) readState(name string) (KrunkitNodeState, error) {
	path, err := c.statePath(name)
	if err != nil {
		return KrunkitNodeState{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return KrunkitNodeState{}, err
	}
	var state KrunkitNodeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return KrunkitNodeState{}, fmt.Errorf("parsing krunkit state for %s: %w", name, err)
	}
	if state.Schema != krunkitStateSchema || state.Name != name {
		return KrunkitNodeState{}, fmt.Errorf("invalid krunkit state for %s", name)
	}
	return state, nil
}

func (c *KrunkitClient) writeState(state KrunkitNodeState) error {
	path, err := c.statePath(state.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSONAtomic(path, state, 0o600)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kiac-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (c *KrunkitClient) ensureNetwork(id string) (krunkitNetworkState, error) {
	if !validKrunkitNetworkID(id) {
		return krunkitNetworkState{}, fmt.Errorf("invalid krunkit network ID %q: use lowercase letters, digits, and dashes", id)
	}
	root, err := c.rootDir()
	if err != nil {
		return krunkitNetworkState{}, err
	}
	dir := filepath.Join(root, "networks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return krunkitNetworkState{}, err
	}
	path := filepath.Join(dir, id+".json")
	if raw, err := os.ReadFile(path); err == nil {
		var network krunkitNetworkState
		if err := json.Unmarshal(raw, &network); err != nil {
			return krunkitNetworkState{}, fmt.Errorf("parsing krunkit network %s: %w", id, err)
		}
		if err := validateKrunkitNetwork(network, id); err != nil {
			return krunkitNetworkState{}, err
		}
		return network, nil
	}

	used := make(map[int]bool)
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && ip[12] == 192 && ip[13] == 168 {
				used[int(ip[14])] = true
			}
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var network krunkitNetworkState
		if json.Unmarshal(raw, &network) == nil {
			if ip := net.ParseIP(strings.Split(network.CIDR, "/")[0]).To4(); ip != nil {
				used[int(ip[2])] = true
			}
		}
	}

	start := 105 + int(fnv32(id)%145)
	chosen := 0
	for offset := 0; offset < 145; offset++ {
		candidate := 105 + (start-105+offset)%145
		if !used[candidate] {
			chosen = candidate
			break
		}
	}
	if chosen == 0 {
		return krunkitNetworkState{}, fmt.Errorf("no free 192.168.x.0/24 subnet is available for krunkit")
	}
	network := krunkitNetworkState{
		Schema:  krunkitStateSchema,
		ID:      id,
		CIDR:    fmt.Sprintf("192.168.%d.0/24", chosen),
		Start:   fmt.Sprintf("192.168.%d.1", chosen),
		End:     fmt.Sprintf("192.168.%d.200", chosen),
		Netmask: "255.255.255.0",
	}
	if err := writeJSONAtomic(path, network, 0o600); err != nil {
		return krunkitNetworkState{}, err
	}
	return network, nil
}

func validateKrunkitNetwork(network krunkitNetworkState, id string) error {
	if network.Schema != krunkitStateSchema || network.ID != id {
		return fmt.Errorf("invalid krunkit network state for %s", id)
	}
	ip, subnet, err := net.ParseCIDR(network.CIDR)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("invalid krunkit network CIDR %q for %s", network.CIDR, id)
	}
	ones, bits := subnet.Mask.Size()
	base := ip.To4()
	if bits != 32 || ones != 24 || base[0] != 192 || base[1] != 168 || base[3] != 0 ||
		network.Netmask != "255.255.255.0" {
		return fmt.Errorf("invalid krunkit network %q for %s: expected a 192.168.x.0/24 subnet", network.CIDR, id)
	}
	wantStart := fmt.Sprintf("192.168.%d.1", base[2])
	wantEnd := fmt.Sprintf("192.168.%d.200", base[2])
	if network.Start != wantStart || network.End != wantEnd {
		return fmt.Errorf("invalid krunkit address range %q-%q for %s", network.Start, network.End, id)
	}
	return nil
}

func validKrunkitNetworkID(id string) bool {
	if len(id) == 0 || len(id) > 63 {
		return false
	}
	for _, r := range id {
		if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func fnv32(s string) uint32 {
	const prime = uint32(16777619)
	hash := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime
	}
	return hash
}

func randomMAC() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[0] = (b[0] | 2) & 0xfe
	return net.HardwareAddr(b).String(), nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func parseSizeBytes(value string, defaultBytes int64) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(value))
	if s == "" {
		return defaultBytes, nil
	}
	multiplier := int64(1)
	for suffix, size := range map[string]int64{
		"KIB": 1 << 10, "KB": 1 << 10, "K": 1 << 10,
		"MIB": 1 << 20, "MB": 1 << 20, "M": 1 << 20,
		"GIB": 1 << 30, "GB": 1 << 30, "G": 1 << 30,
		"TIB": 1 << 40, "TB": 1 << 40, "T": 1 << 40,
	} {
		if strings.HasSuffix(s, suffix) {
			multiplier = size
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix))
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n * multiplier, nil
}

func parseCPUCount(value string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid CPU count %q", value)
	}
	return n, nil
}

func effectiveGPUMemoryMiB(memoryMiB, hostMemoryMiB int) int {
	// krunkit 1.3.x passes this exact value to krun_set_gpu_options2: a
	// 62-GiB IPA window minus RAM rounded to the next GiB, capped by host
	// memory. It is a VM-level hard shared-memory window, not a per-process
	// compute quota. Keep this formula aligned with krunkit/src/context.rs.
	roundedMemoryMiB := (memoryMiB/1024 + 1) * 1024
	windowMiB := 62*1024 - roundedMemoryMiB
	if hostMemoryMiB > 0 && windowMiB > hostMemoryMiB {
		windowMiB = hostMemoryMiB
	}
	if windowMiB < 0 {
		return 0
	}
	return windowMiB
}
