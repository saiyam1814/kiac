package cluster

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

const maxSupportFileBytes = 512 << 10

// SupportBundleOptions controls collection without changing cluster state.
type SupportBundleOptions struct {
	Output         string
	KiacVersion    string
	CommandTimeout time.Duration
}

// SupportBundleResult describes the archive written for the user.
type SupportBundleResult struct {
	Path     string
	Files    int
	Warnings []string
}

type supportFile struct {
	name string
	data []byte
}

type supportCollector struct {
	files    []supportFile
	warnings []string
	home     string
}

// SupportBundle collects bounded, read-only diagnostics. It deliberately does
// not read kubeconfigs, Kubernetes Secrets, container inspect output, process
// environments, or workload logs. All collected text still passes through the
// credential redactor before it reaches disk.
func (m *Manager) SupportBundle(name string, opts SupportBundleOptions) (SupportBundleResult, error) {
	if !ValidName(name) {
		return SupportBundleResult{}, fmt.Errorf("invalid cluster name %q: use lowercase letters, digits, and dashes", name)
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 10 * time.Second
	}
	now := time.Now().UTC()
	output, err := supportOutputPath(name, now, opts.Output)
	if err != nil {
		return SupportBundleResult{}, err
	}
	infos, err := m.rt.List(prefix(name))
	if err != nil {
		return SupportBundleResult{}, err
	}
	if len(infos) == 0 {
		return SupportBundleResult{}, fmt.Errorf("no cluster named %q found", name)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	distro := distroFromNodes(infos)
	cp := ControlPlane(name)

	home, _ := os.UserHomeDir()
	collector := &supportCollector{home: home}
	collector.addText("README.txt", supportBundleReadme)

	report, verifyErr := m.Verify(name, opts.CommandTimeout)
	if verifyErr != nil {
		collector.warnings = append(collector.warnings, "verification: "+compactError(verifyErr))
	}
	collector.addJSON("cluster/verify.json", report)

	version, versionErr := m.rt.Version()
	if versionErr != nil {
		collector.warnings = append(collector.warnings, "runtime version: "+compactError(versionErr))
	}
	statuses := BuildStatuses(infos)
	metadata := struct {
		SchemaVersion    int             `json:"schemaVersion"`
		GeneratedAt      string          `json:"generatedAt"`
		KiacVersion      string          `json:"kiacVersion"`
		ContainerVersion string          `json:"containerVersion,omitempty"`
		Cluster          string          `json:"cluster"`
		Distro           string          `json:"distro"`
		Status           []ClusterStatus `json:"status"`
	}{
		SchemaVersion:    1,
		GeneratedAt:      now.Format(time.RFC3339),
		KiacVersion:      opts.KiacVersion,
		ContainerVersion: version,
		Cluster:          name,
		Distro:           distro,
		Status:           statuses,
	}
	collector.addJSON("metadata.json", metadata)

	collector.collectHost(m.rt, opts.CommandTimeout)
	collector.collectKubernetes(m, cp, distro, report, opts.CommandTimeout)
	collector.collectNodes(m.rt, infos, distro, opts.CommandTimeout)

	if err := writeSupportArchive(output, "kiac-support-"+name, now, collector.files); err != nil {
		return SupportBundleResult{}, err
	}
	return SupportBundleResult{Path: output, Files: len(collector.files), Warnings: collector.warnings}, nil
}

func (c *supportCollector) collectHost(rt *runtime.Client, timeout time.Duration) {
	out, err := hostDiagnosticCommand(timeout, "sw_vers")
	c.addCommand("host/macos.txt", "sw_vers", out, err, true)
	out, err = hostDiagnosticCommand(timeout, "uname", "-mrs")
	c.addCommand("host/kernel.txt", "uname -mrs", out, err, true)
	out, err = rt.SystemStatus(timeout)
	c.addCommand("runtime/system-status.txt", "container system status", out, err, false)
}

func (c *supportCollector) collectKubernetes(m *Manager, cp, distro string, report VerificationReport, timeout time.Duration) {
	if !verificationReachedAPI(report) {
		c.addText("kubernetes/NOT-COLLECTED.txt", "The control-plane VM or Kubernetes API was unavailable. See cluster/verify.json.\n")
		return
	}
	commands := []struct {
		file string
		args []string
	}{
		{"version.txt", []string{"version", "-o", "yaml"}},
		{"nodes.txt", []string{"get", "nodes", "-o", "wide"}},
		{"pods.txt", []string{"get", "pods", "-A", "-o", "wide"}},
		{"workloads.txt", []string{"get", "deployments,statefulsets,daemonsets", "-A", "-o", "wide"}},
		{"services.txt", []string{"get", "services,endpointslices", "-A", "-o", "wide"}},
		{"storage.txt", []string{"get", "storageclass,persistentvolumeclaim,persistentvolume", "-A", "-o", "wide"}},
		{"api-services.txt", []string{"get", "apiservices", "-o", "wide"}},
		{"events.txt", []string{"get", "events", "-A", "--sort-by=.metadata.creationTimestamp"}},
	}
	if verificationStatus(report, "metrics.api") != VerificationSkip {
		commands = append(commands, struct {
			file string
			args []string
		}{"metrics.txt", []string{"top", "nodes"}})
	}
	if verificationStatus(report, "gateway.api") != VerificationSkip {
		commands = append(commands, struct {
			file string
			args []string
		}{"gateway.txt", []string{"get", "gatewayclasses,gateways,httproutes", "-A", "-o", "wide"}})
	}
	if verificationStatus(report, "gpu.device-plugin") != VerificationSkip {
		commands = append(commands,
			struct {
				file string
				args []string
			}{"gpu-runtimeclass.txt", []string{"get", "runtimeclass", gpuRuntimeClass, "-o", "wide"}},
			struct {
				file string
				args []string
			}{"gpu-device-plugin.txt", []string{"get", "daemonset", gpuDevicePlugin, "-n", gpuNamespace, "-o", "wide"}},
			struct {
				file string
				args []string
			}{"gpu-nodes.txt", []string{"get", "nodes", "-l", gpuLabelPresent + "=true", "-o", "wide"}},
		)
	}
	for _, command := range commands {
		out, err := m.diagnosticKubectl(cp, distro, timeout, command.args...)
		full := "container exec " + cp + " kubectl " + strings.Join(command.args, " ")
		c.addCommand("kubernetes/"+command.file, full, out, err, false)
	}
}

func (c *supportCollector) collectNodes(rt *runtime.Client, infos []runtime.Info, distro string, timeout time.Duration) {
	for _, info := range infos {
		base := "nodes/" + info.Name + "/"
		if !strings.EqualFold(info.Status, "running") {
			c.addText(base+"NOT-COLLECTED.txt", "Node VM is "+orUnknown(info.Status)+".\n")
			continue
		}
		out, err := rt.Logs(info.Name, timeout)
		c.addCommand(base+"console.log", "container logs "+info.Name, out, err, false)

		commands := []struct {
			file string
			args []string
		}{
			{"resources.txt", []string{"sh", "-c", "df -h; printf '\\n'; free -h 2>/dev/null || true"}},
			{"network.txt", []string{"sh", "-c", "ip -brief address; printf '\\nIPv4 routes:\\n'; ip route; printf '\\nIPv6 routes:\\n'; ip -6 route 2>/dev/null || true"}},
			{"containers.txt", []string{"sh", "-c", "crictl ps -a 2>/dev/null || ctr -n k8s.io containers list 2>/dev/null || true"}},
			{"edge-proxy.log", []string{"sh", "-c", "tail -500 " + edgeProxyLogPath + " 2>/dev/null || true"}},
		}
		if distro == "k3s" {
			if strings.HasSuffix(info.Name, "-control-plane") {
				commands = append(commands, struct {
					file string
					args []string
				}{"load-balancer.log", []string{"sh", "-c", "tail -500 " + kiacLBK3sLogPath + " 2>/dev/null || true"}})
			}
		} else {
			commands = append(commands,
				struct {
					file string
					args []string
				}{"kubelet.log", []string{"journalctl", "--no-pager", "-n", "500", "-u", "kubelet"}},
				struct {
					file string
					args []string
				}{"containerd.log", []string{"journalctl", "--no-pager", "-n", "500", "-u", "containerd"}},
			)
			if strings.HasSuffix(info.Name, "-control-plane") {
				commands = append(commands, struct {
					file string
					args []string
				}{"load-balancer.log", []string{"journalctl", "--no-pager", "-n", "500", "-u", "kiac-lb"}})
			}
		}
		for _, command := range commands {
			out, err := rt.ExecTimeout(info.Name, timeout, command.args...)
			full := "container exec " + info.Name + " " + strings.Join(command.args, " ")
			c.addCommand(base+command.file, full, out, err, false)
		}
	}
}

func (c *supportCollector) addJSON(name string, value any) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		c.warnings = append(c.warnings, name+": "+err.Error())
		c.addText(name, "JSON encoding failed: "+err.Error()+"\n")
		return
	}
	c.addText(name, string(raw)+"\n")
}

