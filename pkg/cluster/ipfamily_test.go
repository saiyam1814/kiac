package cluster

import "testing"

func TestIPFamilyValid(t *testing.T) {
	for _, f := range []IPFamily{IPv4, DualStack, IPv6} {
		if !f.Valid() {
			t.Errorf("%q should be valid", f)
		}
	}
	for _, f := range []IPFamily{"", "v4", "both", "IPV6"} {
		if IPFamily(f).Valid() {
			t.Errorf("%q should be invalid", f)
		}
	}
}

func TestConfigFamilyDefaultsToIPv4(t *testing.T) {
	// A zero Config must behave exactly as before the feature existed.
	if got := (Config{}).family(); got != IPv4 {
		t.Errorf("empty Config family = %q, want ipv4", got)
	}
	if (Config{}).family().WantsIPv6() {
		t.Error("empty Config must not want IPv6")
	}
}

func TestIPFamilyCIDR(t *testing.T) {
	tests := []struct {
		family      IPFamily
		wantPod     string
		wantService string
	}{
		{IPv4, kubeadmPodCIDRv4, kubeadmServiceCIDRv4},
		{DualStack, kubeadmPodCIDRv4 + "," + kubeadmPodCIDRv6, kubeadmServiceCIDRv4 + "," + kubeadmServiceCIDRv6},
		{IPv6, kubeadmPodCIDRv6, kubeadmServiceCIDRv6},
	}
	for _, tt := range tests {
		if got := tt.family.podCIDR(kubeadmPodCIDRv4, kubeadmPodCIDRv6); got != tt.wantPod {
			t.Errorf("%s podCIDR = %q, want %q", tt.family, got, tt.wantPod)
		}
		if got := tt.family.serviceCIDR(kubeadmServiceCIDRv4, kubeadmServiceCIDRv6); got != tt.wantService {
			t.Errorf("%s serviceCIDR = %q, want %q", tt.family, got, tt.wantService)
		}
	}
}

func TestValidateIPFamily(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"ipv4 no kernel ok", Config{IPFamily: IPv4}, false},
		{"empty defaults ipv4 ok", Config{}, false},
		{"dual without kernel fails", Config{IPFamily: DualStack}, true},
		{"dual with kernel ok", Config{IPFamily: DualStack, Kernel: "/x"}, false},
		{"ipv6 with kernel ok", Config{IPFamily: IPv6, Kernel: "/x"}, false},
		{"dual with cilium fails", Config{IPFamily: DualStack, Kernel: "/x", CNI: "cilium"}, true},
		{"bogus family fails", Config{IPFamily: "nope"}, true},
	}
	for _, tt := range tests {
		err := validateIPFamily(tt.cfg)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: validateIPFamily err = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestPinnedFamily(t *testing.T) {
	tests := []struct {
		in      string
		wantFam IPFamily
		wantPin bool
	}{
		{"", "", false},
		{"KUBELET_EXTRA_ARGS=--foo=bar\n", "", false},
		{"KUBELET_EXTRA_ARGS=--node-ip=10.0.0.1\n", "", false}, // bare v4, not our pinning
		{"KUBELET_EXTRA_ARGS=--node-ip=10.0.0.1,fd00::1\n", DualStack, true},
		{"KUBELET_EXTRA_ARGS=--node-ip=fd00::1\n", IPv6, true},
		{"KUBELET_EXTRA_ARGS=--node-ip=10.0.0.1,fd00::1 --other=x\n", DualStack, true},
	}
	for _, tt := range tests {
		fam, ok := pinnedFamily(tt.in)
		if ok != tt.wantPin || fam != tt.wantFam {
			t.Errorf("pinnedFamily(%q) = (%q,%v), want (%q,%v)", tt.in, fam, ok, tt.wantFam, tt.wantPin)
		}
	}
}

func TestK3sServerArgsDualStack(t *testing.T) {
	args := k3sServerArgs(Config{IPFamily: DualStack, Kernel: "/x"}, "cp")
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	for _, want := range []string{
		"--cluster-cidr=" + k3sPodCIDRv4 + "," + k3sPodCIDRv6,
		"--service-cidr=" + k3sServiceCIDRv4 + "," + k3sServiceCIDRv6,
	} {
		if !contains(joined, want) {
			t.Errorf("k3s dual server args missing %q; got %q", want, joined)
		}
	}
	// IPv4 must not gain dual CIDR flags (no regression).
	v4 := k3sServerArgs(Config{}, "cp")
	for _, a := range v4 {
		if contains(a, "fd00:") {
			t.Errorf("ipv4 k3s args must not include IPv6 CIDR, got %q", a)
		}
	}
}

func TestK3sBootInjectsNodeIPOnlyForDual(t *testing.T) {
	_, dualArgs := k3sBoot(Config{IPFamily: DualStack, Kernel: "/x"}, []string{"server"})
	dual := dualArgs[1]
	if !contains(dual, "--node-ip=") || !contains(dual, "accept_ra=2") {
		t.Errorf("dual k3s boot must set node-ip and accept_ra=2; got %q", dual)
	}
	_, v4Args := k3sBoot(Config{}, []string{"server"})
	if contains(v4Args[1], "--node-ip=") {
		t.Errorf("ipv4 k3s boot must not inject --node-ip; got %q", v4Args[1])
	}
}

func TestK3sKindnetManifestFamily(t *testing.T) {
	dual := k3sKindnetManifestFor(DualStack)
	if !contains(dual, k3sPodCIDRv4+","+k3sPodCIDRv6) {
		t.Errorf("dual kindnet manifest must carry the dual POD_SUBNET")
	}
	if k3sKindnetManifestFor(IPv4) != k3sKindnetManifest {
		t.Errorf("ipv4 kindnet manifest must be unchanged")
	}
}

func TestMergeIPFamily(t *testing.T) {
	// File value applies when the flag was not set on the command line.
	cfg := Config{}
	fc := FileConfig{IPFamily: "dual"}
	if err := fc.Merge(&cfg, new(string), func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if cfg.IPFamily != DualStack {
		t.Errorf("merge from file: IPFamily = %q, want dual", cfg.IPFamily)
	}
	// An explicit --ip-family flag wins over the file.
	cfg = Config{IPFamily: IPv6}
	if err := fc.Merge(&cfg, new(string), func(f string) bool { return f == "ip-family" }); err != nil {
		t.Fatal(err)
	}
	if cfg.IPFamily != IPv6 {
		t.Errorf("flag should win over file: IPFamily = %q, want ipv6", cfg.IPFamily)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
