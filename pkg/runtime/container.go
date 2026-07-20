// Package runtime drives Apple's `container` CLI. Every Kubernetes node
// kiac creates is one apple/containerization lightweight VM managed by
// that binary, so this package is the only place that shells out to it.
package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Client wraps the apple/container CLI binary.
type Client struct {
	Bin string

	capAddProbe *bool
}

func New() *Client { return &Client{Bin: "container"} }

// CommandError carries the captured output of a failed CLI invocation so
// callers can surface actionable diagnostics instead of "exit status 1".
type CommandError struct {
	Args   []string
	Output string
	Err    error
}

func (e *CommandError) Error() string {
	out := strings.TrimSpace(e.Output)
	if len(out) > 2000 {
		out = out[len(out)-2000:]
	}
	return fmt.Sprintf("container %s failed: %v\n%s", strings.Join(e.Args, " "), e.Err, out)
}

func (c *Client) run(args ...string) (string, error) {
	cmd := exec.Command(c.Bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), &CommandError{Args: args, Output: string(out), Err: err}
	}
	return string(out), nil
}

// Available reports whether the container binary is on PATH.
func (c *Client) Available() bool {
	_, err := exec.LookPath(c.Bin)
	return err == nil
}

// Version returns the CLI version string, e.g. "1.0.0".
func (c *Client) Version() (string, error) {
	out, err := c.run("--version")
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`(\d+\.\d+\.\d+)`).FindStringSubmatch(out)
	if m == nil {
		return strings.TrimSpace(out), nil
	}
	return m[1], nil
}

// SystemRunning reports whether the container API server responds.
func (c *Client) SystemRunning() bool {
	out, err := c.run("system", "status")
	if err != nil {
		return false
	}
	low := strings.ToLower(out)
	return !strings.Contains(low, "not running")
}

// SystemStart starts the container API server.
func (c *Client) SystemStart() error {
	_, err := c.run("system", "start")
	return err
}

// RunOpts describes one node VM.
type RunOpts struct {
	Name       string
	Image      string
	CPUs       string
	Memory     string
	Env        []string // KEY=VALUE pairs, passed as -e flags
	Entrypoint string   // overrides the image entrypoint when non-empty
	Kernel     string   // custom kernel Image path (--kernel), empty = bundled default
	Args       []string // command arguments appended after the image
}

// RunDetached boots a node VM. The kindest/node entrypoint brings up
// systemd and containerd inside the VM. Nodes get the full capability
// set (container CLI 1.0 tightened the default and the entrypoint needs
// CAP_SYS_ADMIN); the VM boundary is the isolation, not capabilities.
func (c *Client) RunDetached(o RunOpts) error {
	args := []string{"run", "-d", "--name", o.Name}
	if c.supportsCapAdd() {
		args = append(args, "--cap-add", "ALL")
	}
	if o.CPUs != "" {
		args = append(args, "--cpus", o.CPUs)
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	if o.Entrypoint != "" {
		args = append(args, "--entrypoint", o.Entrypoint)
	}
	if o.Kernel != "" {
		args = append(args, "--kernel", o.Kernel)
	}
	args = append(args, o.Image)
	args = append(args, o.Args...)
	_, err := c.run(args...)
	return err
}

// supportsCapAdd probes once whether this container CLI knows --cap-add
// (added in 1.0.0); 0.x grants a wider default set and lacks the flag.
func (c *Client) supportsCapAdd() bool {
	if c.capAddProbe == nil {
		out, _ := c.run("run", "--help")
		v := strings.Contains(out, "--cap-add")
		c.capAddProbe = &v
	}
	return *c.capAddProbe
}

// Exec runs a command inside a node and returns combined output.
func (c *Client) Exec(name string, command ...string) (string, error) {
	args := append([]string{"exec", name}, command...)
	return c.run(args...)
}

// ExecStdin runs a command inside a node with r piped to stdin.
func (c *Client) ExecStdin(name string, r io.Reader, command ...string) error {
	args := append([]string{"exec", "-i", name}, command...)
	cmd := exec.Command(c.Bin, args...)
	cmd.Stdin = r
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &CommandError{Args: args, Output: string(out), Err: err}
	}
	return nil
}

// WaitReady polls until containerd inside the node answers, i.e. the VM
// finished booting systemd and the kubelet's runtime is up.
func (c *Client) WaitReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := c.Exec(name, "systemctl", "is-active", "containerd")
		if err == nil && strings.TrimSpace(out) == "active" {
			return nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("node %s did not become ready: %w", name, lastErr)
	}
	return fmt.Errorf("node %s did not become ready in %s", name, timeout)
}

