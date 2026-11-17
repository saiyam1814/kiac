package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeHTTPTestClient(t *testing.T, server *httptest.Server) *nativeBrokerClient {
	t.Helper()
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "admin.token")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("private-admin-token-do-not-log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := newNativeBrokerClient(server.URL, caFile, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.client.CloseIdleConnections)
	return client
}

func TestNativeBrokerClientUsesAuthenticatedCAValidatedLeaseProtocol(t *testing.T) {
	var acquire, revoke, health int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-admin-token-do-not-log" {
			t.Error("broker request did not use the driver-only admin token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/health":
			health++
			json.NewEncoder(w).Encode(nativeBrokerHealth{Ready: true, Backend: "native-metal", InferenceAvailable: true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			acquire++
			var request nativeLeaseRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Namespace != "work" || request.ClaimUID != "claim-one" || request.NodeName != "node-one" || request.TTLSeconds != 120 {
				t.Error("incorrect lease identity or expiry request")
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(nativeLease{ID: "lease-one", Token: strings.Repeat("x", 32), ExpiresAt: time.Now().Add(120 * time.Second)})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/leases/lease-one":
			revoke++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected broker request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := nativeHTTPTestClient(t, server)
	status, err := client.Health(t.Context())
	if err != nil || !status.InferenceAvailable {
		t.Fatalf("health failed: %v", err)
	}
	lease, err := client.Acquire(t.Context(), nativeLeaseRequest{Namespace: "work", ClaimUID: "claim-one", NodeName: "node-one", TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Revoke(t.Context(), lease.ID); err != nil {
		t.Fatal(err)
	}
	if acquire != 1 || revoke != 1 || health != 1 {
		t.Fatalf("unexpected protocol calls: health=%d acquire=%d revoke=%d", health, acquire, revoke)
	}
}

func TestNativeBrokerClientRejectsRedirectsOversizedBodiesAndSecretEchoes(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"redirect", http.StatusTemporaryRedirect, ""},
		{"oversize", http.StatusOK, strings.Repeat("x", 17<<10)},
		{"error-secret-echo", http.StatusUnauthorized, "private-admin-token-do-not-log"},
		{"bad-json", http.StatusOK, "private-admin-token-do-not-log"},
		{"not-ready", http.StatusOK, `{"ready":false,"backend":"native-metal"}`},
		{"wrong-backend", http.StatusOK, `{"ready":true,"backend":"venus"}`},
		{"accepted-not-complete", http.StatusAccepted, `{"ready":true,"backend":"native-metal"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if test.code == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "/v1/elsewhere")
				}
				w.WriteHeader(test.code)
				w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := nativeHTTPTestClient(t, server)
			_, err := client.Health(t.Context())
			if err == nil || strings.Contains(err.Error(), "private-admin-token-do-not-log") || requests != 1 {
				t.Fatalf("response was trusted, leaked secrets or followed redirect: err=%v requests=%d", err, requests)
			}
		})
	}
}

func TestNativeBrokerCAInjectedIntoPodContainsOnlyPublicCertificates(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "token")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	ca = append(ca, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("a-private-key-must-not-reach-a-pod")})...)
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("a", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, mountedCA, err := newNativeBrokerClient(server.URL, caFile, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	defer client.client.CloseIdleConnections()
	if strings.Contains(string(mountedCA), "PRIVATE KEY") {
		t.Fatal("private PEM data was copied into the workload CA bundle")
	}
	block, rest := pem.Decode(mountedCA)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("mounted CA is not a public certificate bundle")
	}
}

func TestNativeBrokerClientRefusesUntrustedTLSAndCancelledRequests(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nativeBrokerHealth{Ready: true, Backend: "native-metal"})
	}))
	defer server.Close()
	client := nativeHTTPTestClient(t, server)
	// A client that trusts only system roots must not trust the broker's
	// self-signed certificate; there is no insecure verification fallback.
	client.client = &http.Client{Timeout: time.Second}
	if _, err := client.Health(t.Context()); err == nil {
		t.Fatal("untrusted self-signed broker certificate was accepted")
	}
	client = nativeHTTPTestClient(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Health(ctx); err == nil {
		t.Fatal("cancelled broker request succeeded")
	}
}

func TestNativeBrokerOriginAndLeaseResponseValidation(t *testing.T) {
	for _, origin := range []string{"http://localhost:9443", "https://user:secret@localhost", "https://localhost/api", "https://localhost/?token=x", "https://localhost/#x", "https:///missing"} {
		if _, _, err := newNativeBrokerClient(origin, "", ""); err == nil {
			t.Fatalf("unsafe broker origin accepted: %s", origin)
		}
	}
	now := time.Now()
	valid := nativeLease{ID: "lease_1-a", Token: strings.Repeat("x", 32), ExpiresAt: now.Add(120 * time.Second)}
	if err := validateNativeLease(valid, now, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*nativeLease){
		func(l *nativeLease) { l.ID = "../../secrets" },
		func(l *nativeLease) { l.Token = "x\ny" },
		func(l *nativeLease) { l.ExpiresAt = now.Add(-time.Second) },
		func(l *nativeLease) { l.ExpiresAt = now.Add(time.Hour) },
	} {
		lease := valid
		mutate(&lease)
		if err := validateNativeLease(lease, now, 120*time.Second); err == nil {
			t.Fatal("invalid lease response was accepted")
		}
	}
}
