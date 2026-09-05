package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	stdruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	minKrunkitVersion  = "1.3.2"
	minVMNetVersion    = "0.13.0"
	defaultDiskBytes   = int64(20 << 30)
	defaultSSHUser     = "fedora"
	krunkitInstallHint = "brew tap libkrun/krun && brew trust libkrun/krun && brew install krunkit"
	vmnetInstallHint   = "on macOS 26+: brew tap nirs/vmnet-helper && brew trust nirs/vmnet-helper && brew install vmnet-helper; on macOS 14-15: use https://github.com/nirs/vmnet-helper#installation"
)

// KrunkitClient manages persistent Linux VMs backed by krunkit and
// vmnet-helper. Unlike apple/container, neither tool has an inventory or exec
// API, so this client owns both responsibilities through durable state and SSH.
type KrunkitClient struct {
	Bin           string
	VMNetRunBin   string
	SSHBin        string
	SSHKeygenBin  string
	HDIUtilBin    string
	ARPBin        string
	BrewBin       string
	SysctlBin     string
	PSBin         string
	RootDir       string
	HostMemoryMiB int // test override; zero reads hw.memsize from the host
	StopTimeout   time.Duration
	KillTimeout   time.Duration

	mu sync.Mutex
}

var _ NodeBackend = (*KrunkitClient)(nil)

func NewKrunkit() *KrunkitClient {
	return &KrunkitClient{
		Bin:          "krunkit",
		VMNetRunBin:  "vmnet-run",
		SSHBin:       "ssh",
		SSHKeygenBin: "ssh-keygen",
		HDIUtilBin:   "hdiutil",
		ARPBin:       "arp",
		BrewBin:      "brew",
		SysctlBin:    "sysctl",
		PSBin:        "ps",
	}
}

