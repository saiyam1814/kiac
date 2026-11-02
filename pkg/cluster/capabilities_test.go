package cluster

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestSupportedClusterCapabilitiesMatchInstalledArtifacts(t *testing.T) {
	capabilities := SupportedClusterCapabilities()
	if len(capabilities) != 4 {
		t.Fatalf("capabilities = %+v, want both distros on both backends", capabilities)
	}
	for _, capability := range capabilities {
		if !slices.Contains(capability.Versions, capability.DefaultVersion) {
			t.Fatalf("%s/%s default %q is not supported: %v", capability.Backend, capability.Distro, capability.DefaultVersion, capability.Versions)
		}
		if capability.Backend == runtime.BackendContainer {
			want := SupportedVersions()
			if capability.Distro == "k3s" {
				want = SupportedK3sVersions()
			}
			if !reflect.DeepEqual(capability.Versions, want) || capability.GPUDrivers != nil {
				t.Fatalf("ordinary %s capabilities = %+v, want versions %v without GPU drivers", capability.Distro, capability, want)
			}
			continue
		}
		for _, version := range capability.Versions {
			if capability.Distro == "kubeadm" {
				if _, ok := kubeadmGPUReleases[version]; !ok {
					t.Fatalf("advertised kubeadm GPU release %q has no binaries", version)
				}
			} else if _, ok := k3sARM64Assets[version]; !ok {
				t.Fatalf("advertised K3s GPU release %q has no binary", version)
			}
			for _, driver := range SupportedGPUResourceDrivers() {
				resolved, err := ResolveGPUVersion(capability.Distro, version, driver)
				advertised := slices.Contains(capability.GPUDrivers[driver], version)
				if advertised != (err == nil) || (advertised && resolved != version) {
					t.Fatalf("%s/%s/%s advertised=%v but resolves to %q, %v", capability.Distro, version, driver, advertised, resolved, err)
				}
			}
		}
		wantCount := len(kubeadmGPUReleases)
		if capability.Distro == "k3s" {
			wantCount = len(k3sARM64Assets)
		}
		if len(capability.Versions) != wantCount {
			t.Fatalf("%s advertises %d of %d pinned GPU releases", capability.Distro, len(capability.Versions), wantCount)
		}
	}
}

func TestResolveGPUVersion(t *testing.T) {
	for _, tc := range []struct {
		name, distro, version, driver, want, wantErr string
	}{
		{name: "kubeadm default", want: "v1.37.0"},
		{name: "kubeadm minor", distro: "kubeadm", version: "1.37", want: "v1.37.0"},
		{name: "kubeadm prefixed minor", distro: "kubeadm", version: "v1.37", driver: "dra", want: "v1.37.0"},
		{name: "kubeadm exact", distro: "kubeadm", version: "1.37.0", want: "v1.37.0"},
		{name: "k3s minor", distro: "k3s", version: "1.36", driver: "dra", want: "v1.36.4-k3s1"},
		{name: "k3s patch", distro: "k3s", version: "1.36.4", want: "v1.36.4-k3s1"},
		{name: "k3s plus revision", distro: "k3s", version: "v1.36.4+k3s1", want: "v1.36.4-k3s1"},
		{name: "k3s older device plugin", distro: "k3s", version: "1.32", want: "v1.32.13-k3s1"},
		{name: "no kubeadm binaries", distro: "kubeadm", version: "1.36", wantErr: "no pinned kubeadm GPU-node binaries"},
		{name: "no kubeadm patch", distro: "kubeadm", version: "1.37.1", wantErr: "no pinned kubeadm GPU-node binaries"},
		{name: "no k3s patch", distro: "k3s", version: "1.36.5", wantErr: "no pinned k3s GPU-node binaries"},
		{name: "no k3s revision", distro: "k3s", version: "1.36.4+k3s2", wantErr: "no pinned k3s GPU-node binaries"},
		{name: "prerelease", distro: "kubeadm", version: "1.37.0-rc.1", wantErr: "no pinned kubeadm GPU-node binaries"},
		{name: "DRA too old", distro: "k3s", version: "1.35", driver: "dra", wantErr: "requires Kubernetes 1.36 or newer"},
		{name: "unknown driver", distro: "kubeadm", driver: "fake", wantErr: "unknown --gpu-resource-driver"},
		{name: "unknown distro", distro: "fake", wantErr: "unknown GPU cluster distro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveGPUVersion(tc.distro, tc.version, tc.driver)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ResolveGPUVersion error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("ResolveGPUVersion = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestGPUCreateRejectsImpossibleVersionsBeforeBackendAccess(t *testing.T) {
	for _, tc := range []struct {
		name, distro, version, driver, wantErr string
	}{
		{name: "kubeadm old minor", distro: "kubeadm", version: "1.36", wantErr: "no pinned kubeadm GPU-node binaries"},
		{name: "k3s missing patch", distro: "k3s", version: "v1.37.99-k3s1", wantErr: "no pinned k3s GPU-node binaries"},
		{name: "k3s old DRA", distro: "k3s", version: "1.35", driver: "dra", wantErr: "requires Kubernetes 1.36 or newer"},
		{name: "invalid driver", distro: "kubeadm", version: "1.37", driver: "fake", wantErr: "unknown --gpu-resource-driver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			// No backend or host client exists: touching inventory, host
			// preflight, or VM operations would panic instead of succeeding.
			m := &Manager{}
			cfg := Config{Name: "gpu", GPUWorkers: 1, K8sVersion: tc.version, GPUDriver: tc.driver, GPUImage: "missing.raw"}
			var err error
			if tc.distro == "k3s" {
				err = m.CreateK3s(cfg)
			} else {
				err = m.Create(cfg)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("create error = %v, want %q", err, tc.wantErr)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid create modified the host: %v, %v", entries, err)
			}
		})
	}
}

func TestCapabilityVersionsSortNumerically(t *testing.T) {
	versions := []string{"v1.9.9-k3s2", "v1.37.0-k3s1", "v1.9.10-k3s2", "v1.37.0-k3s10", "v1.37.0-k3s2"}
	slices.SortFunc(versions, compareCapabilityVersions)
	want := []string{"v1.37.0-k3s10", "v1.37.0-k3s2", "v1.37.0-k3s1", "v1.9.10-k3s2", "v1.9.9-k3s2"}
	if !reflect.DeepEqual(versions, want) {
		t.Fatalf("sorted versions = %v, want %v", versions, want)
	}
}
