package runtime

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

const (
	krunkitDiskReserveBytes uint64 = 4 << 30
	krunkitDiskPerNodeBytes uint64 = 3 << 30
)

// CheckDiskHeadroom admits a new topology using a thin-provisioning estimate:
// 4 GiB of host reserve plus 3 GiB per VM. It neither reserves space nor
// guarantees that later guest-disk growth or workload downloads will fit.
// The check is read-only, including when the state directory does not exist.
func (c *KrunkitClient) CheckDiskHeadroom(nodeCount int) error {
	root, err := c.rootDir()
	if err != nil {
		return err
	}
	return checkKrunkitDiskHeadroom(root, nodeCount, availableKrunkitDiskBytes)
}

func checkKrunkitDiskHeadroom(root string, nodeCount int, available func(string) (uint64, error)) error {
	if nodeCount < 1 || uint64(nodeCount) > (math.MaxUint64-krunkitDiskReserveBytes)/krunkitDiskPerNodeBytes {
		return fmt.Errorf("invalid krunkit topology size %d for disk-headroom check", nodeCount)
	}
	required := krunkitDiskReserveBytes + uint64(nodeCount)*krunkitDiskPerNodeBytes
	path, err := nearestExistingKrunkitDirectory(root)
	if err != nil {
		return fmt.Errorf("check host disk headroom for krunkit: %w", err)
	}
	free, err := available(path)
	if err != nil {
		return fmt.Errorf("check host disk headroom for krunkit at %s: %w", path, err)
	}
	if free < required {
		return fmt.Errorf("insufficient host disk headroom for %d krunkit VM(s): %.2f GiB available at %s, need at least %.0f GiB (4 GiB reserve + 3 GiB per VM); free host disk space before creating this topology. This thin-provisioning estimate does not reserve space or guarantee later guest-disk growth", nodeCount, float64(free)/(1<<30), path, float64(required)/(1<<30))
	}
	return nil
}

func nearestExistingKrunkitDirectory(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", path)
			}
			return path, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		// A dangling symlink cannot become a writable state directory via MkdirAll.
		if info, linkErr := os.Lstat(path); linkErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a dangling symlink", path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}

func availableKrunkitDiskBytes(path string) (uint64, error) {
	// W_OK|X_OK: creating files in a directory requires write and search access.
	// Check without creating a sentinel or modifying the filesystem.
	if err := syscall.Access(path, 3); err != nil {
		return 0, fmt.Errorf("filesystem is not writable: %w", err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 || stat.Bavail > math.MaxUint64/uint64(stat.Bsize) {
		return 0, fmt.Errorf("invalid available disk-space measurement")
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