// Owns reports whether a node has durable krunkit state. It deliberately does
// not depend on the process being alive: stopped and crashed VMs must continue
// routing to this backend for start, delete, status, and support operations.
func (c *KrunkitClient) Owns(name string) bool {
	path, err := c.statePath(name)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// State returns a node's durable backend metadata for diagnostics and GPU
// resource accounting.
func (c *KrunkitClient) State(name string) (KrunkitNodeState, error) {
	unlock, err := c.lockStore()
	if err != nil {
		return KrunkitNodeState{}, err
	}
	defer unlock()
	return c.readState(name)
}

// Preflight rejects dependency combinations known to boot a VM with a broken
// Venus device. The official krunkit formula currently uses Homebrew core's
// Venus-enabled virglrenderer; older supported installs used a tapped formula.
func (c *KrunkitClient) Preflight() error {
	if stdruntime.GOOS != "darwin" || stdruntime.GOARCH != "arm64" {
		return fmt.Errorf("real Apple GPU nodes require macOS on Apple silicon")
	}
	krunkit, err := c.resolveBinary(c.Bin)
	if err != nil {
		return fmt.Errorf("krunkit is not installed; install it with: %s", krunkitInstallHint)
	}
	if err := requireToolVersion(krunkit, minKrunkitVersion); err != nil {
		return fmt.Errorf("checking krunkit: %w", err)
	}
	vmnetRun, err := c.resolveVMNetRun()
	if err != nil {
		return fmt.Errorf("vmnet-helper is not installed; install it with: %s", vmnetInstallHint)
	}
	if err := requireToolVersion(vmnetRun, minVMNetVersion); err != nil {
		return fmt.Errorf("checking vmnet-helper: %w", err)
	}
	for _, spec := range []struct {
		value      string
		candidates []string
	}{
		{c.SSHBin, []string{"/usr/bin/ssh"}},
		{c.SSHKeygenBin, []string{"/usr/bin/ssh-keygen"}},
		{c.HDIUtilBin, []string{"/usr/bin/hdiutil"}},
		{c.ARPBin, []string{"/usr/sbin/arp"}},
		{c.SysctlBin, []string{"/usr/sbin/sysctl"}},
		{c.PSBin, []string{"/bin/ps"}},
	} {
		if _, err := c.resolveBinary(spec.value, spec.candidates...); err != nil {
			return err
		}
	}
	return c.validateVirglrenderer()
}

func (c *KrunkitClient) validateVirglrenderer() error {
	brew, err := c.resolveBinary(c.BrewBin, "/opt/homebrew/bin/brew")
	if err != nil {
		return fmt.Errorf("Homebrew is required to verify krunkit's GPU renderer; install krunkit with: %s", krunkitInstallHint)
	}
	out, err := exec.Command(brew, "info", "--json=v2", "virglrenderer").CombinedOutput()
	if err != nil {
		return fmt.Errorf("checking virglrenderer: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	var info struct {
		Formulae []struct {
			FullName  string `json:"full_name"`
			Installed []any  `json:"installed"`
		} `json:"formulae"`
	}
	if err := json.Unmarshal(out, &info); err != nil || len(info.Formulae) == 0 {
		return fmt.Errorf("checking virglrenderer formula: unexpected brew output")
	}
	formula := info.Formulae[0]
	if !supportedVirglFormula(formula.FullName) || len(formula.Installed) == 0 {
		return fmt.Errorf("the installed virglrenderer cannot create a krunkit Venus GPU; install the official stack with: %s", krunkitInstallHint)
	}
	return nil
}

func supportedVirglFormula(name string) bool {
	switch name {
	case "virglrenderer", "homebrew/core/virglrenderer", "libkrun/krun/virglrenderer", "slp/krun/virglrenderer":
		return true
	default:
		return false
	}
}

func requireToolVersion(bin, minimum string) error {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return &CommandError{Tool: bin, Args: []string{"--version"}, Output: string(out), Err: err}
	}
	version := regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(string(out))
	if version == "" {
		return fmt.Errorf("could not parse version from %q", strings.TrimSpace(string(out)))
	}
	if compareVersions(version, minimum) < 0 {
		return fmt.Errorf("version %s is too old; need %s or newer", version, minimum)
	}
	return nil
}

func compareVersions(a, b string) int {
	parse := func(value string) [3]int {
		var result [3]int
		parts := strings.Split(value, ".")
		for i := 0; i < len(result) && i < len(parts); i++ {
			result[i], _ = strconv.Atoi(parts[i])
		}
		return result
	}
	aa, bb := parse(a), parse(b)
	for i := range aa {
		if aa[i] < bb[i] {
			return -1
		}
		if aa[i] > bb[i] {
			return 1
		}
	}
	return 0
}

func (c *KrunkitClient) RunDetached(opts RunOpts) (retErr error) {
	if !validVMName(opts.Name) {
		return fmt.Errorf("invalid krunkit VM name %q", opts.Name)
	}
	if opts.Image == "" {
		return fmt.Errorf("krunkit VM %s needs a bootable raw disk image", opts.Name)
	}
	image, err := filepath.Abs(opts.Image)
	if err != nil {
		return err
	}
	imageInfo, err := os.Stat(image)
	if err != nil || !imageInfo.Mode().IsRegular() {
		return fmt.Errorf("krunkit boot disk %q is not a regular file", opts.Image)
	}
	if opts.Entrypoint != "" || len(opts.Args) > 0 || len(opts.Env) > 0 || opts.Kernel != "" {
		return fmt.Errorf("krunkit VM %s must be provisioned after boot; OCI entrypoint, args, env, and custom kernels are unsupported", opts.Name)
	}
	if err := ValidateMounts(opts.Mounts); err != nil {
		return err
	}
	cpus, err := parseCPUCount(opts.CPUs)
	if err != nil {
		return err
	}
	memoryBytes, err := parseSizeBytes(opts.Memory, 4<<30)
	if err != nil || memoryBytes%(1<<20) != 0 {
		return fmt.Errorf("invalid krunkit memory %q: use whole MiB or GiB", opts.Memory)
	}
	memoryMiB := int(memoryBytes >> 20)
	hostMemoryMiB, err := c.hostMemoryMiB()
	if err != nil {
		return err
	}
	if memoryMiB < 1024 || memoryMiB > 60*1024 || effectiveGPUMemoryMiB(memoryMiB, hostMemoryMiB) == 0 {
		return fmt.Errorf("krunkit memory must be between 1G and 60G")
	}
	diskBytes, err := parseSizeBytes(opts.DiskSize, defaultDiskBytes)
	if err != nil {
		return err
	}
	if diskBytes < imageInfo.Size() {
		return fmt.Errorf("krunkit disk size %s is smaller than the %d-byte base image", opts.DiskSize, imageInfo.Size())
	}

	unlock, err := c.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	if c.Owns(opts.Name) {
		return fmt.Errorf("krunkit VM %q already exists", opts.Name)
	}
	network, err := c.ensureNetwork(opts.NetworkID)
	if err != nil {
		return err
	}
	created := false
	nodeDir := ""
	defer func() {
		if retErr != nil && !created {
			if nodeDir != "" {
				_ = os.RemoveAll(nodeDir)
			}
			c.removeUnusedNetwork(opts.NetworkID)
		}
	}()
	nodeDir, err = c.nodeDir(opts.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		return err
	}
	keyPath, publicKey, err := c.ensureSSHKey()
	if err != nil {
		return err
	}
	_ = keyPath
	mac, err := randomMAC()
	if err != nil {
		return err
	}
	state := KrunkitNodeState{
		Schema:       krunkitStateSchema,
		Name:         opts.Name,
		Image:        image,
		Distro:       opts.Distro,
		K8sVersion:   opts.K8sVersion,
		GPU:          opts.GPU,
		Disk:         filepath.Join(nodeDir, "disk.img"),
		Seed:         filepath.Join(nodeDir, "cidata.iso"),
		MAC:          mac,
		CPUs:         cpus,
		MemoryMiB:    memoryMiB,
		GPUMemoryMiB: effectiveGPUMemoryMiB(memoryMiB, hostMemoryMiB),
		NetworkID:    opts.NetworkID,
		NetworkCIDR:  network.CIDR,
		SSHUser:      defaultSSHUser,
		Status:       "creating",
		Created:      time.Now().UTC(),
		Mounts:       append([]Mount(nil), opts.Mounts...),
	}
	state.RestSocket, err = c.shortSocketPath(opts.Name)
	if err != nil {
		return err
	}
	if err := cloneDisk(image, state.Disk, diskBytes); err != nil {
		return fmt.Errorf("creating disk for %s: %w", opts.Name, err)
	}
	if err := c.createCloudInitISO(nodeDir, state, publicKey, opts.DNS); err != nil {
		return err
	}
	state.Status = "stopped"
	if err := c.writeState(state); err != nil {
		return err
	}
	if err := c.startState(&state); err != nil {
		return err
	}
	created = true
	return nil
}

func (c *KrunkitClient) hostMemoryMiB() (int, error) {
	if c.HostMemoryMiB > 0 {
		return c.HostMemoryMiB, nil
	}
	sysctl, err := c.resolveBinary(c.SysctlBin, "/usr/sbin/sysctl")
	if err != nil {
		return 0, fmt.Errorf("reading host memory: %w", err)
	}
	out, err := exec.Command(sysctl, "-n", "hw.memsize").CombinedOutput()
	if err != nil {
		return 0, &CommandError{Tool: sysctl, Args: []string{"-n", "hw.memsize"}, Output: string(out), Err: err}
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || bytes <= 0 {
		return 0, fmt.Errorf("reading host memory: unexpected hw.memsize %q", strings.TrimSpace(string(out)))
	}
	return int(bytes >> 20), nil
}

func validVMName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0 && i < len(name)-1) {
			continue
		}
		return false
	}
	return true
}