// IP returns the node's IPv4 address. It asks the guest for its default
// route source address, which is stable across container CLI versions
// whose `inspect` JSON shapes differ (0.x vs 1.x).
func (c *Client) IP(name string) (string, error) {
	out, err := c.Exec(name, "sh", "-c", "ip -4 route get 1.1.1.1 | awk '{for(i=1;i<NF;i++) if ($i==\"src\") print $(i+1)}'")
	if err == nil {
		ip := strings.TrimSpace(out)
		if regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(ip) {
			return ip, nil
		}
	}
	// Fallback: scrape any IPv4 out of `container inspect`.
	raw, ierr := c.run("inspect", name)
	if ierr != nil {
		if err != nil {
			return "", err
		}
		return "", ierr
	}
	m := regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+)(?:/\d+)?`).FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("could not determine IP address of node %s", name)
	}
	return m[1], nil
}

// IPv6 returns the node's global IPv6 address, the mirror of IP: it asks
// the guest for the source address of its default IPv6 route, which is
// the vmnet-assigned global address (link-local fe80:: is never the
// route source for off-link traffic). Returns "" with no error when the
// node has no global IPv6 address, so callers can tell "IPv6 disabled"
// apart from a lookup failure.
func (c *Client) IPv6(name string) (string, error) {
	// Preferred: the source address the kernel would use for off-link
	// traffic, i.e. the vmnet global address. Fallback: the first
	// global-scope inet6 address on any interface, which appears the
	// moment SLAAC assigns it even if the default route lags a beat
	// behind (both arrive from the same router advertisement at boot).
	out, err := c.Exec(name, "sh", "-c",
		"ip -6 route get 2606:4700:4700::1111 2>/dev/null | awk '{for(i=1;i<NF;i++) if ($i==\"src\") print $(i+1)}' | head -n1; "+
			"ip -6 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Fields(out) {
		parsed := net.ParseIP(line)
		if parsed == nil || parsed.To4() != nil || parsed.To16() == nil {
			continue
		}
		// Reject loopback (::1, which `route get` returns before a v6
		// default route exists) and link-local (fe80::), so only a real
		// vmnet global address is ever returned.
		if parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
			continue
		}
		return line, nil
	}
	return "", nil
}

// NetworkHasIPv6 reports whether the container network the nodes attach
// to advertises an IPv6 subnet. vmnet's default network is dual-stack on
// macOS 26+, but a host on older macOS (or a custom v4-only network)
// hands out no IPv6, and --ip-family dual/ipv6 cannot work there. The
// check reads `container network inspect default` (status.ipv6Subnet),
// tolerating the CLI's differing 0.x/1.x JSON shapes by scanning for any
// ipv6Subnet-like field.
func (c *Client) NetworkHasIPv6(network string) (bool, error) {
	if network == "" {
		network = "default"
	}
	out, err := c.run("network", "inspect", network)
	if err != nil {
		return false, err
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		// A single object rather than an array on some CLI versions.
		var one map[string]any
		if err2 := json.Unmarshal([]byte(strings.TrimSpace(out)), &one); err2 != nil {
			return false, fmt.Errorf("parsing network inspect output: %w", err)
		}
		rows = []map[string]any{one}
	}
	for _, row := range rows {
		if v6 := firstString(row, "status.ipv6Subnet", "ipv6Subnet"); v6 != "" {
			return true, nil
		}
	}
	return false, nil
}

// Info is one row from `container ls`.
type Info struct {
	Name    string
	Image   string
	Status  string
	IP      string // first IPv4 without CIDR suffix; empty while stopped
	Created string // creation timestamp as the CLI reports it (RFC3339)
}

// List returns containers whose names start with prefix (running or not).
func (c *Client) List(prefix string) ([]Info, error) {
	out, err := c.run("ls", "-a", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parseList(out, prefix)
}

// parseList tolerates the differing JSON shapes emitted by container CLI
// 0.x and 1.x by only relying on fields that exist in both.
func parseList(out, prefix string) ([]Info, error) {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		return nil, fmt.Errorf("parsing container ls output: %w", err)
	}
	var infos []Info
	for _, row := range rows {
		info := Info{
			Name:    firstString(row, "configuration.id", "id", "name"),
			Image:   firstString(row, "configuration.image.reference", "image", "imageRef"),
			Status:  firstString(row, "status.state", "status", "state"),
			IP:      firstIPv4(row),
			Created: firstString(row, "configuration.creationDate", "creationDate", "created"),
		}
		if info.Name == "" || !strings.HasPrefix(info.Name, prefix) {
			continue
		}
		infos = append(infos, info)
	}
	return infos, nil
}

var ipv4Re = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// firstIPv4 pulls a node address out of an ls row: CLI 1.x lists it
// under status.networks (ipv4Address), 0.x under a top-level networks
// array (address); both append a CIDR suffix.
func firstIPv4(row map[string]any) string {
	var lists []any
	if st, ok := row["status"].(map[string]any); ok {
		lists = append(lists, st["networks"])
	}
	lists = append(lists, row["networks"])
	for _, l := range lists {
		arr, ok := l.([]any)
		if !ok {
			continue
		}
		for _, item := range arr {
			net, ok := item.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"ipv4Address", "address"} {
				if s, ok := net[key].(string); ok {
					ip, _, _ := strings.Cut(s, "/")
					if ipv4Re.MatchString(ip) {
						return ip
					}
				}
			}
		}
	}
	return ""
}

// firstString digs dotted paths out of loosely-typed JSON and returns the
// first hit.
func firstString(m map[string]any, paths ...string) string {
	for _, p := range paths {
		cur := any(m)
		ok := true
		for _, key := range strings.Split(p, ".") {
			node, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur, isMap = node[key]
			if !isMap {
				ok = false
				break
			}
		}
		if ok {
			if s, isStr := cur.(string); isStr && s != "" {
				return s
			}
		}
	}
	return ""
}

// Remove force-deletes containers, ignoring not-found errors.
// Stop halts a running node VM; the container and its state remain.
func (c *Client) Stop(name string) error {
	_, err := c.run("stop", name)
	return err
}

// Start boots a previously stopped node VM.
func (c *Client) Start(name string) error {
	_, err := c.run("start", name)
	return err
}

func (c *Client) Remove(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, names...)
	out, err := c.run(args...)
	if err != nil && !strings.Contains(out, "not found") {
		return err
	}
	return nil
}

// ImageSave exports a local image to an OCI tarball.
func (c *Client) ImageSave(image, path string) error {
	_, err := c.run("image", "save", image, "--output", path)
	return err
}

// ImagePull pulls an image so `run` starts instantly afterwards.
func (c *Client) ImagePull(image string) error {
	_, err := c.run("image", "pull", image)
	return err
}
