package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func addressTestClient(t *testing.T) (*KrunkitClient, KrunkitNodeState, guestAddressReport) {
	t.Helper()
	c, name := sshTestClient(t, "#!/bin/sh\nexit 0\n")
	state, err := c.State(name)
	if err != nil {
		t.Fatal(err)
	}
	network, err := c.ensureNetwork("address-test")
	if err != nil {
		t.Fatal(err)
	}
	state.NetworkID = network.ID
	state.NetworkCIDR = network.CIDR
	state.MAC = "02:0a:0b:0c:0d:0e"
	state.GuestAddressReport = true
	if err := c.prepareGuestAddressReport(&state); err != nil {
		t.Fatal(err)
	}
	if err := c.writeState(state); err != nil {
		t.Fatal(err)
	}
	return c, state, guestAddressReport{Schema: 1, Name: name, MAC: state.MAC, Generation: state.AddressGeneration, IPv4: strings.TrimSuffix(network.Start, ".1") + ".7"}
}

func writeAddressReport(t *testing.T, c *KrunkitClient, state KrunkitNodeState, report guestAddressReport) string {
	t.Helper()
	dir, err := c.guestAddressDir(state.Name)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, guestAddressReportFile)
	if err := writeJSONAtomic(filename, report, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestKrunkitGuestAddressRejectsUntrustedReports(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*guestAddressReport)
	}{
		{"valid", func(*guestAddressReport) {}},
		{"schema", func(r *guestAddressReport) { r.Schema = 2 }},
		{"other-node", func(r *guestAddressReport) { r.Name = "another-vm" }},
		{"other-mac", func(r *guestAddressReport) { r.MAC = "02:11:22:33:44:55" }},
		{"malformed-mac", func(r *guestAddressReport) { r.MAC = "bad" }},
		{"old-boot", func(r *guestAddressReport) { r.Generation = "old" }},
		{"missing-generation", func(r *guestAddressReport) { r.Generation = "" }},
		{"ipv6", func(r *guestAddressReport) { r.IPv4 = "::1" }},
		{"mapped-ipv6", func(r *guestAddressReport) { r.IPv4 = "::ffff:192.168.120.2" }},
		{"outside-subnet", func(r *guestAddressReport) { r.IPv4 = "127.0.0.1" }},
		{"network", func(r *guestAddressReport) { r.IPv4 = r.IPv4[:strings.LastIndex(r.IPv4, ".")] + ".0" }},
		{"gateway", func(r *guestAddressReport) { r.IPv4 = r.IPv4[:strings.LastIndex(r.IPv4, ".")] + ".1" }},
		{"outside-dhcp-pool", func(r *guestAddressReport) { r.IPv4 = r.IPv4[:strings.LastIndex(r.IPv4, ".")] + ".201" }},
		{"broadcast", func(r *guestAddressReport) { r.IPv4 = r.IPv4[:strings.LastIndex(r.IPv4, ".")] + ".255" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, state, report := addressTestClient(t)
			tc.change(&report)
			writeAddressReport(t, c, state, report)
			got, err := c.nodeAddress(state)
			if tc.name == "valid" {
				if err != nil || got != report.IPv4 {
					t.Fatalf("address=%q err=%v", got, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s report at %q", tc.name, got)
			}
		})
	}
}

func TestKrunkitGuestAddressFileSafety(t *testing.T) {
	for _, kind := range []string{"malformed", "trailing", "unknown-field", "oversize", "symlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			c, state, report := addressTestClient(t)
			filename := writeAddressReport(t, c, state, report)
			switch kind {
			case "malformed":
				_ = os.WriteFile(filename, []byte("{"), 0o600)
			case "trailing":
				f, _ := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0)
				_, _ = f.WriteString("{}\n")
				_ = f.Close()
			case "unknown-field":
				raw, _ := json.Marshal(report)
				raw = append(raw[:len(raw)-1], []byte(",\"unexpected\":true}")...)
				_ = os.WriteFile(filename, raw, 0o600)
			case "oversize":
				_ = os.WriteFile(filename, []byte(strings.Repeat(" ", maxGuestAddressReport+1)), 0o600)
			case "symlink":
				victim := filepath.Join(t.TempDir(), "outside")
				_ = os.Rename(filename, victim)
				if err := os.Symlink(victim, filename); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				_ = os.Remove(filename)
				if err := syscall.Mkfifo(filename, 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				_ = os.Remove(filename)
				if err := os.Mkdir(filename, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(t.TempDir(), "ssh-called")
			c.SSHBin = writeExecutable(t, t.TempDir(), "ssh", "#!/bin/sh\ntouch '"+marker+"'\n")
			// Neither a cached IP nor valid ARP output may bypass a rejected report.
			state.IP = report.IPv4
			if err := c.writeState(state); err != nil {
				t.Fatal(err)
			}
			c.ARPBin = writeExecutable(t, t.TempDir(), "arp", fmt.Sprintf("#!/bin/sh\necho '? (%s) at %s on bridge100'\n", report.IPv4, state.MAC))
			started := time.Now()
			if _, err := c.ExecTimeout(state.Name, time.Second, "true"); err == nil {
				t.Fatal("accepted invalid report")
			}
			if time.Since(started) > time.Second {
				t.Fatal("invalid report blocked readiness")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("invalid report reached SSH: %v", err)
			}
		})
	}
}

func TestKrunkitGuestAddressPrecedesARPAndPersists(t *testing.T) {
	c, state, report := addressTestClient(t)
	writeAddressReport(t, c, state, report)
	arpMarker := filepath.Join(t.TempDir(), "arp-called")
	c.ARPBin = writeExecutable(t, t.TempDir(), "arp", "#!/bin/sh\ntouch '"+arpMarker+"'\nexit 1\n")
	if _, err := c.ExecTimeout(state.Name, time.Second, "true"); err != nil {
		t.Fatal(err)
	}
	latest, err := c.State(state.Name)
	if err != nil || latest.IP != report.IPv4 {
		t.Fatalf("saved IP=%q err=%v", latest.IP, err)
	}
	infos, err := c.List(state.Name)
	if err != nil || len(infos) != 1 || infos[0].IP != report.IPv4 {
		t.Fatalf("inventory=%+v err=%v", infos, err)
	}
	if _, err := os.Stat(arpMarker); !os.IsNotExist(err) {
		t.Fatalf("report path ran ARP: %v", err)
	}
	// A new DHCP address replaces the old saved address through the same path.
	report.IPv4 = report.IPv4[:strings.LastIndex(report.IPv4, ".")] + ".8"
	writeAddressReport(t, c, state, report)
	if _, err := c.ExecTimeout(state.Name, time.Second, "true"); err != nil {
		t.Fatal(err)
	}
	latest, _ = c.State(state.Name)
	if latest.IP != report.IPv4 {
		t.Fatalf("changed DHCP address not saved: %s", latest.IP)
	}
}

func TestKrunkitGuestAddressBootGenerationAndShareSafety(t *testing.T) {
	c, state, report := addressTestClient(t)
	filename := writeAddressReport(t, c, state, report)
	// Starting an already-owned running VM must preserve its current report.
	if err := c.startState(&state); err != nil {
		t.Fatal(err)
	}
	if state.AddressGeneration != report.Generation {
		t.Fatal("running VM generation changed")
	}
	if _, err := os.Stat(filename); err != nil {
		t.Fatal("running VM report was removed")
	}
	// The stopped-start preparation clears the old record and rotates the nonce.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, _ := c.guestAddressDir(state.Name)
	generationFile := filepath.Join(dir, "generation.json")
	_ = os.Remove(generationFile)
	if err := os.Symlink(victim, generationFile); err != nil {
		t.Fatal(err)
	}
	if err := c.prepareGuestAddressReport(&state); err != nil {
		t.Fatal(err)
	}
	if state.AddressGeneration == report.Generation || state.IP != "" {
		t.Fatal("boot state not reset")
	}
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		t.Fatal("stale report survived boot preparation")
	}
	raw, _ := os.ReadFile(victim)
	if string(raw) != "untouched" {
		t.Fatal("followed guest generation symlink")
	}
	writeAddressReport(t, c, state, report)
	if _, err := c.nodeAddress(state); err == nil {
		t.Fatal("accepted previous boot report")
	}
	// A guest-status directory symlink must never export another host directory.
	_ = os.RemoveAll(dir)
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if err := c.prepareGuestAddressReport(&state); err == nil {
		t.Fatal("accepted status directory symlink")
	}
}

