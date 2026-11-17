package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func nativeCleanupFixture(t *testing.T, handler http.Handler) (options, *nativeDriver, *fakeNativeBroker) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	root := t.TempDir()
	caFile, tokenFile := filepath.Join(root, "ca.crt"), filepath.Join(root, "admin.token")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("private-admin-token-do-not-log"), 0o600); err != nil {
		t.Fatal(err)
	}
	claim := nativeTestClaim("claim-one")
	driver, broker, _ := newNativeTestDriver(t, claim)
	opts := options{nodeName: "node-one", cdiRoot: driver.cdiRoot, pluginsDir: filepath.Join(root, "plugins"),
		native: nativeOptions{selectedNode: "node-one", endpoint: server.URL, caFile: caFile, adminTokenFile: tokenFile}}
	driver.stateRoot = filepath.Join(opts.pluginsDir, nativeDriverName, "native-state")
	if err := driver.loadStates(); err != nil {
		t.Fatal(err)
	}
	prepareNativeTestClaim(t, driver, claim)
	return opts, driver, broker
}

func TestNativeCleanupLocksAgainstDriverRevokesAndPreservesUnrelatedFiles(t *testing.T) {
	revokes := 0
	opts, driver, broker := nativeCleanupFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/leases/x" || r.Header.Get("Authorization") != "Bearer private-admin-token-do-not-log" {
			t.Errorf("cleanup attempted an unexpected operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		revokes++
		w.WriteHeader(http.StatusNoContent)
	}))
	state := driver.states[types.UID("claim-one")]
	pluginDir := filepath.Join(opts.pluginsDir, nativeDriverName)
	unlock, err := lockNativeCheckpoint(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runNativeCleanup(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "owns this node checkpoint") || revokes != 0 {
		t.Fatal("cleanup raced a live renewing driver or called the broker before obtaining the lock")
	}
	unlock()
	unrelatedCDI := filepath.Join(driver.cdiRoot, "kiac-gpu.json")
	if err := os.WriteFile(unrelatedCDI, []byte("existing Venus CDI"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherPlugin := filepath.Join(opts.pluginsDir, "another.kiac.dev")
	if err := os.Mkdir(otherPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runNativeCleanup(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if revokes != 1 || broker.acquires != 1 {
		t.Fatalf("cleanup did not confirm revocation, or renewed a lease: revokes=%d acquires=%d", revokes, broker.acquires)
	}
	for _, removed := range []string{driver.stateRoot, driver.cdiPath(state)} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("native cleanup left %s", removed)
		}
	}
	for _, preserved := range []string{unrelatedCDI, otherPlugin, filepath.Join(pluginDir, "native.lock")} {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatalf("cleanup removed an unrelated/shared artifact %s: %v", preserved, err)
		}
	}
	if err := runNativeCleanup(t.Context(), opts); err != nil || revokes != 1 {
		t.Fatal("duplicate cleanup was not idempotent", err)
	}
}

func TestNativeCleanupRequiresConfirmedRevocationAndCanRetryAfterRestart(t *testing.T) {
	available := false
	revokes := 0
	opts, driver, _ := nativeCleanupFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Error("cleanup renewed or performed work instead of revoking")
		}
		revokes++
		if !available {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	state := driver.states[types.UID("claim-one")]
	if err := runNativeCleanup(t.Context(), opts); err == nil {
		t.Fatal("cleanup reported success while revocation was unconfirmed")
	}
	for _, retained := range []string{filepath.Join(driver.claimDir(state), "state.json"), driver.cdiPath(state), filepath.Join(driver.claimDir(state), "credentials", "credentials.json")} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatalf("unconfirmed cleanup discarded its retry artifacts: %s: %v", retained, err)
		}
	}
	replacement := restartedNativeTestDriver(driver)
	if err := replacement.loadStates(); err != nil || !replacement.states[state.UID].PendingRevoke {
		t.Fatal("cleanup revocation intention was not persisted", err)
	}
	available = true
	if err := runNativeCleanup(t.Context(), opts); err != nil || revokes != 2 {
		t.Fatal("cleanup could not retry broker-confirmed revocation", err)
	}
}

func TestNativeCleanupRejectsSymlinkedOrPublicStateAndWrongNode(t *testing.T) {
	driver, broker, _ := newNativeTestDriver(t)
	if err := os.RemoveAll(driver.stateRoot); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if err := os.WriteFile(marker, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, driver.stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := driver.loadStates(); err == nil || broker.revokes != 0 {
		t.Fatal("symlinked native checkpoint was accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("symlink target was modified")
	}
	if err := os.Remove(driver.stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(driver.stateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := driver.loadStates(); err == nil {
		t.Fatal("public native checkpoint directory was accepted")
	}
	if err := runNativeCleanup(t.Context(), options{nodeName: "node-one", native: nativeOptions{selectedNode: "node-two"}}); err == nil {
		t.Fatal("cleanup accepted a different selected node")
	}
}
