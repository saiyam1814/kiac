package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// kernelAsset is one downloadable prebuilt node kernel: an uncompressed
// arm64 boot Image published by the kernel-build workflow as a GitHub
// release asset.
type kernelAsset struct {
	URL    string
	SHA256 string // hex digest of the asset; empty means not published yet
	File   string // filename cached under ~/.kiac/kernels/
}

// kernelAssets pins each named kernel to its release asset and exact
// checksum; like nodeImages, kernels are only trusted at exact digests.
// Refresh SHA256 from the .sha256 asset after a kernel-build release.
var kernelAssets = map[string]kernelAsset{
	// "full" enables what Cilium/Calico/flannel need (VXLAN, GENEVE,
	// br_netfilter, BPF JIT + BTF, WireGuard) on top of Apple's base
	// config; see kernel/config-full.
	"full": {
		URL:    "https://github.com/saiyam1814/kiac/releases/download/kernel-v6.12.28-full/kiac-kernel-6.12.28-full",
		SHA256: "", // filled in by the first kernel-build release; downloads are blocked until then
		File:   "kiac-kernel-6.12.28-full",
	},
}

// kernelHTTP allows the kernel to take a while on slow links but never
// hang a create forever; the asset is ~15MB.
var kernelHTTP = &http.Client{Timeout: 10 * time.Minute}

// ResolveKernel maps a --kernel value to a local kernel binary path.
// An empty value keeps the runtime's default kernel ("" out). A path
// to an existing file passes through untouched. A known name ("full")
// downloads the pinned release asset into ~/.kiac/kernels/ once,
// verifying its sha256 both on download and on every cache hit so a
// corrupted or tampered cache can never boot a node.
func ResolveKernel(nameOrPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving kernel cache dir: %w", err)
	}
	return resolveKernel(nameOrPath, filepath.Join(home, ".kiac", "kernels"), kernelAssets)
}

// resolveKernel is ResolveKernel with the cache dir and asset table
// injected so tests can point it at a temp dir and an httptest server.
func resolveKernel(nameOrPath, dir string, assets map[string]kernelAsset) (string, error) {
	if nameOrPath == "" {
		return "", nil
	}
	if fi, err := os.Stat(nameOrPath); err == nil && fi.Mode().IsRegular() {
		return filepath.Abs(nameOrPath)
	}
	// Anything path-shaped that does not exist is a user typo, not a
	// kernel name; failing here beats a confusing "unknown kernel".
	if strings.ContainsRune(nameOrPath, os.PathSeparator) || strings.HasPrefix(nameOrPath, ".") || strings.HasPrefix(nameOrPath, "~") {
		return "", fmt.Errorf("kernel file %q not found", nameOrPath)
	}
	asset, ok := assets[nameOrPath]
	if !ok {
		return "", fmt.Errorf("unknown kernel %q (known: %s; or pass a path to an arm64 boot Image)",
			nameOrPath, strings.Join(kernelNames(assets), ", "))
	}
	if asset.SHA256 == "" {
		return "", fmt.Errorf("kernel %q is not published yet: run the kernel-build workflow, then pin its sha256 in pkg/cluster/kernel.go", nameOrPath)
	}

	dest := filepath.Join(dir, asset.File)
	if err := verifyFileSHA256(dest, asset.SHA256); err == nil {
		return dest, nil
	}
	// Missing or corrupted cache: (re-)download and verify atomically.
	if err := downloadVerified(asset.URL, dest, asset.SHA256); err != nil {
		return "", fmt.Errorf("downloading kernel %q: %w", nameOrPath, err)
	}
	return dest, nil
}

func kernelNames(assets map[string]kernelAsset) []string {
	names := make([]string, 0, len(assets))
	for n := range assets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// verifyFileSHA256 checks that path exists and hashes to want (hex).
func verifyFileSHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", path, got, want)
	}
	return nil
}

// downloadVerified streams url into dest, but only lets a fully
// verified file land there: bytes go to a temp file in the same dir,
// the hash is checked, then an atomic rename publishes it. A partial
// or corrupt download never becomes a cache hit.
func downloadVerified(url, dest, wantSHA256 string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	resp, err := kernelHTTP.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".kiac-kernel-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", url, got, wantSHA256)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
