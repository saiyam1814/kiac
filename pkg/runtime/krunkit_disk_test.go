package runtime

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKrunkitDiskHeadroomBoundaries(t *testing.T) {
	for _, tc := range []struct {
		nodes    int
		required uint64
	}{{1, 7 << 30}, {2, 10 << 30}, {3, 13 << 30}, {7, 25 << 30}} {
		for _, delta := range []int64{-1, 0, 1} {
			free := uint64(int64(tc.required) + delta)
			err := checkKrunkitDiskHeadroom(t.TempDir(), tc.nodes, func(string) (uint64, error) { return free, nil })
			if (err == nil) != (delta >= 0) {
				t.Fatalf("nodes=%d free=%d required=%d err=%v", tc.nodes, free, tc.required, err)
			}
			if err != nil && (!strings.Contains(err.Error(), "4 GiB reserve + 3 GiB per VM") || !strings.Contains(err.Error(), "does not reserve space")) {
				t.Fatalf("missing actionable estimate policy: %v", err)
			}
		}
	}
}
func TestKrunkitDiskHeadroomUsesNearestExistingDirectoryWithoutWrites(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "not-created", "gpu-nodes")
	calls := 0
	err := checkKrunkitDiskHeadroom(root, 3, func(path string) (uint64, error) {
		calls++
		if path != parent {
			t.Fatalf("measured %s, want %s", path, parent)
		}
		return 13 << 30, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("check=%v calls=%d", err, calls)
	}
	if _, err := os.Lstat(filepath.Join(parent, "not-created")); !os.IsNotExist(err) {
		t.Fatalf("headroom check wrote state directory: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("headroom check created files: %v %v", entries, err)
	}
}
func TestKrunkitDiskHeadroomFailsClosed(t *testing.T) {
	root := t.TempDir()
	measurementError := errors.New("statfs unavailable")
	if err := checkKrunkitDiskHeadroom(root, 1, func(string) (uint64, error) { return 0, measurementError }); !errors.Is(err, measurementError) {
		t.Fatalf("measurement error lost: %v", err)
	}
	never := func(string) (uint64, error) { t.Fatal("invalid input reached statfs"); return math.MaxUint64, nil }
	for _, count := range []int{0, -1, int(^uint(0) >> 1)} {
		if err := checkKrunkitDiskHeadroom(root, count, never); err == nil {
			t.Fatalf("accepted node count %d", count)
		}
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkKrunkitDiskHeadroom(file, 1, never); err == nil {
		t.Fatal("regular state-root file accepted")
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := checkKrunkitDiskHeadroom(filepath.Join(dangling, "child"), 1, never); err == nil {
		t.Fatal("dangling state-root symlink accepted")
	}
}
func TestKrunkitCheckDiskHeadroomDoesNotInitializeStore(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "gpu-nodes")
	// Real Statfs is read-only; an intentionally impossible topology fails on
	// any filesystem without requiring the test host itself to be nearly full.
	err := (&KrunkitClient{RootDir: root}).CheckDiskHeadroom(1 << 30)
	if err == nil {
		t.Fatal("impossible disk headroom unexpectedly available")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only check initialized store: %v", err)
	}
}