func cloneDisk(source, destination string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	_ = os.Remove(destination)
	if out, err := exec.Command("/bin/cp", "-c", source, destination).CombinedOutput(); err != nil {
		_ = os.Remove(destination)
		src, openErr := os.Open(source)
		if openErr != nil {
			return openErr
		}
		defer src.Close()
		dst, createErr := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return fmt.Errorf("copy-on-write clone failed (%s), and fallback copy failed: %w", strings.TrimSpace(string(out)), createErr)
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return err
	}
	return os.Truncate(destination, size)
}

func (c *KrunkitClient) ensureSSHKey() (string, string, error) {
	root, err := c.rootDir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	key := filepath.Join(root, "id_ed25519")
	pub := key + ".pub"
	if _, err := os.Stat(key); errors.Is(err, os.ErrNotExist) {
		keygen, err := c.resolveBinary(c.SSHKeygenBin, "/usr/bin/ssh-keygen")
		if err != nil {
			return "", "", err
		}
		args := []string{"-q", "-t", "ed25519", "-N", "", "-f", key}
		out, err := exec.Command(keygen, args...).CombinedOutput()
		if err != nil {
			return "", "", &CommandError{Tool: keygen, Args: args, Output: string(out), Err: err}
		}
	}
	if err := os.Chmod(key, 0o600); err != nil {
		return "", "", err
	}
	raw, err := os.ReadFile(pub)
	if err != nil {
		return "", "", err
	}
	return key, strings.TrimSpace(string(raw)), nil
}

