package cluster

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

func TestEdgeProxyEmbeddedBinary(t *testing.T) {
	bin, err := edgeProxyBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bin, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatalf("embedded edge proxy is not an ELF binary")
	}
	if len(bin) < 1024*1024 {
		t.Fatalf("embedded edge proxy is suspiciously small: %d bytes", len(bin))
	}
}

func TestEdgeProxyTunnelToken(t *testing.T) {
	token, err := newEdgeProxyTunnelToken()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("token contains %d random bytes, want 32", len(decoded))
	}
}

func TestEdgeProxyUsesLeastPrivilegeRBAC(t *testing.T) {
	for _, want := range []string{
		"kind: ServiceAccount",
		"resources: [services, nodes]",
		"resources: [endpointslices]",
		"verbs: [get, list]",
		"kubernetes.io/service-account.name: kiac-edge-proxy",
	} {
		if !strings.Contains(edgeProxyRBAC, want) {
			t.Errorf("edge proxy RBAC missing %q", want)
		}
	}
	for _, forbidden := range []string{"verbs: [*]", "resources: [*]", "create", "update", "patch", "delete"} {
		if strings.Contains(edgeProxyRBAC, forbidden) {
			t.Errorf("edge proxy RBAC unexpectedly grants %q", forbidden)
		}
	}
}

func TestEdgeProxyKubectlCommand(t *testing.T) {
	wantKubeadm := []string{"kubectl", "--kubeconfig", adminConf, "get", "nodes"}
	if got := edgeProxyKubectl(adminConf, "get", "nodes"); !slices.Equal(got, wantKubeadm) {
		t.Fatalf("kubeadm kubectl args = %q, want %q", got, wantKubeadm)
	}
	wantK3s := []string{"kubectl", "--kubeconfig", k3sKubeconfig, "get", "nodes"}
	if got := edgeProxyKubectl(k3sKubeconfig, "get", "nodes"); !slices.Equal(got, wantK3s) {
		t.Fatalf("k3s kubectl args = %q, want %q", got, wantK3s)
	}
}

func TestEdgeProxySupervisorScript(t *testing.T) {
	for _, want := range []string{
		edgeProxyNodePath,
		edgeProxyKubeconfigPath,
		edgeProxyTokenPath,
		edgeProxyLogPath,
		edgeProxySupervisorPID,
		"--kubeconfig",
		"--token-file",
		"while :;",
	} {
		if !strings.Contains(edgeProxySupervisorScript, want) {
			t.Errorf("supervisor script missing %q", want)
		}
	}
	if strings.ContainsRune(edgeProxySupervisorScript, '`') {
		t.Error("supervisor script contains a backtick; keep shell snippets Go-raw-string friendly")
	}
}

func TestK3sBootRestartsEdgeProxy(t *testing.T) {
	_, args := k3sBoot(Config{}, []string{"server"})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		edgeProxyNodePath,
		edgeProxyKubeconfigPath,
		edgeProxyTokenPath,
		edgeProxyLogPath,
		edgeProxySupervisorPID,
		"--kubeconfig",
		"--token-file",
		"exec k3s 'server'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("k3s boot command missing %q", want)
		}
	}
}
