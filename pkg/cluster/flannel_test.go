package cluster

import (
	"strings"
	"testing"
	"time"
)

func TestFlannelManifestIsPinnedUpstream(t *testing.T) {
	// The embedded manifest must reference only the pinned release's
	// images, so a version bump cannot half-happen.
	for _, line := range strings.Split(flannelManifest, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "image:") {
			continue
		}
		image := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
		if strings.HasPrefix(image, "ghcr.io/flannel-io/flannel:") && !strings.HasSuffix(image, ":"+FlannelVersion) {
			t.Fatalf("flannel image %q is not pinned to %s", image, FlannelVersion)
		}
		if !strings.Contains(image, ":v") {
			t.Fatalf("image %q is not version-pinned", image)
		}
	}
	if !strings.Contains(flannelManifest, `"Type": "vxlan"`) {
		t.Fatal("flannel manifest no longer configures the vxlan backend")
	}
}

func TestFlannelManifestWithCIDR(t *testing.T) {
	patched, err := flannelManifestWithCIDR(kubeadmPodCIDRv4)
	if err != nil {
		t.Fatalf("flannelManifestWithCIDR: %v", err)
	}
	if !strings.Contains(patched, `"Network": "`+kubeadmPodCIDRv4+`"`) {
		t.Fatalf("patched manifest does not carry the kiac pod CIDR %s", kubeadmPodCIDRv4)
	}
	if strings.Count(patched, `"Network": "`) != 1 {
		t.Fatal("patched manifest carries more than one Network entry")
	}

	patched, err = flannelManifestWithCIDR("10.9.0.0/16")
	if err != nil {
		t.Fatalf("flannelManifestWithCIDR with custom CIDR: %v", err)
	}
	if !strings.Contains(patched, `"Network": "10.9.0.0/16"`) {
		t.Fatal("custom CIDR was not patched into net-conf.json")
	}
	if strings.Contains(patched, flannelUpstreamCIDR) {
		t.Fatal("upstream CIDR survived the patch")
	}
}

func TestFlannelManifestWithCIDRRejectsReshapedManifest(t *testing.T) {
	saved := flannelManifest
	defer func() { flannelManifest = saved }()
	flannelManifest = strings.Replace(saved, flannelUpstreamCIDR, `"Network": "192.168.0.0/16"`, 1)
	if _, err := flannelManifestWithCIDR("10.9.0.0/16"); err == nil {
		t.Fatal("a manifest without the upstream Network marker must be refused, not applied unpatched")
	}
}

func TestFlannelDelegatePluginsAreDeclaredInConflist(t *testing.T) {
	// The install path streams exactly the delegate binaries the
	// conflist needs but the node image lacks. If upstream changes the
	// delegation (a new plugin type, or dropping bridge), this pins the
	// assumption so the bump is reviewed rather than silently broken.
	if !strings.Contains(flannelManifest, `"isDefaultGateway": true`) {
		t.Fatal("flannel conflist no longer delegates to the bridge plugin; update ensureFlannelDelegatePlugins")
	}
	if !strings.Contains(flannelManifest, `"type": "portmap"`) {
		t.Fatal("flannel conflist no longer chains portmap; update ensureFlannelDelegatePlugins")
	}
}

func TestWaitFlannelReadyReturnsWithoutWaitBudget(t *testing.T) {
	// --wait 0 must not block: kubectl rollout status --timeout=0s waits
	// forever, so a non-positive budget returns before touching the
	// runtime (the final nodes-Ready step still runs).
	m := NewManager()
	for _, timeout := range []time.Duration{0, -1 * time.Second} {
		if err := m.waitFlannelReady("kiac-x-control-plane", timeout); err != nil {
			t.Fatalf("waitFlannelReady(%s) = %v, want nil without hitting the runtime", timeout, err)
		}
	}
}

func TestInstallCNIRejectsFlannelWithoutKernel(t *testing.T) {
	m := NewManager()
	err := m.installFlannel("kiac-x-control-plane", Config{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "--kernel full") {
		t.Fatalf("installFlannel without a kernel = %v, want a --kernel full hint", err)
	}
}

func TestInstallCNIErrorsNameFlannel(t *testing.T) {
	m := NewManager()
	if err := m.installCNI("cp", Config{CNI: "calico"}); err == nil || !strings.Contains(err.Error(), "flannel") {
		t.Fatalf("calico rejection should point at flannel as an option, got: %v", err)
	}
	if err := m.installCNI("cp", Config{CNI: "wat"}); err == nil || !strings.Contains(err.Error(), "flannel") {
		t.Fatalf("unknown-CNI error should list flannel as supported, got: %v", err)
	}
}