func TestKrunkitAddressFallbackAndRemoteIPValidation(t *testing.T) {
	c, state, report := addressTestClient(t)
	state.GuestAddressReport = false
	state.IP = report.IPv4
	c.ARPBin = writeExecutable(t, t.TempDir(), "arp", "#!/bin/sh\necho '? (127.0.0.1) at "+state.MAC+" on bridge100'\n")
	got, err := c.nodeAddress(state)
	if err != nil || got != report.IPv4 {
		t.Fatalf("legacy stored IP=%q err=%v", got, err)
	}
	state.IP = "127.0.0.1"
	if _, err := c.nodeAddress(state); err == nil || !strings.Contains(err.Error(), "recreate") {
		t.Fatalf("legacy discovery error=%v", err)
	}
	state.GuestAddressReport = true
	writeAddressReport(t, c, state, report)
	if err := c.writeState(state); err != nil {
		t.Fatal(err)
	}
	c.SSHBin = writeExecutable(t, t.TempDir(), "ssh", "#!/bin/sh\necho 127.0.0.1\n")
	if _, err := c.IP(state.Name); err == nil {
		t.Fatal("accepted remote route outside owned network")
	}
	latest, _ := c.State(state.Name)
	if latest.IP != report.IPv4 {
		t.Fatalf("persisted invalid remote IP %q", latest.IP)
	}
}

