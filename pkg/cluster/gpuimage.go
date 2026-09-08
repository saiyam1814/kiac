package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lima-vm/go-qcow2reader"
	qcowconvert "github.com/lima-vm/go-qcow2reader/convert"
)

const DefaultGPUImage = "fedora-44"

type gpuImageAsset struct {
	URL       string
	SHA256    string
	RawSHA256 string
	File      string
	RawFile   string
}

var gpuImageAssets = map[string]gpuImageAsset{
	"fedora-44": {
		URL: "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/aarch64/images/" +
			"Fedora-Cloud-Base-Generic-44-1.7.aarch64.qcow2",
		SHA256:    "55c60a3b80d3616a08705afd0459e75fe9f03c54aba7a46e4002a41a72fa0d5b",
		RawSHA256: "432913a8fa8028d173b865c69874ac68ae6dc0d9f85d67a862eac923c644b12d",
		File:      "Fedora-Cloud-Base-Generic-44-1.7.aarch64.qcow2",
		RawFile:   "Fedora-Cloud-Base-Generic-44-1.7.aarch64.raw",
	},
}

// SupportedGPUImages returns the verified boot-disk aliases accepted by
// --gpu-image. A caller may also provide an absolute raw disk path.
func SupportedGPUImages() []string {
	images := make([]string, 0, len(gpuImageAssets))
	for name := range gpuImageAssets {
		images = append(images, name)
	}
	sort.Strings(images)
	return images
}

// ResolveGPUImage returns a trusted raw ARM64 cloud disk suitable for
// krunkit. The official Fedora qcow2 is downloaded and verified once, then
// converted in-process so GPU users do not need the large QEMU dependency.
func ResolveGPUImage(nameOrPath string) (string, error) {
	if nameOrPath == "" {
		nameOrPath = DefaultGPUImage
	}
	if info, err := os.Stat(nameOrPath); err == nil && info.Mode().IsRegular() {
		path, err := filepath.Abs(nameOrPath)
		if err != nil {
			return "", err
		}
		if err := rejectQCOW2GPUImage(path); err != nil {
			return "", err
		}
		return path, nil
	}
	if strings.ContainsRune(nameOrPath, os.PathSeparator) || strings.HasPrefix(nameOrPath, ".") || strings.HasPrefix(nameOrPath, "~") {
		return "", fmt.Errorf("GPU node image %q not found", nameOrPath)
	}
	asset, ok := gpuImageAssets[nameOrPath]
	if !ok {
		return "", fmt.Errorf("unknown GPU node image %q (known: %s; or pass a raw disk path)", nameOrPath, strings.Join(SupportedGPUImages(), ", "))
	}
	if asset.RawSHA256 == "" {
		return "", fmt.Errorf("GPU node image %q is not fully pinned: raw image checksum is missing", nameOrPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving GPU image cache: %w", err)
	}
	return resolveGPUImage(asset, filepath.Join(home, ".kiac", "images"))
}

func rejectQCOW2GPUImage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var header [4]byte
	n, err := f.Read(header[:])
	if err != nil && n == 0 {
		return err
	}
	if n == len(header) && string(header[:]) == "QFI\xfb" {
		return fmt.Errorf("GPU node image %q is qcow2; pass a raw ARM64 disk (the %s alias is converted automatically)", path, DefaultGPUImage)
	}
	return nil
}

func resolveGPUImage(asset gpuImageAsset, dir string) (string, error) {
	qcowPath := filepath.Join(dir, asset.File)
	rawPath := filepath.Join(dir, asset.RawFile)
	if err := verifyFileSHA256(rawPath, asset.RawSHA256); err == nil {
		return rawPath, nil
	}
	if err := verifyFileSHA256(qcowPath, asset.SHA256); err != nil {
		if err := downloadVerified(asset.URL, qcowPath, asset.SHA256); err != nil {
			return "", fmt.Errorf("downloading GPU node image: %w", err)
		}
	}
	if err := convertQCOW2ToRaw(qcowPath, rawPath, asset.RawSHA256); err != nil {
		return "", fmt.Errorf("converting GPU node image: %w", err)
	}
	return rawPath, nil
}

func convertQCOW2ToRaw(source, destination, wantSHA256 string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	image, err := qcow2reader.Open(in)
	if err != nil {
		return err
	}
	defer image.Close()
	if err := image.Readable(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(destination), ".kiac-gpu-image-*.raw")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err := out.Chmod(0o644); err != nil {
		out.Close()
		return err
	}
	if err := qcowconvert.Convert(out, image, qcowconvert.Options{}); err != nil {
		out.Close()
		return err
	}
	if err := out.Truncate(image.Size()); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := verifyFileSHA256(out.Name(), wantSHA256); err != nil {
		return err
	}
	return os.Rename(out.Name(), destination)
}
