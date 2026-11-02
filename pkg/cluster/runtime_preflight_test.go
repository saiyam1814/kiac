package cluster

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

type versionPreflightRuntime struct {
	runtime.HostRuntime
	version    string
	versionErr error
	events     []string
}

func (r *versionPreflightRuntime) List(string) ([]runtime.Info, error) {
	r.events = append(r.events, "list")
	return nil, nil
}
func (r *versionPreflightRuntime) Available() bool {
	r.events = append(r.events, "available")
	return true
}
func (r *versionPreflightRuntime) Version() (string, error) {
	r.events = append(r.events, "version")
	return r.version, r.versionErr
}
func (r *versionPreflightRuntime) SystemStart(bool) error {
	r.events = append(r.events, "system start")
	return nil
}
func (r *versionPreflightRuntime) ImagePull(string) error {
	r.events = append(r.events, "image pull")
	return errors.New("stop after checked pull")
}

func TestCreateChecksRuntimeBeforeServiceStartOrImagePull(t *testing.T) {
	for _, distro := range []string{"kubeadm", "k3s"} {
		for _, tc := range []struct {
			name, version string
			versionErr    error
			allowPull     bool
		}{
			{name: "old recommended", version: "1.4.1"},
			{name: "last vulnerable minor", version: "1.5.0"},
			{name: "boundary prerelease", version: "1.6.0-rc.1"},
			{name: "unknown", version: "unknown"},
			{name: "unreadable", versionErr: errors.New("version probe failed")},
			{name: "patched boundary", version: "1.6.0", allowPull: true},
			{name: "recommended", version: "1.6.1", allowPull: true},
		} {
			t.Run(distro+"/"+tc.name, func(t *testing.T) {
				rt := &versionPreflightRuntime{version: tc.version, versionErr: tc.versionErr}
				m := &Manager{rt: rt}
				cfg := Config{Name: "preflight", Image: "example.test/node@sha256:" + strings.Repeat("a", 64)}
				var err error
				if distro == "k3s" {
					err = m.CreateK3s(cfg)
				} else {
					err = m.Create(cfg)
				}
				if err == nil {
					t.Fatal("expected version rejection or checked-pull sentinel")
				}
				want := []string{"list", "available", "version"}
				if tc.allowPull {
					want = append(want, "system start", "image pull")
					if err.Error() != "stop after checked pull" {
						t.Fatalf("patched runtime failed before pull: %v", err)
					}
				}
				if !reflect.DeepEqual(rt.events, want) {
					t.Fatalf("runtime calls = %v, want %v; error = %v", rt.events, want, err)
				}
			})
		}
	}
}

func TestGPUCreateDoesNotRequireAppleContainerVersion(t *testing.T) {
	for _, distro := range []string{"kubeadm", "k3s"} {
		t.Run(distro, func(t *testing.T) {
			// An absent Apple CLI and nil host runtime would fail or panic if
			// ordinary-runtime preflight leaked into either GPU create path.
			m := &Manager{
				container: &runtime.Client{Bin: filepath.Join(t.TempDir(), "no-container")},
				krunkit:   &runtime.KrunkitClient{Bin: filepath.Join(t.TempDir(), "no-krunkit"), RootDir: t.TempDir()},
			}
			cfg := Config{Name: "gpu-preflight", GPUWorkers: 1}
			var err error
			if distro == "k3s" {
				err = m.CreateK3s(cfg)
			} else {
				err = m.Create(cfg)
			}
			if err == nil || (!strings.Contains(err.Error(), "krunkit is not installed") && !strings.Contains(err.Error(), "real Apple GPU nodes require macOS on Apple silicon")) {
				t.Fatalf("GPU create error = %v, want independent GPU preflight rejection", err)
			}
		})
	}
}