func TestKrunkitGuestAddressMountTargetsRejectedBeforeCreation(t *testing.T) {
	source := t.TempDir()
	image := filepath.Join(t.TempDir(), "disk.img")
	_ = os.WriteFile(image, []byte("image"), 0o600)
	for _, target := range []string{"/", "/run", "/run/kiac", "/run/kiac/status", "/run/kiac/status/child", "/var/run", "/var/run/kiac", "/var/run/kiac/status", "/var/run/kiac/status/child", "/etc/systemd", "/etc/systemd/system/kiac-network-report.service.d", "/etc/systemd/system/kiac-network-report.timer.d", "/etc/systemd/system/run-kiac-status.mount.d", "/usr/local", "/usr/local/libexec/kiac-network-report"} {
		t.Run(target, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			c := &KrunkitClient{RootDir: root}
			err := c.RunDetached(RunOpts{Name: "test-vm", Image: image, Mounts: []Mount{{Source: source, Target: target}}})
			if err == nil || !strings.Contains(err.Error(), "guest address reporting") {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid mount created state: %v", err)
			}
		})
	}
	if err := validateGuestAddressMounts([]Mount{{Target: "/work"}, {Target: "/run/work"}}); err != nil {
		t.Fatal(err)
	}
}

func TestKrunkitGuestReporterPublishesAtomicDHCPChanges(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is not installed on this host")
	}
	c, state, report := addressTestClient(t)
	share, _ := c.guestAddressDir(state.Name)
	macFile := filepath.Join(t.TempDir(), "mac")
	_ = os.WriteFile(macFile, []byte(state.MAC+"\n"), 0o600)
	script := strings.ReplaceAll(guestAddressReporter(state), guestAddressMountPath, share)
	script = strings.ReplaceAll(script, "/sys/class/net/eth0/address", macFile)
	scriptFile := filepath.Join(t.TempDir(), "report.py")
	_ = os.WriteFile(scriptFile, []byte(script), 0o600)
	bin := t.TempDir()
	run := func(ip string) {
		t.Helper()
		writeExecutable(t, bin, "ip", fmt.Sprintf("#!/bin/sh\necho '[{\"addr_info\":[{\"local\":\"%s\",\"scope\":\"global\"}]}]'\n", ip))
		cmd := exec.Command(python, scriptFile)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("reporter: %v %s", err, raw)
		}
	}
	run(report.IPv4)
	if got, err := c.nodeAddress(state); err != nil || got != report.IPv4 {
		t.Fatalf("report=%q err=%v", got, err)
	}
	if err := c.prepareGuestAddressReport(&state); err != nil {
		t.Fatal(err)
	}
	report.IPv4 = report.IPv4[:strings.LastIndex(report.IPv4, ".")] + ".11"
	run(report.IPv4)
	if got, err := c.nodeAddress(state); err != nil || got != report.IPv4 {
		t.Fatalf("new boot/DHCP report=%q err=%v", got, err)
	}
	entries, _ := os.ReadDir(share)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".address-") {
			t.Fatal("left partial reporter file")
		}
	}
}

