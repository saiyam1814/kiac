package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveGPUImageUsesVerifiedRawCache(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("raw disk fixture")
	sum := sha256.Sum256(raw)
	asset := gpuImageAsset{
		RawFile:   "image.raw",
		RawSHA256: hex.EncodeToString(sum[:]),
	}
	path := filepath.Join(dir, asset.RawFile)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveGPUImage(asset, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("resolved path = %q, want %q", got, path)
	}
}

func TestResolveGPUImageRejectsCustomQCOW2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.qcow2")
	if err := os.WriteFile(path, []byte{'Q', 'F', 'I', 0xfb, 0, 0, 0, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveGPUImage(path)
	if err == nil || !strings.Contains(err.Error(), "is qcow2; pass a raw ARM64 disk") {
		t.Fatalf("ResolveGPUImage error = %v", err)
	}
}

// This opt-in test verifies that the pinned Fedora qcow2 converts to the exact
// raw checksum in the asset table. Runtime-smoke can provide the downloaded
// source without making every unit-test run fetch a 504 MiB image.
func TestPinnedGPUImageConversion(t *testing.T) {
	source := os.Getenv("KIAC_TEST_GPU_QCOW2")
	if source == "" {
		t.Skip("set KIAC_TEST_GPU_QCOW2 to the pinned Fedora qcow2")
	}
	asset := gpuImageAssets[DefaultGPUImage]
	destination := filepath.Join(t.TempDir(), asset.RawFile)
	if err := convertQCOW2ToRaw(source, destination, asset.RawSHA256); err != nil {
		t.Fatal(err)
	}
}