func (c *supportCollector) addCommand(name, command, output string, err error, optional bool) {
	status := "success"
	if err != nil {
		status = diagnosticError(err)
		if !optional {
			c.warnings = append(c.warnings, name+": "+compactError(err))
		}
	}
	body := "Command: " + command + "\nStatus: " + status + "\n\n" + output
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	c.addText(name, body)
}

func (c *supportCollector) addText(name, text string) {
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		c.warnings = append(c.warnings, "ignored unsafe archive path: "+name)
		return
	}
	text = redactSupportText(text, c.home)
	if len(text) > maxSupportFileBytes {
		text = truncateSupportText(text)
	}
	c.files = append(c.files, supportFile{name: clean, data: []byte(text)})
}

func hostDiagnosticCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return string(out), err
}

func diagnosticError(err error) string {
	var commandErr *runtime.CommandError
	if errors.As(err, &commandErr) {
		return "failed: " + compact(commandErr.Err.Error())
	}
	return "failed: " + compact(err.Error())
}

func verificationReachedAPI(report VerificationReport) bool {
	return verificationStatus(report, "kubernetes.api") == VerificationPass
}

func verificationStatus(report VerificationReport, id string) VerificationStatus {
	for _, check := range report.Checks {
		if check.ID == id {
			return check.Status
		}
	}
	return VerificationSkip
}

