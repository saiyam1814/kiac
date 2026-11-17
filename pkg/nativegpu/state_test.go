package nativegpu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeTLSStateAndCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "service")
	state, err := OpenServiceState(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if other, err := OpenServiceState(dir, "127.0.0.1:0"); err == nil {
		other.Close()
		t.Fatal("duplicate host service acquired state lock")
	}
	broker := testBroker(t, &brokerEngine{})
	// The broker token must match the generated private state for real TLS calls.
	broker.adminToken = state.AdminToken
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- state.Serve(ctx, broker) }()
	var credentials Credentials
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		credentials, err = AdminCredentials(dir)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	response, err := Request(requestCtx, credentials, http.MethodGet, "/v1/admin/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !json.Valid(body) {
		t.Fatal("health was not JSON")
	}
	if _, err := Request(requestCtx, credentials, http.MethodPost, "/v1/leases", LeaseRequest{Namespace: "test", ClaimUID: "claim", NodeName: "node"}); err != nil {
		t.Fatal(err)
	}
	// Never follow a caller-provided non-TLS or path-bearing endpoint.
	for _, endpoint := range []string{"http://127.0.0.1:1", "https://example.test/path", "https://user:pass@example.test", "https://example.test?query=x"} {
		bad := credentials
		bad.Endpoint = endpoint
		if client, err := Client(bad); err == nil {
			client.CloseIdleConnections()
			t.Fatal("unsafe origin accepted", endpoint)
		}
	}
	bad := credentials
	bad.CAFile = filepath.Join(t.TempDir(), "wrong-ca.crt")
	if err := os.WriteFile(bad.CAFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Client(bad); err == nil {
		t.Fatal("invalid CA accepted")
	}
	consumer := Credentials{Version: 1, Endpoint: state.Endpoint, Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CAFile: "ca.crt"}
	data, _ := json.Marshal(consumer)
	credentialPath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credentialPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadCredentials(credentialPath)
	if err != nil || loaded.CAFile != filepath.Join(dir, "ca.crt") {
		t.Fatal("relative CA resolution failed", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TLS server did not stop")
	}
	if _, err := AdminCredentials(dir); err == nil {
		t.Fatal("stopped service retained live endpoint metadata")
	}
}

func TestNativeStateRejectsUnsafePathsAndForeignCAType(t *testing.T) {
	base := t.TempDir()
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "8.8.8.8:0", "localhost:0"} {
		dir := filepath.Join(base, "address-"+big.NewInt(int64(len(address))).String())
		if state, err := OpenServiceState(dir, address); err == nil {
			state.Close()
			t.Fatal("unsafe listener accepted", address)
		}
	}
	public := filepath.Join(base, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if state, err := OpenServiceState(public, "127.0.0.1:0"); err == nil {
		state.Close()
		t.Fatal("public state directory accepted")
	}
	symlink := filepath.Join(base, "link")
	if err := os.Symlink(public, symlink); err != nil {
		t.Fatal(err)
	}
	if state, err := OpenServiceState(symlink, "127.0.0.1:0"); err == nil {
		state.Close()
		t.Fatal("symlinked state directory accepted")
	}
	dir := filepath.Join(base, "private")
	state, err := OpenServiceState(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
	publicKey, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, publicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if state, err := OpenServiceState(dir, "127.0.0.1:0"); err == nil {
		state.Close()
		t.Fatal("mismatched CA type accepted")
	}
}
