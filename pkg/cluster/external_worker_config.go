package cluster

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const externalWireGuardInterface = "kiacwg0"

// ExternalWorkerConfig deliberately describes only the supported alpha:
// an IPv4 K3s/Flannel VXLAN agent reached over an operator-managed WireGuard peer.
type ExternalWorkerConfig struct {
	Name, Server, TokenFile, WireGuardFile, NodeIP, K8sVersion string
	PodCIDR, ServiceCIDR                                       string
	CPUs, Memory, DiskSize                                     string
	WaitTimeout                                                time.Duration
}

type workerWireGuard struct {
	Address, PrivateKey, PublicKey, PresharedKey, Endpoint string
	AllowedIPs                                             []netip.Prefix
	MTU, Keepalive                                         int
}

type preparedExternalWorker struct {
	config ExternalWorkerConfig
	token  string
	caHash string
	wg     workerWireGuard
}

var externalWorkerNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

func prepareExternalWorker(cfg ExternalWorkerConfig) (preparedExternalWorker, error) {
	p := preparedExternalWorker{config: cfg}
	if !externalWorkerNameRE.MatchString(cfg.Name) {
		return p, fmt.Errorf("worker name must be 1..32 lowercase letters, digits or dashes, starting and ending with a letter or digit")
	}
	u, err := url.Parse(cfg.Server)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return p, fmt.Errorf("--server must be an https://IPv4:PORT endpoint without credentials or a path")
	}
	serverIP, err := netip.ParseAddr(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if err != nil || !serverIP.Is4() || !serverIP.IsGlobalUnicast() || portErr != nil || port < 1 || port > 65535 {
		return p, fmt.Errorf("--server must contain a unicast IPv4 address and explicit port")
	}
	p.config.Server = strings.TrimSuffix(u.String(), "/")
	nodeIP, err := netip.ParseAddr(cfg.NodeIP)
	if err != nil || !nodeIP.Is4() || !nodeIP.IsGlobalUnicast() || nodeIP == serverIP {
		return p, fmt.Errorf("--node-ip must be a distinct unicast IPv4 WireGuard address")
	}
	if cfg.K8sVersion == "" {
		return p, fmt.Errorf("--k8s-version is required and must match the external K3s server release")
	}
	version, err := ResolveGPUVersion("k3s", cfg.K8sVersion, "device-plugin")
	if err != nil {
		return p, fmt.Errorf("external worker version: %w", err)
	}
	p.config.K8sVersion = normalizeK3sStateVersion(version)
	if p.config.CPUs == "" {
		p.config.CPUs = "2"
	}
	cpus, err := strconv.Atoi(p.config.CPUs)
	if err != nil || cpus < 1 || cpus > 32 {
		return p, fmt.Errorf("worker --cpus must be between 1 and 32")
	}
	if p.config.Memory == "" {
		p.config.Memory = "2G"
	}
	if p.config.DiskSize == "" {
		p.config.DiskSize = "20G"
	}
	for _, size := range []struct {
		name, value      string
		minimum, maximum int64
	}{
		{"memory", p.config.Memory, 1 << 30, 60 << 30}, {"disk", p.config.DiskSize, 8 << 30, 1 << 40},
	} {
		bytes, err := externalWorkerSize(size.value)
		if err != nil || bytes < size.minimum || bytes > size.maximum || (size.name == "memory" && bytes%(1<<20) != 0) {
			return p, fmt.Errorf("worker --%s must be between %dG and %dG (for example 2G or 2048M)", size.name, size.minimum>>30, size.maximum>>30)
		}
	}
	if p.config.WaitTimeout <= 0 {
		p.config.WaitTimeout = 10 * time.Minute
	}
	if p.config.WaitTimeout > time.Hour {
		return p, fmt.Errorf("worker --wait must not exceed one hour")
	}
	pod, err := workerPrefix(cfg.PodCIDR)
	if err != nil {
		return p, fmt.Errorf("--pod-cidr must be the external cluster's canonical IPv4 pod CIDR")
	}
	service, err := workerPrefix(cfg.ServiceCIDR)
	if err != nil || pod.Overlaps(service) || pod.Contains(nodeIP) || service.Contains(nodeIP) || pod.Contains(serverIP) || service.Contains(serverIP) {
		return p, fmt.Errorf("pod, service and advertised node/API address ranges must not overlap")
	}
	raw, err := readWorkerSecret(cfg.TokenFile, 4096)
	if err != nil {
		return p, fmt.Errorf("reading --token-file: %w", err)
	}
	p.token = strings.TrimSpace(string(raw))
	if len(p.token) < 70 || !strings.HasPrefix(p.token, "K10") || p.token[67:69] != "::" || strings.ContainsAny(p.token, "\r\n\x00\t ") {
		return p, fmt.Errorf("--token-file must contain a full CA-pinned K3s K10 secure token; short tokens are not accepted")
	}
	if _, err := hex.DecodeString(p.token[3:67]); err != nil {
		return p, fmt.Errorf("K3s secure token must contain a 64-character hexadecimal CA hash")
	}
	creds := p.token[69:]
	if !regexp.MustCompile(`^(?:(?:server|node):[^:]+|[a-z0-9]{6}\.[a-z0-9]{16})$`).MatchString(creds) {
		return p, fmt.Errorf("secure token credentials must be a K3s server/agent credential or bootstrap token")
	}
	p.caHash = strings.ToLower(p.token[3:67])
	raw, err = readWorkerSecret(cfg.WireGuardFile, 64*1024)
	if err != nil {
		return p, fmt.Errorf("reading --wireguard-config: %w", err)
	}
	p.wg, err = parseWorkerWireGuard(string(raw))
	if err != nil {
		return p, err
	}
	address, _ := netip.ParsePrefix(p.wg.Address)
	if address.Addr() != nodeIP {
		return p, fmt.Errorf("WireGuard Address must equal --node-ip with a /32 prefix")
	}
	for _, prefix := range []netip.Prefix{netip.PrefixFrom(serverIP, 32), pod, service} {
		if !workerRouteCovered(prefix, p.wg.AllowedIPs) {
			return p, fmt.Errorf("WireGuard AllowedIPs must cover the API address and the declared pod/service CIDRs")
		}
	}
	return p, nil
}

