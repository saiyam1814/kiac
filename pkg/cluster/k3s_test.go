package cluster

import (
	"strings"
	"testing"
)

func TestResolveK3sImage(t *testing.T) {
	cases := []struct {
		in      string
		want    string // substring of resolved image
		wantErr bool
	}{
		{in: "1.36", want: "rancher/k3s:v1.36.2-k3s1@sha256:"},
		{in: "v1.36", want: "rancher/k3s:v1.36.2-k3s1@sha256:"},
		{in: "1.32", want: "rancher/k3s:v1.32.13-k3s1@sha256:"},
		{in: "1.34.9", want: "rancher/k3s:v1.34.9-k3s1@sha256:"},
		{in: "1.34.2", want: "rancher/k3s:v1.34.2-k3s1"}, // unpinned patch fallback
		{in: "1.19", wantErr: true},
		{in: "latest", wantErr: true},
	}
	for _, c := range cases {
		got, err := ResolveK3sImage(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ResolveK3sImage(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveK3sImage(%q): %v", c.in, err)
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("ResolveK3sImage(%q) = %q, want substring %q", c.in, got, c.want)
		}
	}
	if def := SupportedK3sVersions()[0]; def != DefaultK8sVersion {
		t.Errorf("newest k3s-supported version %q should equal default %q", def, DefaultK8sVersion)
	}
}

// Every pinned k3s image must be fully pinned: registry-qualified,
// digest-pinned, and a real k3s tag.
func TestK3sImagePins(t *testing.T) {
	for minor, img := range k3sImages {
		if !strings.HasPrefix(img, "docker.io/rancher/k3s:v"+minor+".") {
			t.Errorf("k3sImages[%q] = %q: tag does not match the minor", minor, img)
		}
		if !strings.Contains(img, "-k3s") {
			t.Errorf("k3sImages[%q] = %q: missing -k3sN tag suffix", minor, img)
		}
		if !strings.Contains(img, "@sha256:") {
			t.Errorf("k3sImages[%q] = %q: not digest-pinned", minor, img)
		}
	}
}

func TestK3sServerArgs(t *testing.T) {
	args := k3sServerArgs(Config{}, "kiac-dev-control-plane")
	if args[0] != "server" {
		t.Fatalf("first arg = %q, want server", args[0])
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --flannel-backend=none ",   // kiac applies kindnet instead (no br_netfilter in the kernel)
		" --disable-network-policy ", // k3s netpol controller targets the flannel bridge
		" --tls-san kiac-dev-control-plane ",
		" --node-name kiac-dev-control-plane ",
		" --disable=traefik ", // never fight --gateway Traefik for 80/443
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("server args %q missing %q", joined, want)
		}
	}
	for _, banned := range []string{"servicelb", "metrics-server", "local-storage"} {
		if strings.Contains(joined, banned) {
			t.Errorf("default server args must not disable %s: %q", banned, joined)
		}
	}

	all := k3sServerArgs(Config{NoMetrics: true, NoStorage: true, NoLB: true}, "cp")
	joined = strings.Join(all, " ")
	for _, want := range []string{"--disable=metrics-server", "--disable=local-storage", "--disable=servicelb"} {
		if !strings.Contains(joined, want) {
			t.Errorf("No* server args %q missing %q", joined, want)
		}
	}
}

func TestK3sAgentArgsAndEnv(t *testing.T) {
	args := k3sAgentArgs("kiac-dev-worker-1")
	if len(args) != 3 || args[0] != "agent" || args[1] != "--node-name" || args[2] != "kiac-dev-worker-1" {
		t.Errorf("agent args = %q", args)
	}
	env := k3sAgentEnv("192.168.64.5", "tok123")
	if len(env) != 2 || env[0] != "K3S_URL=https://192.168.64.5:6443" || env[1] != "K3S_TOKEN=tok123" {
		t.Errorf("agent env = %q", env)
	}
}

func TestK3sToken(t *testing.T) {
	a, err := k3sToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := k3sToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || a == b {
		t.Errorf("tokens should be 32 hex chars and unique, got %q, %q", a, b)
	}
}

func TestK3sNodesReady(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
		ok   bool
	}{
		{name: "empty output", out: "", want: 1, ok: false},
		{name: "one ready", out: "kiac-dev-control-plane   Ready   control-plane,master   1m   v1.36.2+k3s1\n", want: 1, ok: true},
		{name: "not ready", out: "kiac-dev-control-plane   NotReady   control-plane,master   1m   v1.36.2+k3s1\n", want: 1, ok: false},
		{name: "waiting for second node", out: "kiac-dev-control-plane   Ready   control-plane,master   1m   v1.36.2+k3s1\n", want: 2, ok: false},
		{
			name: "server ready worker not",
			out:  "kiac-dev-control-plane   Ready   control-plane,master   2m   v1.36.2+k3s1\nkiac-dev-worker-1   NotReady   <none>   1s   v1.36.2+k3s1\n",
			want: 2, ok: false,
		},
		{
			name: "all ready",
			out:  "kiac-dev-control-plane   Ready   control-plane,master   2m   v1.36.2+k3s1\nkiac-dev-worker-1   Ready   <none>   30s   v1.36.2+k3s1\n",
			want: 2, ok: true,
		},
		{name: "cordoned still ready", out: "n1   Ready,SchedulingDisabled   <none>   1m   v1.36.2+k3s1\n", want: 1, ok: true},
		{name: "extra nodes count", out: "n1   Ready   <none>   1m   x\nn2   Ready   <none>   1m   x\n", want: 1, ok: true},
	}
	for _, c := range cases {
		if got := k3sNodesReady(c.out, c.want); got != c.ok {
			t.Errorf("%s: k3sNodesReady(want=%d) = %v, want %v", c.name, c.want, got, c.ok)
		}
	}
}