func TestKrunkitCloudInitInstallsPersistentGuestReporter(t *testing.T) {
	c, state, _ := addressTestClient(t)
	c.HDIUtilBin = writeExecutable(t, t.TempDir(), "hdiutil", "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then shift; touch \"$1\"; exit; fi; shift; done\n")
	nodeDir, _ := c.nodeDir(state.Name)
	state.Seed = filepath.Join(nodeDir, "cidata.iso")
	if err := c.createCloudInitISO(nodeDir, state, "ssh-ed25519 TEST", nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(nodeDir, "cloud-init", "user-data"))
	if err != nil {
		t.Fatal(err)
	}
	var config cloudInitConfig
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, file := range config.WriteFiles {
		files[file.Path] = file.Content
	}
	for file, want := range map[string]string{
		"/etc/systemd/system/run-kiac-status.mount":       "What=kiac-status",
		"/etc/systemd/system/kiac-network-report.service": "Requires=run-kiac-status.mount",
		"/etc/systemd/system/kiac-network-report.timer":   "OnUnitActiveSec=3s",
		"/usr/local/libexec/kiac-network-report":          "os.replace(filename, share + \"/address.json\")",
	} {
		if !strings.Contains(files[file], want) {
			t.Fatalf("missing persistent reporter component %s", file)
		}
	}
	found := false
	for _, command := range config.RunCmd {
		if strings.Join(command, " ") == "systemctl enable --now run-kiac-status.mount kiac-network-report.timer" {
			found = true
		}
	}
	if !found {
		t.Fatal("reporter is not enabled on every boot")
	}
}

func TestKrunkitLegacyRestartKeepsExistingMounts(t *testing.T) {
	c, state, report := addressTestClient(t)
	state.GuestAddressReport = false
	state.AddressGeneration = ""
	state.PID = 0
	state.ProcessStart = ""
	state.ProcessCommandHash = ""
	state.Status = "stopped"
	nodeDir, _ := c.nodeDir(state.Name)
	state.Disk = filepath.Join(nodeDir, "disk.img")
	state.Seed = filepath.Join(nodeDir, "cidata.iso")
	for _, filename := range []string{state.Disk, state.Seed} {
		if err := os.WriteFile(filename, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state.Mounts = []Mount{{Source: t.TempDir(), Target: "/run"}}
	argsFile := filepath.Join(t.TempDir(), "args")
	c.Bin = writeExecutable(t, t.TempDir(), "krunkit", "#!/bin/sh\nexit 0\n")
	c.VMNetRunBin = writeExecutable(t, t.TempDir(), "vmnet-run", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+argsFile+"'\nsleep 30\n")
	c.StopTimeout = 10 * time.Millisecond
	c.KillTimeout = 10 * time.Millisecond
	if err := c.writeState(state); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Remove(state.Name) })
	if err := c.Start(state.Name); err != nil {
		t.Fatalf("legacy mount rejected: %v", err)
	}
	var raw []byte
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(argsFile)
		if err == nil && strings.Contains(string(raw), "mountTag=kiac0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mountTag="+guestAddressMountTag) {
		t.Fatal("legacy VM acquired an unsupported reporter mount")
	}
	if !strings.Contains(string(raw), "mountTag=kiac0") {
		t.Fatal("legacy user mount was omitted")
	}
	state, err = c.State(state.Name)
	if err != nil {
		t.Fatal(err)
	}
	state.IP = report.IPv4
	c.ARPBin = writeExecutable(t, t.TempDir(), "arp", "#!/bin/sh\nexit 0\n")
	if got, err := c.nodeAddress(state); err != nil || got != report.IPv4 {
		t.Fatalf("legacy saved address=%q err=%v", got, err)
	}
}
