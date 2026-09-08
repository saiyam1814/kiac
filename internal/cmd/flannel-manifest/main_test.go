package main

import (
	"os"
	"strings"
	"testing"
)

const sample = `apiVersion: apps/v1
kind: DaemonSet
spec:
  template:
    spec:
      containers:
      - args:
        - --ip-masq
        - --kube-subnet-mgr
        command:
        - /opt/bin/flanneld
        image: ghcr.io/flannel-io/flannel:v9.9.9
        name: kube-flannel
        resources:
          requests:
            cpu: 100m
      initContainers:
      - args:
        - -f
        image: ghcr.io/flannel-io/flannel-cni-plugin:v1.0.0-flannel1
        name: install-cni-plugin
      - args:
        - -f
        image: ghcr.io/flannel-io/flannel:v9.9.9
        name: install-cni
`

var digests = map[string]string{
	"ghcr.io/flannel-io/flannel:v9.9.9":                     "sha256:" + strings.Repeat("a", 64),
	"ghcr.io/flannel-io/flannel-cni-plugin:v1.0.0-flannel1": "sha256:" + strings.Repeat("b", 64),
}

func TestImages(t *testing.T) {
	got := Images(sample)
	if len(got) != 2 || got[0] != "ghcr.io/flannel-io/flannel:v9.9.9" || got[1] != "ghcr.io/flannel-io/flannel-cni-plugin:v1.0.0-flannel1" {
		t.Fatalf("Images = %v", got)
	}
}

func TestPatchPinsEveryImageAndAddsProbesOnce(t *testing.T) {
	out, err := Patch(sample, digests)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "flannel:v9.9.9@sha256:"+strings.Repeat("a", 64)); n != 2 {
		t.Errorf("flannel image pinned %d times, want 2 (container and init container)", n)
	}
	if !strings.Contains(out, "flannel-cni-plugin:v1.0.0-flannel1@sha256:"+strings.Repeat("b", 64)) {
		t.Error("cni plugin image not pinned")
	}
	for _, want := range []string{
		"        - --kube-subnet-mgr\n        - --healthz-port=8081\n        command:",
		"        livenessProbe:\n          httpGet:\n            path: /healthz",
		"        name: kube-flannel\n        ports:\n        - containerPort: 8081",
		"        readinessProbe:\n          httpGet:\n            path: /readyz",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("patched manifest lacks:\n%s\n---\n%s", want, out)
		}
	}
	if strings.Count(out, "readinessProbe:") != 1 || strings.Count(out, "--healthz-port") != 1 {
		t.Error("probes must be added exactly once, to the kube-flannel container")
	}
	if _, err := Patch(out, digests); err == nil {
		t.Error("patching an already patched manifest must be refused")
	}
}

func TestPatchRefusesUnknownShapes(t *testing.T) {
	if _, err := Patch(strings.Replace(sample, "name: kube-flannel", "name: flanneld", 1), digests); err == nil {
		t.Error("renamed container must be refused")
	}
	if _, err := Patch(sample, map[string]string{"ghcr.io/flannel-io/missing:v1": "sha256:" + strings.Repeat("c", 64)}); err == nil {
		t.Error("digest for an image absent from the manifest must be refused")
	}
	if _, err := Patch(sample, map[string]string{"ghcr.io/flannel-io/flannel:v9.9.9": "latest"}); err == nil {
		t.Error("non-sha256 digest must be refused")
	}
}

func TestCommittedManifestMatchesPatchShape(t *testing.T) {
	// The committed asset must be what this tool produces: every image
	// digest-pinned and the probes present exactly once, so a hand edit
	// that drifts from the tool is caught here rather than at bump time.
	data, err := os.ReadFile("../../../pkg/cluster/assets/flannel.yaml")
	if err != nil {
		t.Skip("committed manifest not found from this directory")
	}
	manifest := string(data)
	for _, image := range Images(manifest) {
		if !strings.Contains(image, "@sha256:") {
			t.Errorf("committed image %s is not digest-pinned", image)
		}
	}
	if _, err := Patch(manifest, nil); err == nil {
		t.Error("committed manifest should already carry the probes, so re-patching must be refused")
	}
}

func TestPatchNormalizesCRLF(t *testing.T) {
	out, err := Patch(strings.ReplaceAll(sample, "\n", "\r\n"), digests)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\r") {
		t.Error("CRLF survived patching")
	}
	if strings.Count(out, "@sha256:") != 3 || strings.Count(out, "readinessProbe:") != 1 {
		t.Errorf("CRLF input was not fully patched:\n%s", out)
	}
}

func TestInputShapes(t *testing.T) {
	for _, ok := range []string{"v0.28.9", "v1.0.0"} {
		if !releaseTag.MatchString(ok) {
			t.Errorf("release tag %q rejected", ok)
		}
	}
	for _, bad := range []string{"", "0.28.9", "v0.28", "v0.28.9/../x", "v0.28.9?x=1", "main"} {
		if releaseTag.MatchString(bad) {
			t.Errorf("release tag %q accepted", bad)
		}
	}
	if m := ghcrImage.FindStringSubmatch("ghcr.io/flannel-io/flannel-cni-plugin:v1.9.1-flannel3"); m == nil || m[1] != "flannel-io/flannel-cni-plugin" || m[2] != "v1.9.1-flannel3" {
		t.Errorf("legitimate image not parsed: %v", m)
	}
	for _, bad := range []string{"docker.io/x/y:v1", "ghcr.io/x/y", "ghcr.io/x/y:v1/../z", "ghcr.io/x/y:v1?token=1", "ghcr.io/../y:v1", "ghcr.io/x/y:v1@sha256:abc"} {
		if ghcrImage.MatchString(bad) {
			t.Errorf("image %q accepted", bad)
		}
	}
}
