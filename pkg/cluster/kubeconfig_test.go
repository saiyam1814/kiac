package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleAdminConf = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Zm9v
    server: https://kiac-dev-control-plane:6443
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: kubernetes-admin
  name: kubernetes-admin@kubernetes
current-context: kubernetes-admin@kubernetes
users:
- name: kubernetes-admin
  user:
    client-certificate-data: Zm9v
    client-key-data: Zm9v
`

func TestMergeAndRemoveKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	t.Setenv("KUBECONFIG", path)

	merged, err := mergeKubeconfig("dev", sampleAdminConf, "192.168.64.2")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged != path {
		t.Fatalf("merged into %q, want %q", merged, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{"kiac-dev", "https://192.168.64.2:6443", "current-context: kiac-dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("merged kubeconfig missing %q:\n%s", want, out)
		}
	}

	// Merging again must not duplicate entries.
	if _, err := mergeKubeconfig("dev", sampleAdminConf, "192.168.64.9"); err != nil {
		t.Fatalf("re-merge: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if got := strings.Count(string(raw), "name: kiac-dev"); got != 3 {
		t.Errorf("expected exactly 3 kiac-dev entries (cluster/user/context), got %d", got)
	}
	if !strings.Contains(string(raw), "192.168.64.9") {
		t.Error("re-merge did not update server address")
	}

	if err := removeKubeconfig("dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "kiac-dev") {
		t.Errorf("kiac-dev entries left after removal:\n%s", raw)
	}
}

func TestServiceAccountKubeconfigDropsAdminCredentials(t *testing.T) {
	got, err := serviceAccountKubeconfig("dev", sampleAdminConf, "192.168.64.2", "restricted-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kiac-dev-edge-proxy",
		"https://192.168.64.2:6443",
		"certificate-authority-data: Zm9v",
		"token: restricted-token",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("restricted kubeconfig missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"client-certificate-data", "client-key-data", "kubernetes-admin"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("restricted kubeconfig contains admin credential %q:\n%s", forbidden, got)
		}
	}
}
