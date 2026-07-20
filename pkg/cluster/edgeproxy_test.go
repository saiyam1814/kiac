package cluster

import (
	"bytes"
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

func TestEdgeProxySupervisorScript(t *testing.T) {
	for _, want := range []string{
		edgeProxyNodePath,
		edgeProxyKubeconfigPath,
		edgeProxyLogPath,
		edgeProxySupervisorPID,
		"--kubeconfig",
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
		edgeProxyLogPath,
		edgeProxySupervisorPID,
		"--kubeconfig",
		"exec k3s 'server'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("k3s boot command missing %q", want)
		}
	}
}