func (c *KrunkitClient) startState(state *KrunkitNodeState) error {
	if c.processOwned(state) {
		state.Status = "running"
		return c.writeState(*state)
	}
	vmnetRun, err := c.resolveVMNetRun()
	if err != nil {
		return err
	}
	krunkit, err := c.resolveBinary(c.Bin)
	if err != nil {
		return err
	}
	network, err := c.loadNetwork(state.NetworkID)
	if err != nil {
		return err
	}
	nodeDir, err := c.nodeDir(state.Name)
	if err != nil {
		return err
	}
	if state.Disk != filepath.Join(nodeDir, "disk.img") || state.Seed != filepath.Join(nodeDir, "cidata.iso") {
		return fmt.Errorf("krunkit VM %s has unsafe disk state; delete and recreate it", state.Name)
	}
	restSocket := state.RestSocket
	if restSocket == "" {
		restSocket, err = c.shortSocketPath(state.Name)
		if err != nil {
			return err
		}
		state.RestSocket = restSocket
	} else {
		expectedSocket, err := c.shortSocketPath(state.Name)
		if err != nil {
			return err
		}
		if restSocket != expectedSocket {
			return fmt.Errorf("krunkit VM %s has unsafe REST socket state; delete and recreate it", state.Name)
		}
	}
	_ = os.Remove(restSocket)
	// Fedora may replace its transient first-boot host key before cloud-init
	// settles. The target is still authenticated by this node's recorded MAC,
	// isolated vmnet subnet, and Kiac client key; relearn the host key per boot.
	_ = os.Remove(filepath.Join(nodeDir, "known_hosts"))
	args := []string{
		"--operation-mode=shared",
		"--start-address=" + network.Start,
		"--end-address=" + network.End,
		"--subnet-mask=" + network.Netmask,
		"--",
		krunkit,
		fmt.Sprintf("--memory=%d", state.MemoryMiB),
		fmt.Sprintf("--cpus=%d", state.CPUs),
		"--restful-uri=unix://" + restSocket,
		"--pidfile=" + filepath.Join(nodeDir, "krunkit.pid"),
		"--log-file=" + filepath.Join(nodeDir, "krunkit.log"),
		"--krun-log-level=1",
		"--device=virtio-blk,path=" + state.Disk,
		"--device=virtio-blk,path=" + state.Seed,
		"--device=virtio-serial,logFilePath=" + filepath.Join(nodeDir, "serial.log"),
		"--device=virtio-rng",
		"--device=virtio-net,type=unixgram,fd=4,mac=" + state.MAC + ",offloading=off",
	}
	for i, mount := range state.Mounts {
		args = append(args, fmt.Sprintf("--device=virtio-fs,sharedDir=%s,mountTag=kiac%d", mount.Source, i))
	}
	logFile, err := os.OpenFile(filepath.Join(nodeDir, "vmnet.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(vmnetRun, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	logFile.Close()
	state.PID = cmd.Process.Pid
	state.ProcessStart, err = c.processStartTime(state.PID)
	if err != nil {
		terminateStartedCommand(cmd, durationOr(c.KillTimeout, 5*time.Second))
		return fmt.Errorf("recording process identity for %s: %w", state.Name, err)
	}
	processCommand, err := c.processCommand(state.PID)
	if err != nil {
		terminateStartedCommand(cmd, durationOr(c.KillTimeout, 5*time.Second))
		return fmt.Errorf("recording process command for %s: %w", state.Name, err)
	}
	state.ProcessCommandHash = processCommandHash(processCommand)
	state.IP = ""
	state.Status = "running"
	if err := c.writeState(*state); err != nil {
		terminateStartedCommand(cmd, durationOr(c.KillTimeout, 5*time.Second))
		return err
	}
	if err := cmd.Process.Release(); err != nil {
		terminateStartedCommand(cmd, durationOr(c.KillTimeout, 5*time.Second))
		return err
	}
	return nil
}

func (c *KrunkitClient) loadNetwork(id string) (krunkitNetworkState, error) {
	if !validKrunkitNetworkID(id) {
		return krunkitNetworkState{}, fmt.Errorf("invalid krunkit network ID %q", id)
	}
	root, err := c.rootDir()
	if err != nil {
		return krunkitNetworkState{}, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "networks", id+".json"))
	if err != nil {
		return krunkitNetworkState{}, err
	}
	var network krunkitNetworkState
	if err := json.Unmarshal(raw, &network); err != nil {
		return krunkitNetworkState{}, err
	}
	if err := validateKrunkitNetwork(network, id); err != nil {
		return krunkitNetworkState{}, err
	}
	return network, nil
}

func (c *KrunkitClient) Exec(name string, command ...string) (string, error) {
	return c.execContext(context.Background(), name, nil, command...)
}

func (c *KrunkitClient) ExecTimeout(name string, timeout time.Duration, command ...string) (string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.execContext(ctx, name, nil, command...)
}

func (c *KrunkitClient) ExecStdin(name string, input io.Reader, command ...string) error {
	_, err := c.execContext(context.Background(), name, input, command...)
	return err
}

func (c *KrunkitClient) execContext(ctx context.Context, name string, input io.Reader, command ...string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("no command specified for krunkit VM %s", name)
	}
	state, err := c.readState(name)
	if err != nil {
		return "", err
	}
	if !c.processOwned(&state) {
		return "", fmt.Errorf("krunkit VM %s is not running", name)
	}
	ip := c.findARPAddress(state.MAC)
	if ip == "" {
		ip = state.IP
	}
	host := ip
	if host == "" {
		host = state.Name + ".local"
	}
	ssh, err := c.resolveBinary(c.SSHBin, "/usr/bin/ssh")
	if err != nil {
		return "", err
	}
	root, err := c.rootDir()
	if err != nil {
		return "", err
	}
	nodeDir, err := c.nodeDir(name)
	if err != nil {
		return "", err
	}
	remote := "sudo -- " + shellJoin(command)
	args := []string{
		"-F", "/dev/null",
		"-i", filepath.Join(root, "id_ed25519"),
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(nodeDir, "known_hosts"),
		"-o", "ConnectTimeout=5",
		state.SSHUser + "@" + host,
		remote,
	}
	cmd := exec.CommandContext(ctx, ssh, args...)
	cmd.Stdin = input
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return string(out), &CommandError{Tool: ssh, Args: redactSSHArgs(args), Output: string(out), Err: err}
	}
	if ip != "" && ip != state.IP {
		_ = c.updateState(name, func(latest *KrunkitNodeState) { latest.IP = ip })
	}
	return string(out), nil
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func redactSSHArgs(args []string) []string {
	result := append([]string(nil), args...)
	for i := 0; i+1 < len(result); i++ {
		if result[i] == "-i" {
			result[i+1] = "<kiac-ssh-key>"
		}
	}
	return result
}

