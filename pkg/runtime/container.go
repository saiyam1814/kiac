// Package runtime drives Apple's `container` CLI. Every Kubernetes node
// kiac creates is one apple/containerization lightweight VM managed by
// that binary, so this package is the only place that shells out to it.
package runtime

import (
	"encoding/json"
	"fmt"
	"io"
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
	Name   string
	Image  string
	CPUs   string
	Memory string
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
	args = append(args, o.Image)
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

// Info is one row from `container ls`.
type Info struct {
	Name   string
	Image  string
	Status string
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
			Name:   firstString(row, "configuration.id", "id", "name"),
			Image:  firstString(row, "configuration.image.reference", "image", "imageRef"),
			Status: firstString(row, "status", "state"),
		}
		if info.Name == "" || !strings.HasPrefix(info.Name, prefix) {
			continue
		}
		infos = append(infos, info)
	}
	return infos, nil
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
