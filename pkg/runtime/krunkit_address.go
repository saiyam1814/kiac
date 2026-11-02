package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	guestAddressDirectory  = "guest-status"
	guestAddressMountTag   = "kiac-status"
	guestAddressMountPath  = "/run/kiac/status"
	guestAddressReportFile = "address.json"
	maxGuestAddressReport  = 4096
)

type guestAddressReport struct {
	Schema     int    `json:"schema"`
	Name       string `json:"name"`
	MAC        string `json:"mac"`
	Generation string `json:"generation"`
	IPv4       string `json:"ipv4"`
}

type legacyAddressDiscoveryError struct{ name string }

func (e *legacyAddressDiscoveryError) Error() string {
	return fmt.Sprintf("cannot discover the address of legacy krunkit VM %s: no matching ARP entry or saved IP; macOS 27 can restrict ARP discovery in app contexts; recreate this cluster with the current Kiac version to enable guest-reported addresses", e.name)
}

// New VMs report their own address. Do not let a missing or invalid report fall
// back to a stale address from an earlier boot. Old disks retain their original
// ARP/saved-IP path because their cloud-init has already run.
func (c *KrunkitClient) nodeAddress(state KrunkitNodeState) (string, error) {
	if state.GuestAddressReport {
		ip, err := c.readGuestAddressReport(state)
		if err != nil {
			return "", fmt.Errorf("waiting for a valid guest address report from %s: %w", state.Name, err)
		}
		return ip, nil
	}
	for _, ip := range []string{c.findARPAddress(state.MAC), state.IP} {
		if ip != "" && c.validateNodeIPv4(state, ip) == nil {
			return ip, nil
		}
	}
	return "", &legacyAddressDiscoveryError{name: state.Name}
}

func (c *KrunkitClient) guestAddressDir(name string) (string, error) {
	dir, err := c.nodeDir(name)
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, guestAddressDirectory)
	if strings.ContainsAny(dir, ",\r\n") {
		return "", fmt.Errorf("guest status directory cannot contain commas or newlines")
	}
	return dir, nil
}

func checkGuestAddressDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("guest status path is not a regular directory")
	}
	return nil
}

// Called only after process ownership confirms this VM is stopped. Atomic
// replacement never follows a generation-file symlink left by a guest.
func (c *KrunkitClient) prepareGuestAddressReport(state *KrunkitNodeState) error {
	dir, err := c.guestAddressDir(state.Name)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := checkGuestAddressDir(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	generation, err := randomID()
	if err != nil {
		return err
	}
	// Removing a symlink removes the link itself, never its target.
	if err := os.Remove(filepath.Join(dir, guestAddressReportFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(dir, "generation.json"), generation, 0o600); err != nil {
		return err
	}
	state.AddressGeneration = generation
	state.IP = ""
	return nil
}

func readGuestReportFile(file string) ([]byte, error) {
	// NOFOLLOW closes the final-component symlink race. NONBLOCK prevents a FIFO
	// swapped in before open from hanging a readiness probe before fstat.
	fd, err := syscall.Open(file, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxGuestAddressReport {
		return nil, fmt.Errorf("guest address report must be a regular file of at most %d bytes", maxGuestAddressReport)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxGuestAddressReport+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxGuestAddressReport {
		return nil, fmt.Errorf("guest address report exceeds %d bytes", maxGuestAddressReport)
	}
	return raw, nil
}

func (c *KrunkitClient) readGuestAddressReport(state KrunkitNodeState) (string, error) {
	dir, err := c.guestAddressDir(state.Name)
	if err != nil {
		return "", err
	}
	if err := checkGuestAddressDir(dir); err != nil {
		return "", err
	}
	raw, err := readGuestReportFile(filepath.Join(dir, guestAddressReportFile))
	if err != nil {
		return "", err
	}
	var report guestAddressReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return "", fmt.Errorf("invalid guest address JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("guest address report must contain exactly one JSON object")
	}
	mac, macErr := net.ParseMAC(report.MAC)
	if report.Schema != 1 || report.Name != state.Name || macErr != nil || len(mac) != 6 || normalizeMAC(report.MAC) != normalizeMAC(state.MAC) || state.AddressGeneration == "" || report.Generation != state.AddressGeneration {
		return "", fmt.Errorf("guest address report identity or boot generation does not match this VM")
	}
	if err := c.validateNodeIPv4(state, report.IPv4); err != nil {
		return "", err
	}
	return report.IPv4, nil
}

func (c *KrunkitClient) validateNodeIPv4(state KrunkitNodeState, value string) error {
	ip, err := netip.ParseAddr(value)
	if err != nil || !ip.Is4() {
		return fmt.Errorf("reported address is not IPv4")
	}
	network, err := c.loadNetwork(state.NetworkID)
	if err != nil {
		return err
	}
	if state.NetworkCIDR != network.CIDR {
		return fmt.Errorf("recorded VM subnet does not match its owned network")
	}
	prefix, err := netip.ParsePrefix(network.CIDR)
	if err != nil || !prefix.Contains(ip) {
		return fmt.Errorf("reported IPv4 is outside this VM's subnet")
	}
	// vmnet's recorded DHCP range is .1 through .200; .1 is the host gateway.
	// Network, broadcast, and addresses outside that pool are never guest IPs.
	octets := ip.As4()
	if octets[3] < 2 || octets[3] > 200 {
		return fmt.Errorf("reported IPv4 is not in this VM's usable DHCP pool")
	}
	return nil
}

func validateGuestAddressMounts(mounts []Mount) error {
	protected := []string{guestAddressMountPath, "/usr/local/libexec/kiac-network-report", "/etc/systemd/system/run-kiac-status.mount", "/etc/systemd/system/kiac-network-report.service", "/etc/systemd/system/kiac-network-report.timer"}
	for _, unit := range append([]string(nil), protected[2:]...) {
		protected = append(protected, unit+".d")
	}
	for _, mount := range mounts {
		target := path.Clean(mount.Target)
		// Fedora's /var/run is a symlink to /run.
		if target == "/var/run" || strings.HasPrefix(target, "/var/run/") {
			target = "/run" + strings.TrimPrefix(target, "/var/run")
		}
		for _, reserved := range protected {
			if target == reserved || strings.HasPrefix(target, reserved+"/") || target == "/" || strings.HasPrefix(reserved, target+"/") {
				return fmt.Errorf("mount target %q overlaps Kiac's guest address reporting files", mount.Target)
			}
		}
	}
	return nil
}