func (c *KrunkitClient) WaitReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		state, err := c.readState(name)
		if err != nil {
			return err
		}
		if !c.processOwned(&state) {
			return fmt.Errorf("krunkit VM %s exited during boot; inspect: %s", name, filepath.Join(filepath.Dir(state.Disk), "vmnet.log"))
		}
		if _, err := c.ExecTimeout(name, 8*time.Second, "true"); err == nil {
			remaining := time.Until(deadline)
			if remaining > 2*time.Minute {
				remaining = 2 * time.Minute
			}
			// cloud-init exits 2 when boot completed with recoverable warnings
			// (Fedora can transiently fail a hostname write before systemd is
			// fully online). That is a terminal ready state, not an invitation
			// to poll until the whole cluster timeout expires.
			if _, err := c.ExecTimeout(name, remaining, "sh", "-c",
				`cloud-init status --wait; rc=$?; [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]`); err == nil {
				return nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("krunkit VM %s did not become SSH-ready in %s: %w", name, timeout, lastErr)
	}
	return fmt.Errorf("krunkit VM %s did not become SSH-ready in %s", name, timeout)
}

func (c *KrunkitClient) IP(name string) (string, error) {
	out, err := c.Exec(name, "sh", "-c", `ip -4 route get 1.1.1.1 | awk '{for(i=1;i<NF;i++) if ($i=="src") print $(i+1)}'`)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if net.ParseIP(ip).To4() == nil {
		return "", fmt.Errorf("could not determine IPv4 address of krunkit VM %s", name)
	}
	state, err := c.readState(name)
	if err == nil && state.IP != ip {
		_ = c.updateState(name, func(latest *KrunkitNodeState) { latest.IP = ip })
	}
	return ip, nil
}

