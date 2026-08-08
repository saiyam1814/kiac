package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestResolveNode(t *testing.T) {
	infos := []runtime.Info{
		{Name: "kiac-dev-control-plane"},
		{Name: "kiac-dev-worker-1"},
		{Name: "kiac-dev-worker-2"},
	}
	cases := []struct {
		name    string
		cluster string
		node    string
		want    string
		wantErr string // substring of the error, empty for success
	}{
		{name: "short worker", cluster: "dev", node: "worker-1", want: "kiac-dev-worker-1"},
		{name: "short control plane", cluster: "dev", node: "control-plane", want: "kiac-dev-control-plane"},
		{name: "full container name", cluster: "dev", node: "kiac-dev-worker-2", want: "kiac-dev-worker-2"},
		{name: "unknown node lists choices", cluster: "dev", node: "worker-9", wantErr: "control-plane, worker-1, worker-2"},
		{name: "full name from another cluster", cluster: "dev", node: "kiac-prod-worker-1", wantErr: `no node "kiac-prod-worker-1"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveNode(infos, c.cluster, c.node)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("resolveNode(%q, %q) = %q, want %q", c.cluster, c.node, got, c.want)
			}
		})
	}
}

func TestStartNodeK3sRequiresResumeForMultipleStoppedNodes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	script := `#!/bin/sh
case "$*" in
  "ls -a --format json")
    printf '%s\n' '[{"configuration":{"id":"kiac-dev-control-plane","image":{"reference":"docker.io/rancher/k3s:v1.36.2-k3s1"}},"status":{"state":"stopped"}},{"configuration":{"id":"kiac-dev-worker-1","image":{"reference":"docker.io/rancher/k3s:v1.36.2-k3s1"}},"status":{"state":"stopped"}}]'
    ;;
  *)
    printf 'unexpected command: %s\n' "$*" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := (&Manager{rt: &runtime.Client{Bin: bin}}).StartNode("dev", "worker-1")
	if err == nil || !strings.Contains(err.Error(), "has 2 stopped nodes") || !strings.Contains(err.Error(), "kiac resume cluster --name dev") {
		t.Fatalf("StartNode error = %v, want explicit whole-cluster resume guidance", err)
	}
}
