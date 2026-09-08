package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestValidKrunkitNetworkID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"dev", true},
		{"gpu-lab-2", true},
		{"", false},
		{"../outside", false},
		{"Dev", false},
		{"-dev", true},
		{"dev-", true},
		{"dev--gpu", true},
	} {
		if got := validKrunkitNetworkID(tc.id); got != tc.want {
			t.Errorf("validKrunkitNetworkID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestValidateKrunkitNetwork(t *testing.T) {
	valid := krunkitNetworkState{
		Schema: krunkitStateSchema, ID: "dev", CIDR: "192.168.120.0/24",
		Start: "192.168.120.1", End: "192.168.120.200", Netmask: "255.255.255.0",
	}
	if err := validateKrunkitNetwork(valid, "dev"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*krunkitNetworkState){
		func(network *krunkitNetworkState) { network.Schema++ },
		func(network *krunkitNetworkState) { network.ID = "other" },
		func(network *krunkitNetworkState) { network.CIDR = "10.0.0.0/24" },
		func(network *krunkitNetworkState) { network.Start = "192.168.120.2" },
		func(network *krunkitNetworkState) { network.End = "192.168.120.250" },
		func(network *krunkitNetworkState) { network.Netmask = "255.255.0.0" },
	} {
		network := valid
		mutate(&network)
		if err := validateKrunkitNetwork(network, "dev"); err == nil {
			t.Fatalf("invalid network was accepted: %+v", network)
		}
	}
}

func TestKrunkitLifecycleUsesDurableInventory(t *testing.T) {
	root := t.TempDir()
	binDir := t.TempDir()
	krunkit := writeExecutable(t, binDir, "krunkit", "#!/bin/sh\nexit 0\n")
	vmnetRun := writeExecutable(t, binDir, "vmnet-run", "#!/bin/sh\nsleep 30\n")
	hdiutil := writeExecutable(t, binDir, "hdiutil", `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    : > "$1"
    exit 0
  fi
  shift
done
exit 1
`)
	arp := writeExecutable(t, binDir, "arp", "#!/bin/sh\nprintf '%s\\n' '? (192.168.222.7) at 2:a:b:c:d:e on bridge101 ifscope [bridge]'\n")

	if err := os.WriteFile(filepath.Join(root, "id_ed25519"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "id_ed25519.pub"), []byte("ssh-ed25519 AAAATEST kiac-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "fedora.raw")
	if err := os.WriteFile(base, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &KrunkitClient{
		Bin:           krunkit,
		VMNetRunBin:   vmnetRun,
		HDIUtilBin:    hdiutil,
		ARPBin:        arp,
		RootDir:       root,
		HostMemoryMiB: 64 * 1024,
		StopTimeout:   10 * time.Millisecond,
		KillTimeout:   10 * time.Millisecond,
	}
	if err := c.RunDetached(RunOpts{
		Backend:   BackendKrunkit,
		Name:      "kiac-dev-control-plane",
		Image:     base,
		CPUs:      "4",
		Memory:    "2G",
		DiskSize:  "2M",
		NetworkID: "dev",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Remove("kiac-dev-control-plane") })

	if !c.Owns("kiac-dev-control-plane") {
		t.Fatal("created VM is absent from durable inventory")
	}
	state, err := c.State("kiac-dev-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if state.MemoryMiB != 2048 || state.GPUMemoryMiB != 59*1024 || state.NetworkID != "dev" {
		t.Fatalf("state = %+v", state)
	}
	if state.ProcessStart == "" || state.ProcessCommandHash == "" {
		t.Fatalf("process identity was not persisted: %+v", state)
	}
	if got := c.findARPAddress(state.MAC); got != "" {
		t.Fatalf("unrelated ARP address matched as %q", got)
	}
	state.MAC = "02:0a:0b:0c:0d:0e"
	if err := c.writeState(state); err != nil {
		t.Fatal(err)
	}
	if got := c.findARPAddress(state.MAC); got != "192.168.222.7" {
		t.Fatalf("ARP address = %q", got)
	}

	infos, err := c.List("kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Backend != BackendKrunkit || infos[0].Status != "running" {
		t.Fatalf("inventory = %+v", infos)
	}
	if err := c.Stop("kiac-dev-control-plane"); err != nil {
		t.Fatal(err)
	}
	infos, err = c.List("kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Status != "stopped" {
		t.Fatalf("stopped inventory = %+v", infos)
	}
	if err := c.Start("kiac-dev-control-plane"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("kiac-dev-control-plane"); err != nil {
		t.Fatal(err)
	}
	if c.Owns("kiac-dev-control-plane") {
		t.Fatal("removed VM remains in inventory")
	}
	if _, err := os.Stat(filepath.Join(root, "networks", "dev.json")); !os.IsNotExist(err) {
		t.Fatalf("unused network state was not removed: %v", err)
	}
}

func TestKrunkitCloudInitCarriesDNSMountsAndPermissions(t *testing.T) {
	root := t.TempDir()
	hdiutil := writeExecutable(t, root, "hdiutil", `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; : > "$1"; exit 0; fi
  shift
done
exit 1
`)
	nodeDir := filepath.Join(root, "node")
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &KrunkitClient{RootDir: root, HDIUtilBin: hdiutil}
	state := KrunkitNodeState{
		Name: "kiac-dev-gpu-1", MAC: "02:01:02:03:04:05",
		Seed:   filepath.Join(nodeDir, "cidata.iso"),
		Mounts: []Mount{{Source: "/tmp/source", Target: "/work dir", ReadOnly: true}},
	}
	if err := c.createCloudInitISO(nodeDir, state, "ssh-ed25519 AAAATEST", []string{"1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	userData, err := os.ReadFile(filepath.Join(nodeDir, "cloud-init", "user-data"))
	if err != nil {
		t.Fatal(err)
	}
	network, err := os.ReadFile(filepath.Join(nodeDir, "cloud-init", "network-config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config", "70-kiac-gpu.rules", "kiac0 /work\\040dir virtiofs", "ssh-ed25519 AAAATEST"} {
		if !strings.Contains(string(userData), want) {
			t.Errorf("user-data does not contain %q:\n%s", want, userData)
		}
	}
	for _, want := range []string{"02:01:02:03:04:05", "1.1.1.1", "use-dns: false"} {
		if !strings.Contains(string(network), want) {
			t.Errorf("network-config does not contain %q:\n%s", want, network)
		}
	}
}

func TestKrunkitHelpers(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", defaultDiskBytes},
		{"2048M", 2 << 30},
		{"20GiB", 20 << 30},
	} {
		got, err := parseSizeBytes(tc.value, defaultDiskBytes)
		if err != nil || got != tc.want {
			t.Errorf("parseSizeBytes(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
	if _, err := parseSizeBytes("2.5G", 0); err == nil {
		t.Fatal("fractional size unexpectedly accepted")
	}
	if got := shellJoin([]string{"sh", "-c", "printf '%s' hello world"}); got != `'sh' '-c' 'printf '\''%s'\'' hello world'` {
		t.Fatalf("shellJoin = %q", got)
	}
	if compareVersions("1.3.2", "1.3.2") != 0 || compareVersions("1.4.0", "1.3.2") <= 0 || compareVersions("1.2.9", "1.3.2") >= 0 {
		t.Fatal("version comparison is incorrect")
	}
	if !validVMName("kiac-dev-gpu-1") || validVMName("Bad_Name") || validVMName("-bad") {
		t.Fatal("VM name validation is incorrect")
	}
	if got := effectiveGPUMemoryMiB(8*1024, 64*1024); got != 53*1024 {
		t.Fatalf("effective GPU memory = %d MiB, want %d", got, 53*1024)
	}
	for _, formula := range []string{"virglrenderer", "homebrew/core/virglrenderer", "libkrun/krun/virglrenderer", "slp/krun/virglrenderer"} {
		if !supportedVirglFormula(formula) {
			t.Errorf("supported virglrenderer formula %q was rejected", formula)
		}
	}
	if supportedVirglFormula("unrelated/tap/virglrenderer") {
		t.Fatal("unrelated virglrenderer formula was accepted")
	}
}

func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTailIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTail(path, 4)
	if err != nil || string(got) != "6789" {
		t.Fatalf("readTail = %q, %v", got, err)
	}
}

func TestKrunkitStopMissingProcessIsFast(t *testing.T) {
	c := &KrunkitClient{RootDir: t.TempDir()}
	state := KrunkitNodeState{Schema: krunkitStateSchema, Name: "kiac-dev-worker-1", PID: 999999, Status: "running", Created: time.Now()}
	if err := c.writeState(state); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(state.Name); err != nil {
		t.Fatal(err)
	}
}

func TestKrunkitStopDoesNotSignalReusedPID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	c := &KrunkitClient{RootDir: t.TempDir()}
	started, err := c.processStartTime(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	state := KrunkitNodeState{
		Schema: krunkitStateSchema, Name: "kiac-dev-gpu-1", PID: cmd.Process.Pid,
		ProcessStart: started, ProcessCommandHash: "not-the-live-command", Status: "running",
		Disk: filepath.Join(c.RootDir, "nodes", "kiac-dev-gpu-1", "disk.img"), Created: time.Now(),
	}
	if err := c.writeState(state); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(state.Name); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was signaled: %v", err)
	}
	stored, err := c.readState(state.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PID != 0 || stored.Status != "stopped" {
		t.Fatalf("stale state was not cleared: %+v", stored)
	}
}

func TestKrunkitStoreLockCoordinatesClients(t *testing.T) {
	root := t.TempDir()
	first := &KrunkitClient{RootDir: root}
	second := &KrunkitClient{RootDir: root}
	unlockFirst, err := first.lockStore()
	if err != nil {
		t.Fatal(err)
	}
	type lockResult struct {
		unlock func()
		err    error
	}
	acquired := make(chan lockResult, 1)
	go func() {
		unlock, err := second.lockStore()
		acquired <- lockResult{unlock: unlock, err: err}
	}()
	select {
	case result := <-acquired:
		if result.unlock != nil {
			result.unlock()
		}
		t.Fatal("second client acquired a held store lock")
	case <-time.After(75 * time.Millisecond):
	}
	unlockFirst()
	select {
	case result := <-acquired:
		if result.err != nil {
			t.Fatal(result.err)
		}
		result.unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("second client did not acquire the released store lock")
	}
}

func TestKrunkitFailedCreateTerminatesProcessAndRemovesState(t *testing.T) {
	root := t.TempDir()
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "vmnet.pid")
	vmnetRun := writeExecutable(t, binDir, "vmnet-run", "#!/bin/sh\nprintf '%s' \"$$\" > \""+pidFile+"\"\nsleep 30\n")
	hdiutil := writeExecutable(t, binDir, "hdiutil", `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; : > "$1"; exit 0; fi
  shift
done
exit 1
`)
	badPS := writeExecutable(t, binDir, "ps", "#!/bin/sh\nexit 1\n")
	if err := os.WriteFile(filepath.Join(root, "id_ed25519"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "id_ed25519.pub"), []byte("ssh-ed25519 AAAATEST kiac-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "fedora.raw")
	if err := os.WriteFile(base, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &KrunkitClient{
		Bin: writeExecutable(t, binDir, "krunkit", "#!/bin/sh\nexit 0\n"), VMNetRunBin: vmnetRun,
		HDIUtilBin: hdiutil, PSBin: badPS, RootDir: root, HostMemoryMiB: 64 * 1024,
		KillTimeout: 500 * time.Millisecond,
	}
	err := c.RunDetached(RunOpts{Name: "kiac-fail-gpu-1", Image: base, CPUs: "2", Memory: "2G", DiskSize: "2M", NetworkID: "fail"})
	if err == nil || !strings.Contains(err.Error(), "recording process identity") {
		t.Fatalf("RunDetached error = %v", err)
	}
	rawPID, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(string(rawPID))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("failed create leaked process %d", pid)
	}
	if c.Owns("kiac-fail-gpu-1") {
		t.Fatal("failed create left durable node state")
	}
	if _, err := os.Stat(filepath.Join(root, "networks", "fail.json")); !os.IsNotExist(err) {
		t.Fatalf("failed create left stale network state: %v", err)
	}
}

// This opt-in acceptance test boots a real Fedora disk through the backend and
// exercises durable stop/start/delete. It is reserved for the self-hosted
// Apple-silicon runtime-smoke job.
func TestKrunkitRealLifecycle(t *testing.T) {
	image := os.Getenv("KIAC_TEST_KRUNKIT_IMAGE")
	if image == "" {
		t.Skip("set KIAC_TEST_KRUNKIT_IMAGE to a bootable raw ARM64 disk")
	}
	c := NewKrunkit()
	c.RootDir = filepath.Join(t.TempDir(), "state")
	if err := c.Preflight(); err != nil {
		t.Fatal(err)
	}
	const name = "kiac-backend-e2e"
	if err := c.RunDetached(RunOpts{
		Backend: BackendKrunkit, Name: name, Image: image,
		CPUs: "2", Memory: "4G", DiskSize: "8G", NetworkID: "backend-e2e",
		DNS: []string{"1.1.1.1"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Remove(name) })
	if err := c.WaitReady(name, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	out, err := c.Exec(name, "sh", "-c", "uname -m; test -e /dev/dri/renderD128")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "aarch64") {
		t.Fatalf("unexpected guest architecture: %q", out)
	}
	firstIP, err := c.IP(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(name); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(name); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitReady(name, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	secondIP, err := c.IP(name)
	if err != nil {
		t.Fatal(err)
	}
	if firstIP == "" || secondIP == "" {
		t.Fatalf("invalid IPs before/after restart: %q, %q", firstIP, secondIP)
	}
	if err := c.Remove(name); err != nil {
		t.Fatal(err)
	}
	if c.Owns(name) {
		t.Fatal("deleted real VM remains in backend inventory")
	}
}