func externalWorkerSize(value string) (int64, error) {
	parts := regexp.MustCompile(`(?i)^([1-9][0-9]*)([KMGT])(?:I?B)?$`).FindStringSubmatch(value)
	if parts == nil {
		return 0, fmt.Errorf("invalid size")
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	multiplier := map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}[strings.ToUpper(parts[2])]
	if err != nil || n > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("invalid size")
	}
	return n * multiplier, nil
}

func workerPrefix(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() || p.Bits() < 1 || p != p.Masked() || !p.Addr().IsGlobalUnicast() {
		return netip.Prefix{}, fmt.Errorf("invalid canonical IPv4 prefix")
	}
	return p, nil
}

func workerRouteCovered(want netip.Prefix, routes []netip.Prefix) bool {
	for _, route := range routes {
		if route.Bits() <= want.Bits() && route.Contains(want.Addr()) {
			return true
		}
	}
	return false
}

func readWorkerSecret(path string, limit int64) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("a private file is required")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > limit {
		return nil, fmt.Errorf("must be a regular owner-private file (chmod 600), at most %d bytes", limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, fmt.Errorf("cannot read a bounded private file")
	}
	return b, nil
}

func parseWorkerWireGuard(raw string) (workerWireGuard, error) {
	w := workerWireGuard{MTU: 1420, Keepalive: 25}
	values := map[string]string{}
	section := ""
	sections := map[string]bool{}
	s := bufio.NewScanner(strings.NewReader(raw))
	for s.Scan() {
		line := strings.TrimSpace(strings.SplitN(s.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if (line != "[Interface]" && line != "[Peer]") || sections[line] {
				return w, fmt.Errorf("WireGuard config requires exactly one Interface and one Peer")
			}
			section, sections[line] = line, true
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		allowed := (section == "[Interface]" && (key == "PrivateKey" || key == "Address" || key == "MTU")) ||
			(section == "[Peer]" && (key == "PublicKey" || key == "PresharedKey" || key == "Endpoint" || key == "AllowedIPs" || key == "PersistentKeepalive"))
		fullKey := section + key
		if !ok || !allowed || value == "" || values[fullKey] != "" {
			return w, fmt.Errorf("unsupported, empty or duplicate WireGuard field; shell hooks and full-tunnel configs are not supported")
		}
		values[fullKey] = value
	}
	if err := s.Err(); err != nil {
		return w, fmt.Errorf("invalid WireGuard configuration")
	}
	w.Address, w.PrivateKey = values["[Interface]Address"], values["[Interface]PrivateKey"]
	w.PublicKey, w.PresharedKey, w.Endpoint = values["[Peer]PublicKey"], values["[Peer]PresharedKey"], values["[Peer]Endpoint"]
	a, err := netip.ParsePrefix(w.Address)
	if err != nil || !a.Addr().Is4() || a.Bits() != 32 || !a.Addr().IsGlobalUnicast() {
		return w, fmt.Errorf("WireGuard Address must be one unicast IPv4 /32 address")
	}
	for _, item := range []struct{ name, key string }{{"PrivateKey", w.PrivateKey}, {"PublicKey", w.PublicKey}, {"PresharedKey", w.PresharedKey}} {
		if item.name == "PresharedKey" && item.key == "" {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(item.key)
		if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != item.key {
			return w, fmt.Errorf("WireGuard %s must be a base64-encoded 32-byte key", item.name)
		}
	}
	host, port, err := net.SplitHostPort(w.Endpoint)
	ip, ipErr := netip.ParseAddr(host)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || ipErr != nil || !ip.Is4() || !ip.IsGlobalUnicast() || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return w, fmt.Errorf("WireGuard Endpoint must be a unicast IPv4:PORT outside the tunnel")
	}
	for _, entry := range strings.Split(values["[Peer]AllowedIPs"], ",") {
		p, err := workerPrefix(strings.TrimSpace(entry))
		if err != nil || p.Contains(ip) {
			return w, fmt.Errorf("WireGuard AllowedIPs must be explicit IPv4 routes that do not capture its Endpoint")
		}
		for _, old := range w.AllowedIPs {
			if old == p {
				return w, fmt.Errorf("duplicate WireGuard AllowedIPs route")
			}
		}
		w.AllowedIPs = append(w.AllowedIPs, p)
	}
	for _, item := range []struct {
		key      string
		dst      *int
		min, max int
	}{{"[Interface]MTU", &w.MTU, 1280, 1420}, {"[Peer]PersistentKeepalive", &w.Keepalive, 1, 120}} {
		if value := values[item.key]; value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < item.min || n > item.max {
				return w, fmt.Errorf("WireGuard %s is outside the supported range", strings.TrimPrefix(item.key, "[Peer]"))
			}
			*item.dst = n
		}
	}
	return w, nil
}
