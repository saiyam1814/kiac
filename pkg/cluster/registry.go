package cluster

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// RegistryCAs maps an HTTPS registry authority (host[:port]) to a PEM CA bundle.
type RegistryCAs map[string]string

func (r *RegistryCAs) Set(value string) error {
	host, path, ok := strings.Cut(value, "=")
	if !ok || path == "" {
		return fmt.Errorf("registry CA must be HOST[:PORT]=PEM_FILE")
	}
	if err := validateRegistryHost(host); err != nil {
		return err
	}
	if *r == nil {
		*r = RegistryCAs{}
	}
	if _, exists := (*r)[host]; exists {
		return fmt.Errorf("duplicate registry CA for %q", host)
	}
	(*r)[host] = path
	return nil
}

func (r *RegistryCAs) String() string {
	var pairs []string
	for host, path := range *r {
		pairs = append(pairs, host+"="+path)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (*RegistryCAs) Type() string { return "registry-ca" }

var registryDNS = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func validateRegistryHost(host string) error {
	u, err := url.Parse("https://" + host)
	if err != nil || host == "" || strings.TrimSpace(host) != host || strings.ToLower(host) != host ||
		u.User != nil || u.Host != host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(host, "\\%") {
		return fmt.Errorf("invalid registry %q: use a lowercase host or IP with optional port, without a URL scheme or path", host)
	}
	name := u.Hostname()
	if net.ParseIP(name) == nil {
		if !registryDNS.MatchString(name) || strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
			return fmt.Errorf("invalid registry host %q", host)
		}
		for _, label := range strings.Split(name, ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return fmt.Errorf("invalid registry host %q", host)
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid registry port in %q", host)
		}
	} else if strings.HasSuffix(host, ":") {
		return fmt.Errorf("empty registry port in %q", host)
	}
	return nil
}

type registryCA struct {
	host, path string
	pem        []byte
}

// PrepareRegistryCAs validates and snapshots local files before any provisioning.
// The snapshot is reused by all nodes, so files cannot change between uploads.
func (cfg *Config) PrepareRegistryCAs() error {
	if len(cfg.RegistryCAs) > 0 {
		if cfg.Distro == "k3s" {
			for _, mount := range cfg.Mounts {
				target := path.Clean(mount.Target)
				const configDir = "/etc/rancher/k3s"
				if target == "/" || target == configDir || strings.HasPrefix(target, configDir+"/") || strings.HasPrefix(configDir, target+"/") {
					return fmt.Errorf("--registry-ca cannot be combined with a K3s mount at %q; Kiac must manage /etc/rancher/k3s and its registry configuration", mount.Target)
				}
			}
		}
		for _, args := range [][]string{cfg.K3sControlPlaneArgs, cfg.K3sWorkerArgs} {
			for _, arg := range args {
				name, _, _ := strings.Cut(strings.TrimSpace(arg), "=")
				if !strings.HasPrefix(name, "-") {
					continue
				}
				switch strings.TrimLeft(name, "-") {
				case "private-registry", "container-runtime-endpoint", "image-service-endpoint", "docker", "config", "c":
					return fmt.Errorf("--registry-ca cannot be combined with K3s %s; Kiac must manage the registry configuration and built-in runtime", name)
				}
			}
		}
	}
	if cfg.registryCAs != nil || len(cfg.RegistryCAs) == 0 {
		return nil
	}
	var hosts []string
	for host := range cfg.RegistryCAs {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	prepared := make([]registryCA, 0, len(hosts))
	for _, host := range hosts {
		if err := validateRegistryHost(host); err != nil {
			return err
		}
		info, err := os.Stat(cfg.RegistryCAs[host])
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("registry CA for %s must be a readable regular PEM file", host)
		}
		f, err := os.Open(cfg.RegistryCAs[host])
		if err != nil {
			return fmt.Errorf("registry CA for %s: %w", host, err)
		}
		raw, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
		_ = f.Close()
		if err != nil || len(raw) > 1024*1024 {
			return fmt.Errorf("registry CA for %s: cannot read PEM bundle (maximum 1 MiB)", host)
		}
		rest, count := bytes.TrimSpace(raw), 0
		for len(rest) > 0 {
			if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
				return fmt.Errorf("registry CA for %s must contain only PEM CA certificates", host)
			}
			block, remaining := pem.Decode(rest)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return fmt.Errorf("registry CA for %s contains invalid PEM", host)
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !cert.IsCA || !cert.BasicConstraintsValid || (cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0) {
				return fmt.Errorf("registry CA for %s contains a non-CA certificate", host)
			}
			count++
			rest = bytes.TrimSpace(remaining)
		}
		if count == 0 {
			return fmt.Errorf("registry CA for %s is empty", host)
		}
		path := fmt.Sprintf("/etc/kiac/registry-cas/%x.crt", sha256.Sum256([]byte(host)))
		prepared = append(prepared, registryCA{host: host, path: path, pem: raw})
	}
	cfg.registryCAs = prepared
	return nil
}

