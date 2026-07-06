package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestBuildStatuses(t *testing.T) {
	infos := []runtime.Info{
		{Name: "kiac-dev-worker-1", Image: "docker.io/kindest/node:v1.36.1@sha256:abc", Status: "running", IP: "192.168.64.3", Created: "2026-06-11T10:00:05Z"},
		{Name: "kiac-dev-control-plane", Image: "docker.io/kindest/node:v1.36.1@sha256:abc", Status: "running", IP: "192.168.64.2", Created: "2026-06-11T10:00:00Z"},
		{Name: "kiac-dev-worker-2", Image: "docker.io/kindest/node:v1.36.1@sha256:abc", Status: "running", IP: "192.168.64.4", Created: "2026-06-11T10:00:07Z"},
		{Name: "kiac-old-control-plane", Image: "docker.io/kindest/node:v1.34.8@sha256:def", Status: "stopped", Created: "2026-01-02T09:00:00Z"},
		{Name: "kiac-mixed-control-plane", Image: "docker.io/kindest/node:v1.35.5@sha256:eee", Status: "running"},
		{Name: "kiac-mixed-worker-1", Image: "docker.io/kindest/node:v1.35.5@sha256:eee", Status: "stopped"},
		{Name: "unrelated-container", Image: "nginx:1.27.0", Status: "running"},
	}

	got := BuildStatuses(infos)
	if len(got) != 3 {
		t.Fatalf("expected 3 clusters, got %d: %+v", len(got), got)
	}

	cases := []struct {
		name       string
		status     string
		running    int
		total      int
		k8sVersion string
		created    string
	}{
		{name: "dev", status: "3/3 running", running: 3, total: 3, k8sVersion: "v1.36.1", created: "2026-06-11T10:00:00Z"},
		{name: "mixed", status: "1/2 running", running: 1, total: 2, k8sVersion: "v1.35.5", created: ""},
		{name: "old", status: "0/1 stopped", running: 0, total: 1, k8sVersion: "v1.34.8", created: "2026-01-02T09:00:00Z"},
	}
	for i, c := range cases {
		s := got[i]
		if s.Name != c.name || s.Status != c.status || s.Running != c.running || s.Total != c.total {
			t.Errorf("cluster %d = %q %q %d/%d, want %q %q %d/%d", i, s.Name, s.Status, s.Running, s.Total, c.name, c.status, c.running, c.total)
		}
		if s.K8sVersion != c.k8sVersion {
			t.Errorf("cluster %q k8s version = %q, want %q", c.name, s.K8sVersion, c.k8sVersion)
		}
		if s.Created != c.created {
			t.Errorf("cluster %q created = %q, want %q", c.name, s.Created, c.created)
		}
	}

	dev := got[0]
	if len(dev.Nodes) != 3 || dev.Nodes[0].Name != "kiac-dev-worker-1" || dev.Nodes[0].State != "running" || dev.Nodes[0].IP != "192.168.64.3" {
		t.Errorf("dev nodes wrong: %+v", dev.Nodes)
	}

	// The JSON schema is a contract for -o json: keys must stay stable.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"name"`, `"status"`, `"runningNodes"`, `"totalNodes"`, `"k8sVersion"`, `"nodes"`, `"state"`, `"ip"`, `"image"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON output missing key %s:\n%s", key, raw)
		}
	}
}

func TestBuildStatusesEmpty(t *testing.T) {
	if got := BuildStatuses(nil); len(got) != 0 {
		t.Errorf("expected no clusters, got %+v", got)
	}
}

func TestClusterNameFromNode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "kiac-dev-control-plane", want: "dev", ok: true},
		{in: "kiac-dev-worker-12", want: "dev", ok: true},
		{in: "kiac-my-app-worker-1", want: "my-app", ok: true},
		{in: "kiac-my-app-control-plane", want: "my-app", ok: true},
		{in: "other-dev-control-plane", ok: false},
		{in: "kiac-strayvm", ok: false},
		{in: "", ok: false},
	}
	for _, c := range cases {
		got, ok := clusterNameFromNode(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("clusterNameFromNode(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestK8sVersionFromImage(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "docker.io/kindest/node:v1.36.1@sha256:3489c7", want: "v1.36.1"},
		{in: "kindest/node:v1.34.8", want: "v1.34.8"},
		{in: "kindest/node:1.34.8", want: "1.34.8"},
		{in: "localhost:5000/custom/node", want: ""},
		{in: "kindest/node:latest", want: ""},
		{in: "kindest/node", want: ""},
		{in: "", want: ""},
	}
	for _, c := range cases {
		if got := K8sVersionFromImage(c.in); got != c.want {
			t.Errorf("K8sVersionFromImage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatCreated(t *testing.T) {
	if got := FormatCreated("not-a-timestamp"); got != "not-a-timestamp" {
		t.Errorf("non-RFC3339 input should pass through, got %q", got)
	}
	if got := FormatCreated(""); got != "" {
		t.Errorf("empty input should stay empty, got %q", got)
	}
	got := FormatCreated("2026-06-11T10:00:00Z")
	if !strings.HasPrefix(got, "2026-06-11") && !strings.HasPrefix(got, "2026-06-1") {
		t.Errorf("FormatCreated(RFC3339) = %q, want a local 2006-01-02 15:04 rendering", got)
	}
}
