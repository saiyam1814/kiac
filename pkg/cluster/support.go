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

type supportKubectlCommand struct {
	file     string
	args     []string
	optional bool
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
	hasGPU := supportHasGPU(infos)
	hasContainer := false
	for _, info := range infos {
		if info.Backend == "" || info.Backend == runtime.BackendContainer {
			hasContainer = true
			break
		}
	}

	home, _ := os.UserHomeDir()
	collector := &supportCollector{home: home}
	collector.addText("README.txt", supportBundleReadme)

	report, verifyErr := m.Verify(name, opts.CommandTimeout)
	if verifyErr != nil {
		collector.warnings = append(collector.warnings, "verification: "+compactError(verifyErr))
	}
	collector.addJSON("cluster/verify.json", report)

	version := ""
	if hasContainer {
		var versionErr error
		version, versionErr = m.rt.Version()
		if versionErr != nil {
			collector.warnings = append(collector.warnings, "runtime version: "+compactError(versionErr))
		}
	}
	statuses := BuildStatuses(infos)
	var gpuStatus *GPUStatus
	if hasGPU {
		status, statusErr := m.GPUStatusForCluster(name)
		if statusErr != nil {
			collector.warnings = append(collector.warnings, "GPU status: "+compactError(statusErr))
		} else {
			gpuStatus = &status
		}
	}
	metadata := struct {
		SchemaVersion    int             `json:"schemaVersion"`
		GeneratedAt      string          `json:"generatedAt"`
		KiacVersion      string          `json:"kiacVersion"`
		ContainerVersion string          `json:"containerVersion,omitempty"`
		Cluster          string          `json:"cluster"`
		Distro           string          `json:"distro"`
		Status           []ClusterStatus `json:"status"`
		GPU              *GPUStatus      `json:"gpu,omitempty"`
	}{
		SchemaVersion:    1,
		GeneratedAt:      now.Format(time.RFC3339),
		KiacVersion:      opts.KiacVersion,
		ContainerVersion: version,
		Cluster:          name,
		Distro:           distro,
		Status:           statuses,
		GPU:              gpuStatus,
	}
	collector.addJSON("metadata.json", metadata)

	collector.collectHost(m.rt, opts.CommandTimeout, hasContainer, hasGPU)
	collector.collectKubernetes(m, cp, distro, report, opts.CommandTimeout, hasGPU)
	collector.collectNodes(m.rt, infos, distro, opts.CommandTimeout)

	if err := writeSupportArchive(output, "kiac-support-"+name, now, collector.files); err != nil {
		return SupportBundleResult{}, err
	}
	return SupportBundleResult{Path: output, Files: len(collector.files), Warnings: collector.warnings}, nil
}

func (c *supportCollector) collectHost(rt runtime.HostRuntime, timeout time.Duration, hasContainer, hasGPU bool) {
	out, err := hostDiagnosticCommand(timeout, "sw_vers")
	c.addCommand("host/macos.txt", "sw_vers", out, err, true)
	out, err = hostDiagnosticCommand(timeout, "uname", "-mrs")
	c.addCommand("host/kernel.txt", "uname -mrs", out, err, true)
	if hasContainer {
		out, err = rt.SystemStatus(timeout)
		c.addCommand("runtime/container-system-status.txt", "container system status", out, err, false)
	}
	if hasGPU {
		out, err = hostDiagnosticCommand(timeout, "krunkit", "--version")
		c.addCommand("runtime/krunkit-version.txt", "krunkit --version", out, err, true)
		out, err = hostDiagnosticCommand(timeout, "vmnet-run", "--version")
		c.addCommand("runtime/vmnet-helper-version.txt", "vmnet-run --version", out, err, true)
	}
}

func (c *supportCollector) collectKubernetes(m *Manager, cp, distro string, report VerificationReport, timeout time.Duration, hasGPU bool) {
	if !verificationReachedAPI(report) {
		c.addText("kubernetes/NOT-COLLECTED.txt", "The control-plane VM or Kubernetes API was unavailable. See cluster/verify.json.\n")
		return
	}
	commands := []supportKubectlCommand{
		{file: "version.txt", args: []string{"version", "-o", "yaml"}},
		{file: "nodes.txt", args: []string{"get", "nodes", "-o", "wide"}},
		{file: "pods.txt", args: []string{"get", "pods", "-A", "-o", "wide"}},
		{file: "workloads.txt", args: []string{"get", "deployments,statefulsets,daemonsets", "-A", "-o", "wide"}},
		{file: "services.txt", args: []string{"get", "services,endpointslices", "-A", "-o", "wide"}},
		{file: "storage.txt", args: []string{"get", "storageclass,persistentvolumeclaim,persistentvolume", "-A", "-o", "wide"}},
		{file: "api-services.txt", args: []string{"get", "apiservices", "-o", "wide"}},
		{file: "events.txt", args: []string{"get", "events", "-A", "--sort-by=.metadata.creationTimestamp"}},
	}
	if verificationStatus(report, "metrics.api") != VerificationSkip {
		commands = append(commands, supportKubectlCommand{file: "metrics.txt", args: []string{"top", "nodes"}})
	}
	if verificationStatus(report, "gateway.api") != VerificationSkip {
		commands = append(commands, supportKubectlCommand{file: "gateway.txt", args: []string{"get", "gatewayclasses,gateways,httproutes", "-A", "-o", "wide"}})
	}
	if hasGPU {
		commands = append(commands,
			supportKubectlCommand{file: "gpu-driver.txt", args: []string{"get", "daemonsets,pods", "-n", "kube-system", "-l", "app.kubernetes.io/name in (kiac-gpu-dra,kiac-gpu-device-plugin,kiac-gpu-compat)", "-o", "wide"}},
			supportKubectlCommand{file: "gpu-dra.txt", args: []string{"get", "deviceclasses.resource.k8s.io,resourceslices.resource.k8s.io", "-o", "yaml"}, optional: true},
			supportKubectlCommand{file: "gpu-compat.txt", args: []string{"get", "mutatingwebhookconfiguration", gpuCompatName, "--ignore-not-found", "-o", "yaml"}, optional: true},
			supportKubectlCommand{file: "gpu-compat-namespaces.txt", args: []string{"get", "namespaces", "-l", gpuCompatNamespaceLabel, "--show-labels"}, optional: true},
		)
	}
	for _, command := range commands {
		out, err := m.diagnosticKubectl(cp, distro, timeout, command.args...)
		full := "runtime exec " + cp + " kubectl " + strings.Join(command.args, " ")
		c.addCommand("kubernetes/"+command.file, full, out, err, command.optional)
	}
	if hasGPU {
		for _, workload := range []string{"daemonset/kiac-gpu-dra", "daemonset/kiac-gpu-device-plugin", "deployment/" + gpuCompatName} {
			args := []string{"logs", "-n", "kube-system", workload, "--all-pods=true", "--tail=500"}
			out, err := m.diagnosticKubectl(cp, distro, timeout, args...)
			file := "kubernetes/" + strings.NewReplacer("/", "-", "kiac-", "").Replace(workload) + ".log"
			c.addCommand(file, "runtime exec "+cp+" kubectl "+strings.Join(args, " "), out, err, true)
		}
	}
}