const registryReadyPath = "/etc/rancher/k3s/.kiac-registry-ready"

// K3s automatically reads its default config and YAML drop-ins even when no
// config flag was supplied. They can redirect registry/runtime settings away
// from the files below, so reject them before installing any trust or opening
// the first-boot gate. Include dangling symlinks instead of ignoring them.
const k3sRegistryConfigPreconditions = `for config in /etc/rancher/k3s/config.yaml /etc/rancher/k3s/config.yaml.d/*.yaml; do
  if [ -e "$config" ] || [ -L "$config" ]; then
    echo 'existing K3s config.yaml or YAML drop-in conflicts with --registry-ca; Kiac must manage the registry configuration and built-in runtime' >&2
    exit 1
  fi
done
`

// Runtime Exec combines stdout/stderr. Keep successful migration warnings out
// of the TOML document while retaining stderr when config loading fails.
const containerdRegistryDumpScript = `log=$(mktemp /tmp/kiac-containerd.XXXXXX)
trap 'rm -f "$log"' EXIT
if containerd config dump 2>"$log"; then :; else cat "$log" >&2; exit 1; fi`

// K3s must see registries.yaml on its first start, including the ordinary
// image's entrypoint. The durable marker also works after stop/start or resume.
func registryBootGate(cfg Config) string {
	if len(cfg.RegistryCAs) == 0 {
		return ""
	}
	seconds := int((transferBudget(cfg.WaitTimeout) + 15*time.Second).Seconds())
	return fmt.Sprintf(`KIAC_REGISTRY_I=0; while [ ! -f %s ]; do if [ "$KIAC_REGISTRY_I" -ge %d ]; then echo 'timed out waiting for Kiac registry trust' >&2; exit 1; fi; sleep 1; KIAC_REGISTRY_I=$((KIAC_REGISTRY_I+1)); done; `, registryReadyPath, seconds)
}

