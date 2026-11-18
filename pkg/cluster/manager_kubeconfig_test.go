package cluster

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// Embedding an unset HostRuntime makes any unexpected lifecycle or host
// operation fail the test; exporting needs only inventory, cat, and one IP.
type kubeconfigTestRuntime struct {
	runtime.HostRuntime
	infos                   []runtime.Info
	config, configPath      string
	v4, v6                  string
	listErr, execErr, ipErr error
	calls                   []string
}

func (r *kubeconfigTestRuntime) List(prefix string) ([]runtime.Info, error) {
	r.calls = append(r.calls, "list "+prefix)
	var result []runtime.Info
	for _, info := range r.infos {
		if strings.HasPrefix(info.Name, prefix) {
			result = append(result, info)
		}
	}
	return result, r.listErr
}

func (r *kubeconfigTestRuntime) Exec(name string, command ...string) (string, error) {
	r.calls = append(r.calls, "exec "+name+" "+strings.Join(command, " "))
	if name != ControlPlane("dev") || len(command) != 2 || command[0] != "cat" || command[1] != r.configPath {
		return "", fmt.Errorf("unexpected guest command on %s: %v", name, command)
	}
	return r.config, r.execErr
}

func (r *kubeconfigTestRuntime) IP(name string) (string, error) {
	r.calls = append(r.calls, "ipv4 "+name)
	return r.v4, r.ipErr
}

func (r *kubeconfigTestRuntime) IPv6(name string) (string, error) {
	r.calls = append(r.calls, "ipv6 "+name)
	return r.v6, r.ipErr
}

func TestManagerKubeconfigSelectsExactControlPlaneAndCurrentFamily(t *testing.T) {
	for _, tc := range []struct {
		name, image, distro, source, current, wantPath, family string
		backend                                                string
		overlap                                                bool
	}{
		{name: "kubeadm IPv4", image: "docker.io/kindest/node:v1.37.1", source: "https://192.168.64.10:6443", current: "192.168.64.20", wantPath: adminConf, family: "ipv4"},
		{name: "kubeadm with overlapping K3s cluster", image: "docker.io/kindest/node:v1.37.1", source: "https://192.168.64.10:6443", current: "192.168.64.20", wantPath: adminConf, family: "ipv4", overlap: true},
		{name: "kubeadm IPv6", image: "docker.io/kindest/node:v1.37.1", source: "https://[fd00::10]:6443", current: "fd00::20", wantPath: adminConf, family: "ipv6"},
		{name: "kubeadm dual uses IPv4", image: "docker.io/kindest/node:v1.37.1", source: "https://192.168.64.10:6443", current: "192.168.64.20", wantPath: adminConf, family: "ipv4"},
		{name: "k3s image", image: "docker.io/rancher/k3s:v1.37.1-k3s1", source: "https://127.0.0.1:6443", current: "192.168.64.20", wantPath: k3sKubeconfig, family: "ipv4"},
		{name: "k3s dual uses IPv4", image: "docker.io/rancher/k3s:v1.37.1-k3s1", source: "https://127.0.0.1:6443", current: "192.168.64.20", wantPath: k3sKubeconfig, family: "ipv4"},
		{name: "krunkit K3s metadata", image: "local/gpu-node", distro: "k3s", backend: runtime.BackendKrunkit, source: "https://127.0.0.1:6443", current: "192.168.64.20", wantPath: k3sKubeconfig, family: "ipv4"},
		{name: "krunkit kubeadm metadata", image: "local/gpu-node", distro: "kubeadm", backend: runtime.BackendKrunkit, source: "https://kiac-dev-control-plane:6443", current: "192.168.64.20", wantPath: adminConf, family: "ipv4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			const existing = "existing user kubeconfig must remain byte-for-byte unchanged\n"
			if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBECONFIG", path)
			config := strings.Replace(sampleAdminConf, "https://kiac-dev-control-plane:6443", tc.source, 1)
			infos := []runtime.Info{{Name: ControlPlane("dev"), Image: tc.image, Distro: tc.distro, Backend: tc.backend}}
			if tc.overlap {
				// A foreign K3s cluster sharing dev's prefix must never choose
				// the source path, even when it appears first in inventory.
				infos = append([]runtime.Info{{Name: ControlPlane("dev-extra"), Image: "docker.io/rancher/k3s:v1.37.1-k3s1", Distro: "k3s"}}, infos...)
			}
			rt := &kubeconfigTestRuntime{
				infos:  infos,
				config: config, configPath: tc.wantPath, v4: "192.168.64.20", v6: "fd00::20",
			}
			got, err := (&Manager{rt: rt}).Kubeconfig("dev")
			if err != nil {
				t.Fatal(err)
			}
			server := "https://" + tc.current + ":6443"
			if tc.family == "ipv6" {
				server = "https://[" + tc.current + "]:6443"
			}
			for _, want := range []string{"server: " + server, "current-context: kiac-dev", "client-key-data: Zm9v"} {
				if !strings.Contains(got, want) {
					t.Errorf("export missing %q", want)
				}
			}
			if strings.Contains(got, tc.source) || strings.Contains(got, "dev-extra") {
				t.Error("export retained the stale endpoint or foreign cluster")
			}
			wantCalls := []string{"list kiac-dev-", "exec " + ControlPlane("dev") + " cat " + tc.wantPath, tc.family + " " + ControlPlane("dev")}
			if fmt.Sprint(rt.calls) != fmt.Sprint(wantCalls) {
				t.Errorf("operations = %v, want %v", rt.calls, wantCalls)
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != existing {
				t.Errorf("export changed KUBECONFIG: %v", err)
			}
		})
	}
}

func TestManagerKubeconfigDoesNotCreateKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config")
	t.Setenv("KUBECONFIG", path)
	rt := &kubeconfigTestRuntime{
		infos:  []runtime.Info{{Name: ControlPlane("dev"), Distro: "kubeadm"}},
		config: sampleAdminConf, configPath: adminConf, v4: "192.168.64.20",
	}
	if _, err := (&Manager{rt: rt}).Kubeconfig("dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("export created kubeconfig directory: %v", err)
	}
}

func TestManagerKubeconfigRequiresExactControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name, wantErr string
		infos         []runtime.Info
	}{
		{name: "no cluster", wantErr: `no cluster named "dev" found`},
		{name: "only overlapping cluster", infos: []runtime.Info{{Name: ControlPlane("dev-extra"), Distro: "k3s"}}, wantErr: "has no control-plane container"},
		{name: "only worker", infos: []runtime.Info{{Name: worker("dev", 1)}}, wantErr: "has no control-plane container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &kubeconfigTestRuntime{infos: tc.infos}
			got, err := (&Manager{rt: rt}).Kubeconfig("dev")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Fatalf("export = %q, error = %v, want %q", got, err, tc.wantErr)
			}
			if len(rt.calls) != 1 {
				t.Errorf("export accessed a guest without its control plane: %v", rt.calls)
			}
		})
	}
}

func TestManagerKubeconfigRejectsUnavailableEndpoints(t *testing.T) {
	failure := errors.New("runtime read failed")
	for _, tc := range []struct {
		name, source, address, wantErr string
		listErr, execErr, ipErr        error
	}{
		{name: "inventory error", listErr: failure, wantErr: failure.Error()},
		{name: "guest read error", execErr: failure, wantErr: failure.Error()},
		{name: "address query error", ipErr: failure, wantErr: failure.Error()},
		{name: "missing IPv4", wantErr: "no usable current ipv4 address"},
		{name: "invalid IPv4", address: "999.2.3.4", wantErr: "no usable current ipv4 address"},
		{name: "wrong IPv4 family", address: "fd00::20", wantErr: "no usable current ipv4 address"},
		{name: "unspecified IPv4", address: "0.0.0.0", wantErr: "no usable current ipv4 address"},
		{name: "loopback IPv4", address: "127.0.0.1", wantErr: "no usable current ipv4 address"},
		{name: "missing IPv6", source: "https://[fd00::10]:6443", wantErr: "no usable current ipv6 address"},
		{name: "wrong IPv6 family", source: "https://[fd00::10]:6443", address: "192.168.64.20", wantErr: "no usable current ipv6 address"},
		{name: "unspecified IPv6", source: "https://[fd00::10]:6443", address: "::", wantErr: "no usable current ipv6 address"},
		{name: "loopback IPv6", source: "https://[fd00::10]:6443", address: "::1", wantErr: "no usable current ipv6 address"},
		{name: "missing source server", source: " ", address: "192.168.64.20", wantErr: "invalid API server URL"},
		{name: "invalid source server", source: "https://[broken", address: "192.168.64.20", wantErr: "invalid API server URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			if source == "" {
				source = "https://192.168.64.10:6443"
			}
			rt := &kubeconfigTestRuntime{
				infos:  []runtime.Info{{Name: ControlPlane("dev"), Distro: "kubeadm"}},
				config: strings.Replace(sampleAdminConf, "https://kiac-dev-control-plane:6443", source, 1), configPath: adminConf,
				v4: tc.address, v6: tc.address, listErr: tc.listErr, execErr: tc.execErr, ipErr: tc.ipErr,
			}
			got, err := (&Manager{rt: rt}).Kubeconfig("dev")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Fatalf("export = %q, error = %v, want %q", got, err, tc.wantErr)
			}
			if (tc.listErr != nil || tc.execErr != nil || tc.ipErr != nil) && !errors.Is(err, failure) {
				t.Errorf("export did not preserve runtime error: %v", err)
			}
			if strings.Contains(err.Error(), "Zm9v") {
				t.Error("export error included embedded credentials")
			}
		})
	}
}