func supportOutputPath(name string, now time.Time, requested string) (string, error) {
	filename := fmt.Sprintf("kiac-support-%s-%s.tar.gz", name, now.Format("20060102T150405Z"))
	if requested == "" {
		requested = filename
	} else if info, err := os.Stat(requested); err == nil && info.IsDir() {
		requested = filepath.Join(requested, filename)
	} else if !strings.HasSuffix(requested, ".tar.gz") && !strings.HasSuffix(requested, ".tgz") {
		requested += ".tar.gz"
	}
	abs, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolving support bundle path: %w", err)
	}
	return abs, nil
}

func truncateSupportText(text string) string {
	half := (maxSupportFileBytes - 100) / 2
	headEnd := half
	for headEnd > 0 && !utf8.RuneStart(text[headEnd]) {
		headEnd--
	}
	tailStart := len(text) - half
	for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	return text[:headEnd] + "\n\n[... output truncated by kiac ...]\n\n" + text[tailStart:]
}

func writeSupportArchive(output, root string, now time.Time, files []supportFile) (err error) {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("creating support bundle directory: %w", err)
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating support bundle %s: %w", output, err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = f.Close()
			_ = os.Remove(output)
		}
	}()

	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, file := range files {
		name := path.Join(root, file.name)
		header := &tar.Header{
			Name:    name,
			Mode:    0o600,
			Size:    int64(len(file.data)),
			ModTime: now,
		}
		if err := tw.WriteHeader(header); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return fmt.Errorf("writing support bundle header: %w", err)
		}
		if _, err := tw.Write(file.data); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return fmt.Errorf("writing support bundle data: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("finishing support bundle: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("compressing support bundle: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("securing support bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing support bundle: %w", err)
	}
	complete = true
	return nil
}

var supportRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?is)-----BEGIN [^-\n]*PRIVATE KEY-----.*?-----END [^-\n]*PRIVATE KEY-----`), "[REDACTED PRIVATE KEY]"},
	{regexp.MustCompile(`(?im)^(\s*"?(?:token|password|client-key-data|client-certificate-data|certificate-authority-data|k3s_token|tunnel_token)"?\s*[:=]\s*).+$`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)\S+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(--(?:token|password)(?:=|\s+))\S+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`K10[a-zA-Z0-9._:-]+::server:[a-zA-Z0-9]+`), "[REDACTED K3S TOKEN]"},
	{regexp.MustCompile(`(?i)((?:tunnel[._-]?token|k3s_token)\s*[:=]\s*)[a-f0-9]{32,}`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(https?://[^\s:/]+:)[^@\s]+@`), `${1}[REDACTED]@`},
}

func redactSupportText(text, home string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "?")
	}
	if home != "" && home != "/" {
		text = strings.ReplaceAll(text, home, "$HOME")
	}
	for _, redaction := range supportRedactions {
		text = redaction.re.ReplaceAllString(text, redaction.repl)
	}
	return text
}

const supportBundleReadme = `kiac support bundle

This archive contains read-only host, Apple container runtime, Kubernetes,
and kiac node diagnostics. It intentionally excludes:

- kubeconfig files and client certificates
- Kubernetes Secret objects
- raw container inspect output and process environments
- application workload logs
- edge-proxy token and credential files

Collected text is bounded and passed through credential-pattern redaction.
Names, image references, IP addresses, Kubernetes events, and system logs are
still operational data. Review the archive before sharing it publicly.
`
