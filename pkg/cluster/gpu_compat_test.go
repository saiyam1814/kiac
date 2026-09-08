package cluster

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestGPUCompatTLSAndManifests(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	data, err := newGPUCompatTLS(now)
	if err != nil {
		t.Fatal(err)
	}
	data.ControlPlane = "kiac-dev-control-plane"
	caPEM, err := base64.StdEncoding.DecodeString(data.CABundle)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("CA bundle is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA {
		t.Fatal("generated signer is not a CA")
	}
	if !gpuCompatTLSHealthy(data.ServerCert, data.CABundle, now) {
		t.Fatal("fresh serving certificate should be healthy")
	}
	if gpuCompatTLSHealthy(data.ServerCert, data.CABundle, now.AddDate(1, 0, 0).Add(-15*24*time.Hour)) {
		t.Fatal("serving certificate inside the renewal window should be unhealthy")
	}
	other, err := newGPUCompatTLS(now)
	if err != nil {
		t.Fatal(err)
	}
	if gpuCompatTLSHealthy(data.ServerCert, other.CABundle, now) {
		t.Fatal("serving certificate signed by another CA should be unhealthy")
	}
	if gpuCompatTLSHealthy("not-base64", data.CABundle, now) {
		t.Fatal("malformed serving certificate should be unhealthy")
	}

	core, err := renderGPUCompatManifest("core", gpuCompatCoreManifest, data)
	if err != nil {
		t.Fatal(err)
	}
	webhook, err := renderGPUCompatManifest("webhook", gpuCompatWebhookManifest, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nodeName: kiac-dev-control-plane", "runAsNonRoot: true", data.TLSID} {
		if !strings.Contains(core, want) {
			t.Errorf("core manifest is missing %q", want)
		}
	}
	for _, want := range []string{"failurePolicy: Fail", "kiac.dev/gpu-compat: nvidia", data.CABundle} {
		if !strings.Contains(webhook, want) {
			t.Errorf("webhook manifest is missing %q", want)
		}
	}
}

func TestValidNamespaceName(t *testing.T) {
	for _, name := range []string{"default", "gpu-demo", "a"} {
		if !validNamespaceName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range []string{"", "UPPER", "-gpu", "gpu-", strings.Repeat("a", 64)} {
		if validNamespaceName(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}