func (m *Manager) installRegistryTrust(node, distro string, cfg Config) error {
	if len(cfg.registryCAs) == 0 {
		return nil
	}
	deadline := time.Now().Add(transferBudget(cfg.WaitTimeout))
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("waiting to install registry trust on %s: %w", node, lastErr)
		}
		if _, err := m.rt.ExecTimeout(node, min(5*time.Second, remaining), "true"); err == nil {
			break
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	files := map[string][]byte{}
	var script strings.Builder
	var preconditions strings.Builder
	configs := map[string]any{}
	for _, ca := range cfg.registryCAs {
		files[strings.TrimPrefix(ca.path, "/")] = ca.pem
		fmt.Fprintf(&script, "install -d -m 0755 /etc/kiac/registry-cas\ninstall -m 0644 \"$tmp/%s\" %s\n", strings.TrimPrefix(ca.path, "/"), shQuote(ca.path))
		if distro == "k3s" {
			configs[ca.host] = map[string]any{"tls": map[string]string{"ca_file": ca.path}}
		} else {
			path := "/etc/containerd/certs.d/" + ca.host + "/hosts.toml"
			if i := strings.LastIndex(ca.host, ":"); i > 0 {
				encoded := ca.host[:i] + "_" + ca.host[i+1:] + "_"
				encodedPath := shQuote("/etc/containerd/certs.d/" + encoded)
				fmt.Fprintf(&preconditions, "test ! -e %s && test ! -L %s || { echo 'encoded registry directory already exists; refusing to shadow it' >&2; exit 1; }\n", encodedPath, encodedPath)
			}
			endpoint := "https://" + ca.host
			if ca.host == "docker.io" {
				endpoint = "https://registry-1.docker.io"
			}
			files[strings.TrimPrefix(path, "/")] = []byte(fmt.Sprintf("server = %q\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  ca = %q\n", endpoint, endpoint, ca.path))
			registryPath := shQuote("/etc/containerd/certs.d/" + ca.host)
			fmt.Fprintf(&preconditions, "test ! -e %s && test ! -L %s || { echo 'registry directory already exists; refusing to overwrite' >&2; exit 1; }\n", registryPath, registryPath)
			fmt.Fprintf(&script, "install -d -m 0755 %s\ninstall -m 0644 \"$tmp/%s\" %s\n", shQuote("/etc/containerd/certs.d/"+ca.host), strings.TrimPrefix(path, "/"), shQuote(path))
		}
	}
	if distro == "k3s" {
		files["etc/rancher/k3s/registries.yaml"], _ = yaml.Marshal(map[string]any{"configs": configs})
		preconditions.WriteString(k3sRegistryConfigPreconditions)
		preconditions.WriteString("test ! -e /etc/rancher/k3s/registries.yaml && test ! -L /etc/rancher/k3s/registries.yaml || { echo 'registries.yaml already exists; refusing to overwrite' >&2; exit 1; }\n")
		script.WriteString("install -d -m 0755 /etc/rancher/k3s\nnew=$(mktemp /etc/rancher/k3s/registries.yaml.kiac.XXXXXX)\ninstall -m 0600 \"$tmp/etc/rancher/k3s/registries.yaml\" \"$new\"\nmv -f \"$new\" /etc/rancher/k3s/registries.yaml\n: > " + registryReadyPath + "\n")
	} else {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("installing registry trust on %s: %w", node, context.DeadlineExceeded)
		}
		config, err := m.rt.ExecTimeout(node, min(15*time.Second, remaining), "cat", "/etc/containerd/config.toml")
		if err != nil {
			return fmt.Errorf("read containerd registry configuration on %s: %w", node, err)
		}
		updated, err := containerdRegistryConfig(config)
		if err != nil {
			return fmt.Errorf("registry trust on %s: %w", node, err)
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("installing registry trust on %s: %w", node, context.DeadlineExceeded)
		}
		effective, err := m.rt.ExecTimeout(node, min(15*time.Second, remaining), "sh", "-ec", containerdRegistryDumpScript)
		if err != nil {
			return fmt.Errorf("load effective containerd registry configuration on %s: %w", node, err)
		}
		if _, err := containerdRegistryConfig(effective); err != nil {
			return fmt.Errorf("effective registry trust on %s: %w", node, err)
		}
		if updated != config {
			files["etc/containerd/config.toml"] = []byte(updated)
			script.WriteString("new=$(mktemp /etc/containerd/config.toml.kiac.XXXXXX)\ninstall -m 0600 \"$tmp/etc/containerd/config.toml\" \"$new\"\nmv -f \"$new\" /etc/containerd/config.toml\nsystemctl restart containerd\n")
		}
	}
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	var paths []string
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0600, Size: int64(len(files[path]))}); err != nil {
			return err
		}
		if _, err := tw.Write(files[path]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("installing registry trust on %s: %w", node, context.DeadlineExceeded)
	}
	command := "set -eu\ntmp=$(mktemp -d /tmp/kiac-registry.XXXXXX)\ntrap 'rm -rf \"$tmp\"' EXIT\ntar -xf - -C \"$tmp\"\n" + preconditions.String() + script.String()
	if err := m.rt.ExecStdinTimeout(node, remaining, &payload, "sh", "-ec", command); err != nil {
		return err
	}
	if distro != "k3s" {
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("verifying registry trust on %s: %w", node, context.DeadlineExceeded)
		}
		effective, err := m.rt.ExecTimeout(node, min(15*time.Second, remaining), "sh", "-ec", containerdRegistryDumpScript)
		if err != nil {
			return err
		}
		checked, err := containerdRegistryConfig(effective)
		if err != nil {
			return err
		}
		if checked != effective {
			return fmt.Errorf("containerd on %s did not activate registry trust; check imported configuration", node)
		}
	}
	return nil
}

// Preserve the node image's other settings and refuse a custom registry path.
// Containerd 1.x (config v2) and 2.x (config v3/v4) are supported.
func containerdRegistryConfig(config string) (string, error) {
	var document map[string]any
	if err := toml.Unmarshal([]byte(config), &document); err != nil {
		return "", fmt.Errorf("invalid containerd configuration: %w", err)
	}
	var plugin string
	switch document["version"] {
	case int64(2):
		plugin = "io.containerd.grpc.v1.cri"
	case int64(3), int64(4):
		plugin = "io.containerd.cri.v1.images"
	default:
		return "", fmt.Errorf("registry CA configuration requires containerd config version 2, 3 or 4")
	}
	plugins, _ := document["plugins"].(map[string]any)
	active, ok := plugins[plugin].(map[string]any)
	if !ok {
		return "", fmt.Errorf("containerd config has no active CRI image plugin %s", plugin)
	}
	registry, _ := active["registry"].(map[string]any)
	if registry == nil {
		registry = map[string]any{}
		active["registry"] = registry
	}
	for _, field := range []string{"mirrors", "configs", "auths"} {
		if value, ok := registry[field]; ok {
			if values, ok := value.(map[string]any); !ok || len(values) != 0 {
				return "", fmt.Errorf("containerd has legacy registry %s; use its existing trust configuration", field)
			}
		}
	}
	if path, ok := registry["config_path"]; ok && path != "" {
		if path == "/etc/containerd/certs.d" {
			return config, nil
		}
		return "", fmt.Errorf("containerd has a custom registry config_path; use its existing trust configuration")
	}
	registry["config_path"] = "/etc/containerd/certs.d"
	raw, err := toml.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
