package cluster

import (
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestAPIServerIP(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "admin.conf server line",
			in:   "clusters:\n- cluster:\n    server: https://192.168.64.79:6443\n  name: kubernetes\n",
			want: "192.168.64.79",
		},
		{
			name: "compact yaml",
			in:   `{"server": "https://10.0.0.5:6443"}`,
			want: "10.0.0.5",
		},
		{name: "hostname endpoint is not an IP", in: "server: https://kiac-dev-control-plane:6443", wantErr: true},
		{name: "wrong port ignored", in: "server: https://192.168.64.79:8443", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := apiServerIP(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("apiServerIP(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("apiServerIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOrderNodes(t *testing.T) {
	infos := []runtime.Info{
		{Name: "kiac-dev-worker-10"},
		{Name: "kiac-dev-worker-2"},
		{Name: "kiac-dev-control-plane"},
		{Name: "kiac-dev-worker-1"},
		{Name: "kiac-dev-worker-x"}, // not a kiac node shape; ignored
	}
	cp, workers, err := orderNodes("dev", infos)
	if err != nil {
		t.Fatal(err)
	}
	if cp != "kiac-dev-control-plane" {
		t.Errorf("cp = %q", cp)
	}
	want := []string{"kiac-dev-worker-1", "kiac-dev-worker-2", "kiac-dev-worker-10"}
	if strings.Join(workers, ",") != strings.Join(want, ",") {
		t.Errorf("workers = %v, want %v (numeric order, not lexical)", workers, want)
	}
}

func TestOrderNodesMissingControlPlane(t *testing.T) {
	_, _, err := orderNodes("dev", []runtime.Info{{Name: "kiac-dev-worker-1"}})
	if err == nil || !strings.Contains(err.Error(), "control-plane") {
		t.Fatalf("err = %v, want missing control-plane error", err)
	}
}

func TestHealControlPlaneScript(t *testing.T) {
	s := healControlPlaneScript("192.168.64.79", "192.168.64.86")

	// The sed pattern must escape dots so 192.168.64.7 never matches
	// inside 192.168.64.79, and be word-bounded against suffix matches.
	if !strings.Contains(s, `s#\b192\.168\.64\.79\b#192.168.64.86#g`) {
		t.Errorf("script lacks the escaped, word-bounded sed:\n%s", s)
	}
	// admin.conf and super-admin.conf are the files the node entrypoint
	// never fixes; their presence here is the point of the script.
	for _, f := range []string{
		"/etc/kubernetes/admin.conf",
		"/etc/kubernetes/super-admin.conf",
		"/etc/kubernetes/manifests/etcd.yaml",
		"/etc/kubernetes/manifests/kube-apiserver.yaml",
		"/var/lib/kubelet/kubeadm-flags.env",
	} {
		if !strings.Contains(s, f) {
			t.Errorf("script does not rewrite %s", f)
		}
	}
	// Cert regen must not depend on /kind/kubeadm.conf: kiac's kubeadm
	// init never wrote it (that is what kills the first boot).
	if strings.Contains(s, "/kind/kubeadm.conf") {
		t.Errorf("script must not reference /kind/kubeadm.conf:\n%s", s)
	}
	if !strings.Contains(s, "kubeadm init phase certs apiserver --apiserver-advertise-address 192.168.64.86") {
		t.Errorf("script lacks flag-driven cert regeneration:\n%s", s)
	}
	if !strings.Contains(s, "systemctl restart kubelet") {
		t.Errorf("script never restarts the kubelet:\n%s", s)
	}
	// kiac-lb restart must be guarded: --no-lb clusters have no unit.
	if !strings.Contains(s, "systemctl is-enabled kiac-lb.service") {
		t.Errorf("script lacks the guarded kiac-lb restart:\n%s", s)
	}
}

func TestHealWorkerScript(t *testing.T) {
	s := healWorkerScript("192.168.64.86")
	// The guard keeps the script idempotent; the sed matches any stale
	// address so a half-finished earlier resume stays repairable.
	if !strings.Contains(s, "grep -q 'server: https://192.168.64.86:6443'") {
		t.Errorf("script lacks the already-healed guard:\n%s", s)
	}
	if !strings.Contains(s, `s#(server: https://)[0-9.]+(:6443)#\1192.168.64.86\2#`) {
		t.Errorf("script lacks the any-old-IP sed:\n%s", s)
	}
	if !strings.Contains(s, "systemctl restart kubelet") {
		t.Errorf("script never restarts the kubelet:\n%s", s)
	}
}

func TestHealConfigMapsScript(t *testing.T) {
	s := healConfigMapsScript("192.168.64.86")
	for _, want := range []string{
		"-n kube-system get configmap kube-proxy",
		"rollout restart daemonset kube-proxy",
		"-n kube-public get configmap cluster-info",
		"https://192.168.64.86:6443",
		"--kubeconfig /etc/kubernetes/admin.conf",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}
