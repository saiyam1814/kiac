package cluster

import (
	"strings"
	"testing"
)

func TestGPUConfigForDistroOwnsDistroAndDriverDefaults(t *testing.T) {
	k3s := gpuConfigForDistro(Config{Distro: "kubeadm"}, "k3s")
	if k3s.Distro != "k3s" || k3s.GPUDriver != "device-plugin" {
		t.Fatalf("normalized K3s config = %+v", k3s)
	}
	kubeadm := gpuConfigForDistro(Config{Distro: "k3s", GPUDriver: "dra"}, "kubeadm")
	if kubeadm.Distro != "kubeadm" || kubeadm.GPUDriver != "dra" {
		t.Fatalf("normalized kubeadm config = %+v", kubeadm)
	}
}

func TestValidateNVIDIARuntimeHandler(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler string
		distro  string
		wantErr bool
	}{
		{name: "missing kubeadm class", distro: "kubeadm"},
		{name: "portable runc class", handler: "runc", distro: "kubeadm"},
		{name: "k3s discovered wrapper", handler: "nvidia", distro: "k3s"},
		{name: "unconfigured kubeadm nvidia handler", handler: "nvidia", distro: "kubeadm", wantErr: true},
		{name: "unknown handler", handler: "kata", distro: "k3s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNVIDIARuntimeHandler(tc.handler, tc.distro)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateNVIDIARuntimeHandler(%q, %q) error = %v, wantErr %v", tc.handler, tc.distro, err, tc.wantErr)
			}
		})
	}
}

func TestIsGPUNodeRequiresCanonicalPositiveIndex(t *testing.T) {
	for _, name := range []string{"kiac-dev-gpu-1", "kiac-my-app-gpu-12"} {
		if !isGPUNode(name) {
			t.Errorf("%q should be a GPU node", name)
		}
	}
	for _, name := range []string{"kiac-dev-gpu-", "kiac-dev-gpu-0", "kiac-dev-gpu-01", "kiac-dev-gpu-a"} {
		if isGPUNode(name) {
			t.Errorf("%q should not be a GPU node", name)
		}
	}
}

func TestK3sGPUUnitUsesBoundedKiacReadiness(t *testing.T) {
	for _, role := range []string{"server", "agent"} {
		unit := k3sGPUUnit(role)
		if !strings.Contains(unit, "Type=exec") {
			t.Errorf("%s unit does not use Type=exec", role)
		}
		if strings.Contains(unit, "Type=notify") {
			t.Errorf("%s unit can wait indefinitely for sd_notify", role)
		}
		if !strings.Contains(unit, "ExecStart=/usr/local/bin/k3s "+role) {
			t.Errorf("%s unit has the wrong ExecStart", role)
		}
	}
}
