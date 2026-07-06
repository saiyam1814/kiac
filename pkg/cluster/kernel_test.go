package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestResolveKernelPassthrough(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "Image")
	if err := os.WriteFile(existing, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		in      string
		want    string // exact resolved path; empty with wantErr unset means "" out
		wantErr string // substring of the expected error
	}{
		{name: "empty keeps default kernel", in: "", want: ""},
		{name: "existing file passes through", in: existing, want: existing},
		{name: "missing path is a typo not a name", in: filepath.Join(dir, "nope"), wantErr: "not found"},
		{name: "dot-relative missing path", in: "./nope-kernel", wantErr: "not found"},
		{name: "unknown name lists known ones", in: "mega", wantErr: `known: full`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveKernel(c.in, dir, kernelAssets)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("resolveKernel(%q) err = %v, want substring %q", c.in, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveKernel(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("resolveKernel(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestResolveKernelUnpublished(t *testing.T) {
	// The real "full" entry ships with an empty checksum until the
	// first kernel-build release lands; it must refuse to download.
	// Once the digest is pinned this guard no longer applies.
	if kernelAssets["full"].SHA256 != "" {
		t.Skip("'full' kernel checksum is pinned; unpublished guard retired")
	}
	_, err := resolveKernel("full", t.TempDir(), kernelAssets)
	if err == nil || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("expected not-published error, got %v", err)
	}
}

func TestResolveKernelDownload(t *testing.T) {
	payload := []byte("pretend arm64 boot Image bytes")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/good":
			w.Write(payload)
		case "/corrupt":
			w.Write([]byte("not what the table pinned"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	assets := map[string]kernelAsset{
		"full":    {URL: srv.URL + "/good", SHA256: sum(payload), File: "kiac-kernel-test-full"},
		"corrupt": {URL: srv.URL + "/corrupt", SHA256: sum(payload), File: "kiac-kernel-test-corrupt"},
		"missing": {URL: srv.URL + "/gone", SHA256: sum(payload), File: "kiac-kernel-test-missing"},
	}

	t.Run("download verifies and caches", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "kernels") // exercises MkdirAll
		got, err := resolveKernel("full", dir, assets)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(dir, "kiac-kernel-test-full"); got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		b, err := os.ReadFile(got)
		if err != nil || string(b) != string(payload) {
			t.Fatalf("cached content = %q, %v", b, err)
		}
		before := hits.Load()
		if _, err := resolveKernel("full", dir, assets); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != before {
			t.Fatalf("cache hit still fetched: %d -> %d requests", before, hits.Load())
		}
	})

	t.Run("checksum mismatch rejects and leaves no cache", func(t *testing.T) {
		dir := t.TempDir()
		_, err := resolveKernel("corrupt", dir, assets)
		if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
			t.Fatalf("err = %v, want sha256 mismatch", err)
		}
		if _, serr := os.Stat(filepath.Join(dir, "kiac-kernel-test-corrupt")); !os.IsNotExist(serr) {
			t.Fatalf("rejected download left a cache file: %v", serr)
		}
	})

	t.Run("corrupted cache is re-downloaded", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "kiac-kernel-test-full")
		if err := os.WriteFile(dest, []byte("bit rot"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveKernel("full", dir, assets)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(got)
		if string(b) != string(payload) {
			t.Fatalf("cache not healed: %q", b)
		}
	})

	t.Run("http error surfaces status", func(t *testing.T) {
		_, err := resolveKernel("missing", t.TempDir(), assets)
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err = %v, want 404 status", err)
		}
	})
}
