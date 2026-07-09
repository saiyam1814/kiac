package cluster

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestKiacLBScriptSyntax feeds the embedded script through `sh -n`: the
// controller runs under the node's POSIX sh, so a syntax error would
// only surface inside a live VM otherwise.
func TestKiacLBScriptSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	path := filepath.Join(t.TempDir(), "kiac-lb.sh")
	if err := os.WriteFile(path, []byte(kiacLBScript), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(sh, "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n rejected kiac-lb.sh: %v\n%s", err, out)
	}
}

// TestKiacLBScriptInvariants pins the parts of the script other code
// depends on: the configurable kubeconfig path, the status patch, the
// primary label, and the node-image constraint that jq is unavailable.
func TestKiacLBScriptInvariants(t *testing.T) {
	for _, want := range []string{
		"#!/bin/sh",
		`${KUBECONFIG:=` + adminConf + `}`, // kubeadm default; k3s overrides it
		"--subresource=status",
		`{"status":{"loadBalancer":{"ingress":[{"ip":"`,
		"kiac.io/lb-primary=true",                // prefers the labeled primary node
		"kubernetes.io/service-name=",            // endpointslice lookup for pod-local IPs
		"!node-role.kubernetes.io/control-plane", // workers-first eligibility
	} {
		if !strings.Contains(kiacLBScript, want) {
			t.Errorf("kiac-lb.sh is missing %q", want)
		}
	}
	for _, line := range strings.Split(kiacLBScript, "\n") {
		// Comments may mention jq (the constraint itself); code may not.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "jq") {
			t.Errorf("kiac-lb.sh invokes jq, which the node image does not ship: %q", line)
		}
	}
	if strings.ContainsRune(kiacLBScript, '`') {
		t.Error("kiac-lb.sh contains a backtick; use $(...) so the Go raw string stays valid")
	}
}

func TestKiacLBK3sSupervisor(t *testing.T) {
	for _, want := range []string{
		kiacLBScriptPath,
		kiacLBK3sSupervisorPID,
		kiacLBK3sLogPath,
		"KUBECONFIG=" + k3sKubeconfig,
		"while :; do",
	} {
		if !strings.Contains(kiacLBK3sSupervisorScript, want) {
			t.Errorf("k3s kiac-lb supervisor is missing %q", want)
		}
	}
	if strings.ContainsRune(kiacLBK3sSupervisorScript, '`') {
		t.Error("k3s kiac-lb supervisor contains a backtick; use $(...) so the Go raw string stays valid")
	}
}

// TestKiacLBUnitFile parses the systemd unit as sections of key=value
// pairs and pins the settings the design relies on.
func TestKiacLBUnitFile(t *testing.T) {
	kv := map[string]string{} // "Section/Key" -> value
	section := ""
	for i, line := range strings.Split(kiacLBUnit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Fatalf("line %d is neither [Section] nor key=value: %q", i+1, line)
		}
		kv[section+"/"+k] = v
	}
	for key, want := range map[string]string{
		"Service/ExecStart": kiacLBScriptPath,
		"Service/Restart":   "always",
		"Install/WantedBy":  "multi-user.target",
	} {
		if got := kv[key]; got != want {
			t.Errorf("unit %s = %q, want %q", key, got, want)
		}
	}
	if _, ok := kv["Unit/Description"]; !ok {
		t.Error("unit has no [Unit] Description")
	}
}

// TestLBInstallSteps checks the exec argv construction: the script and
// unit are piped via stdin into `cat >` (no payload quoting through
// argv), and the service is enabled in the same create.
func TestLBInstallSteps(t *testing.T) {
	steps := lbInstallSteps()
	if len(steps) != 3 {
		t.Fatalf("lbInstallSteps() returned %d steps, want 3", len(steps))
	}
	if steps[0].stdin != kiacLBScript {
		t.Error("step 0 must pipe the script via stdin")
	}
	if !strings.Contains(steps[0].args[2], "cat > "+kiacLBScriptPath) ||
		!strings.Contains(steps[0].args[2], "chmod 0755 "+kiacLBScriptPath) {
		t.Errorf("step 0 must write and chmod %s, got %q", kiacLBScriptPath, steps[0].args)
	}
	if steps[1].stdin != kiacLBUnit {
		t.Error("step 1 must pipe the unit via stdin")
	}
	if !strings.Contains(steps[1].args[2], "cat > "+kiacLBUnitPath) {
		t.Errorf("step 1 must write %s, got %q", kiacLBUnitPath, steps[1].args)
	}
	if steps[2].stdin != "" {
		t.Error("step 2 takes no stdin")
	}
	if !strings.Contains(steps[2].args[2], "systemctl daemon-reload") ||
		!strings.Contains(steps[2].args[2], "enable --now kiac-lb.service") {
		t.Errorf("step 2 must daemon-reload and enable --now, got %q", steps[2].args)
	}
	for i, s := range steps {
		if len(s.args) != 3 || s.args[0] != "sh" || s.args[1] != "-c" {
			t.Errorf("step %d must be [sh -c <cmd>], got %q", i, s.args)
		}
	}
}