func (c *KrunkitClient) IPv6(name string) (string, error) {
	out, err := c.Exec(name, "sh", "-c", `ip -6 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1`)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(out)
	if value == "" {
		return "", nil
	}
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return "", nil
	}
	return value, nil
}

func (c *KrunkitClient) findARPAddress(mac string) string {
	arp, err := c.resolveBinary(c.ARPBin, "/usr/sbin/arp")
	if err != nil {
		return ""
	}
	out, err := exec.Command(arp, "-an").Output()
	if err != nil {
		return ""
	}
	want := normalizeMAC(mac)
	re := regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)\) at ([0-9a-fA-F:]+)`)
	for _, match := range re.FindAllStringSubmatch(string(out), -1) {
		if normalizeMAC(match[2]) == want {
			return match[1]
		}
	}
	return ""
}

func normalizeMAC(value string) string {
	parts := strings.Split(strings.ToLower(value), ":")
	for i, part := range parts {
		if len(part) == 1 {
			parts[i] = "0" + part
		}
	}
	return strings.Join(parts, ":")
}

func (c *KrunkitClient) Logs(name string, _ time.Duration) (string, error) {
	if _, err := c.readState(name); err != nil {
		return "", err
	}
	dir, err := c.nodeDir(name)
	if err != nil {
		return "", err
	}
	var result strings.Builder
	for _, file := range []string{"vmnet.log", "krunkit.log", "serial.log"} {
		path := filepath.Join(dir, file)
		raw, err := readTail(path, 512<<10)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if len(raw) > 0 {
			fmt.Fprintf(&result, "==> %s <==\n%s\n", file, raw)
		}
	}
	return result.String(), nil
}

func readTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func (c *KrunkitClient) List(prefix string) ([]Info, error) {
	unlock, err := c.lockStore()
	if err != nil {
		return nil, err
	}
	defer unlock()
	root, err := c.rootDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "nodes")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		state, err := c.readState(entry.Name())
		if err != nil {
			return nil, err
		}
		status := "stopped"
		ip := ""
		originalProcessStart := state.ProcessStart
		originalProcessCommandHash := state.ProcessCommandHash
		if c.processOwned(&state) {
			status = "running"
			ip = c.findARPAddress(state.MAC)
			if ip == "" {
				ip = state.IP
			}
		}
		if state.Status != status || (ip != "" && ip != state.IP) ||
			state.ProcessStart != originalProcessStart || state.ProcessCommandHash != originalProcessCommandHash {
			state.Status = status
			state.IP = ip
			if status == "stopped" {
				state.PID = 0
				state.ProcessStart = ""
				state.ProcessCommandHash = ""
			}
			_ = c.writeState(state)
		}
		infos = append(infos, Info{
			Name:       state.Name,
			Image:      state.Image,
			Status:     status,
			IP:         ip,
			Created:    state.Created.Format(time.RFC3339),
			Backend:    BackendKrunkit,
			Distro:     state.Distro,
			K8sVersion: state.K8sVersion,
			GPU:        state.GPU,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

func (c *KrunkitClient) Stop(name string) error {
	unlock, err := c.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	return c.stopLocked(name)
}

func (c *KrunkitClient) stopLocked(name string) error {
	state, err := c.readState(name)
	if err != nil {
		return err
	}
	if !c.processOwned(&state) {
		state.PID = 0
		state.ProcessStart = ""
		state.ProcessCommandHash = ""
		state.Status = "stopped"
		return c.writeState(state)
	}
	expectedSocket, _ := c.shortSocketPath(name)
	if state.RestSocket == expectedSocket {
		if err := c.requestStop(expectedSocket); err == nil {
			deadline := time.Now().Add(durationOr(c.StopTimeout, 20*time.Second))
			for c.processOwned(&state) && time.Now().Before(deadline) {
				time.Sleep(250 * time.Millisecond)
			}
		}
	}
	if c.processOwned(&state) {
		if err := signalProcess(state.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(durationOr(c.KillTimeout, 5*time.Second))
		for c.processOwned(&state) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if c.processOwned(&state) {
		if err := signalProcess(state.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(durationOr(c.KillTimeout, 5*time.Second))
		for c.processOwned(&state) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if c.processOwned(&state) {
			return fmt.Errorf("krunkit VM %s process %d did not exit after SIGKILL", name, state.PID)
		}
	}
	state.PID = 0
	state.ProcessStart = ""
	state.ProcessCommandHash = ""
	state.IP = ""
	state.Status = "stopped"
	return c.writeState(state)
}

func (c *KrunkitClient) requestStop(socket string) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	resp, err := client.Post("http://krunkit/vm/state", "application/json", bytes.NewBufferString(`{"state":"Stop"}`))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("krunkit stop endpoint returned %s", resp.Status)
	}
	return nil
}

func (c *KrunkitClient) Start(name string) error {
	unlock, err := c.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := c.readState(name)
	if err != nil {
		return err
	}
	return c.startState(&state)
}

func (c *KrunkitClient) Remove(names ...string) error {
	unlock, err := c.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	var firstErr error
	for _, name := range names {
		if !c.Owns(name) {
			continue
		}
		state, err := c.readState(name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := c.stopLocked(name); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		nodeDir, dirErr := c.nodeDir(name)
		if dirErr != nil {
			if firstErr == nil {
				firstErr = dirErr
			}
			continue
		}
		if err := os.RemoveAll(nodeDir); err != nil && firstErr == nil {
			firstErr = err
		}
		if socket, socketErr := c.shortSocketPath(name); socketErr == nil {
			_ = os.Remove(socket)
		}
		c.removeUnusedNetwork(state.NetworkID)
	}
	return firstErr
}

func (c *KrunkitClient) removeUnusedNetwork(id string) {
	if !validKrunkitNetworkID(id) {
		return
	}
	root, err := c.rootDir()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(filepath.Join(root, "nodes"))
	for _, entry := range entries {
		state, err := c.readState(entry.Name())
		if err == nil && state.NetworkID == id {
			return
		}
	}
	_ = os.Remove(filepath.Join(root, "networks", id+".json"))
}

func (c *KrunkitClient) processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func (c *KrunkitClient) processOwned(state *KrunkitNodeState) bool {
	if !c.processAlive(state.PID) {
		return false
	}
	started, err := c.processStartTime(state.PID)
	if err != nil {
		return false
	}
	if state.ProcessStart != "" && state.ProcessStart != started {
		return false
	}
	command, err := c.processCommand(state.PID)
	if err != nil {
		return false
	}
	commandHash := processCommandHash(command)
	if state.ProcessStart != "" && state.ProcessCommandHash != "" {
		return state.ProcessCommandHash == commandHash
	}
	// Adopt state written by the pre-identity backend only when the live
	// command points back to this exact VM. Never trust a bare legacy PID.
	nodeDir, dirErr := c.nodeDir(state.Name)
	nodeDirMatches := dirErr == nil && strings.Contains(command, nodeDir)
	if !nodeDirMatches &&
		(state.RestSocket == "" || !strings.Contains(command, state.RestSocket)) {
		return false
	}
	state.ProcessStart = started
	state.ProcessCommandHash = commandHash
	return true
}

func processCommandHash(command string) string {
	digest := sha256.Sum256([]byte(command))
	return fmt.Sprintf("%x", digest[:])
}

func (c *KrunkitClient) processStartTime(pid int) (string, error) {
	ps, err := c.resolveBinary(c.PSBin, "/bin/ps")
	if err != nil {
		return "", err
	}
	args := []string{"-p", strconv.Itoa(pid), "-o", "lstart="}
	out, err := exec.Command(ps, args...).CombinedOutput()
	if err != nil {
		return "", &CommandError{Tool: ps, Args: args, Output: string(out), Err: err}
	}
	started := strings.TrimSpace(string(out))
	if started == "" {
		return "", fmt.Errorf("process %d has no start time", pid)
	}
	return started, nil
}

func (c *KrunkitClient) processCommand(pid int) (string, error) {
	ps, err := c.resolveBinary(c.PSBin, "/bin/ps")
	if err != nil {
		return "", err
	}
	args := []string{"-p", strconv.Itoa(pid), "-o", "command="}
	out, err := exec.Command(ps, args...).CombinedOutput()
	if err != nil {
		return "", &CommandError{Tool: ps, Args: args, Output: string(out), Err: err}
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *KrunkitClient) updateState(name string, update func(*KrunkitNodeState)) error {
	unlock, err := c.lockStore()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := c.readState(name)
	if err != nil {
		return err
	}
	update(&state)
	return c.writeState(state)
}

func signalProcess(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ESRCH) {
		return syscall.Kill(pid, signal)
	}
	return err
}

// terminateStartedCommand is used before Release when post-start bookkeeping
// fails. Waiting here ensures RunDetached cannot return while an untracked
// vmnet-run process (or its krunkit child) is still alive.
func terminateStartedCommand(cmd *exec.Cmd, timeout time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = signalProcess(cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		_ = signalProcess(cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (c *KrunkitClient) resolveVMNetRun() (string, error) {
	return c.resolveBinary(c.VMNetRunBin,
		"/opt/homebrew/opt/vmnet-helper/libexec/vmnet-run",
		"/opt/vmnet-helper/bin/vmnet-run",
	)
}

func (c *KrunkitClient) resolveBinary(value string, candidates ...string) (string, error) {
	if value != "" {
		if strings.ContainsRune(value, os.PathSeparator) {
			if info, err := os.Stat(value); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return value, nil
			}
		} else if path, err := exec.LookPath(value); err == nil {
			return path, nil
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found", value)
}