func (c *supportCollector) collectNodes(rt runtime.NodeBackend, infos []runtime.Info, distro string, timeout time.Duration) {
	for _, info := range infos {
		base := "nodes/" + info.Name + "/"
		if !strings.EqualFold(info.Status, "running") {
			c.addText(base+"NOT-COLLECTED.txt", "Node VM is "+orUnknown(info.Status)+".\n")
			continue
		}
		out, err := rt.Logs(info.Name, timeout)
		c.addCommand(base+"console.log", "runtime logs "+info.Name, out, err, false)

		commands := []struct {
			file string
			args []string
		}{
			{"resources.txt", []string{"sh", "-c", "df -h; printf '\\n'; free -h 2>/dev/null || true"}},
			{"network.txt", []string{"sh", "-c", "ip -brief address; printf '\\nIPv4 routes:\\n'; ip route; printf '\\nIPv6 routes:\\n'; ip -6 route 2>/dev/null || true"}},
			{"containers.txt", []string{"sh", "-c", "crictl ps -a 2>/dev/null || ctr -n k8s.io containers list 2>/dev/null || true"}},
			{"edge-proxy.log", []string{"sh", "-c", "tail -500 " + edgeProxyLogPath + " 2>/dev/null || true"}},
		}
		if info.GPU || isGPUNode(info.Name) {
			commands = append(commands, struct {
				file string
				args []string
			}{"gpu.txt", []string{"sh", "-c", "ls -la /dev/dri 2>/dev/null || true; printf '\\nrender device:\\n'; readlink -f /sys/class/drm/renderD128/device 2>/dev/null || true; cat /sys/class/drm/renderD128/device/uevent 2>/dev/null || true"}})
		}
		if distro == "k3s" {
			if info.Backend == runtime.BackendKrunkit {
				commands = append(commands, struct {
					file string
					args []string
				}{"k3s.log", []string{"journalctl", "--no-pager", "-n", "500", "-u", "k3s"}})
			}
			if strings.HasSuffix(info.Name, "-control-plane") {
				if info.Backend == runtime.BackendKrunkit {
					commands = append(commands, struct {
						file string
						args []string
					}{"load-balancer.log", []string{"journalctl", "--no-pager", "-n", "500", "-u", "kiac-lb"}})
				} else {
					commands = append(commands, struct {
						file string
						args []string
					}{"load-balancer.log", []string{"sh", "-c", "tail -500 " + kiacLBK3sLogPath + " 2>/dev/null || true"}})
				}
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
			full := "runtime exec " + info.Name + " " + strings.Join(command.args, " ")
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

func supportHasGPU(infos []runtime.Info) bool {
	for _, info := range infos {
		if info.GPU || isGPUNode(info.Name) {
			return true
		}
	}
	return false
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
	{regexp.MustCompile(`(?i)(?:\$HOME|/Users/[^/\s]+)?/\.kiac/gpu-nodes/id_ed25519(?:\.pub)?`), "[REDACTED GPU SSH KEY PATH]"},
	{regexp.MustCompile(`(?im)^(\s*(?:identityfile|ssh_key|private_key_path)\s*[:=]?\s*).*(?:id_ed25519|gpu-nodes).*$`), `${1}[REDACTED GPU SSH KEY PATH]`},
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

This archive contains read-only host, active VM runtime, Kubernetes, kiac
node, and Apple GPU diagnostics. It intentionally excludes:

- kubeconfig files and client certificates
- Kubernetes Secret objects
- raw container inspect output and process environments
- application workload logs
- edge-proxy token and credential files
- kiac GPU SSH private keys and compatibility webhook TLS Secrets

Collected text is bounded and passed through credential-pattern redaction.
Names, image references, IP addresses, Kubernetes events, and system logs are
still operational data. Review the archive before sharing it publicly.
`
