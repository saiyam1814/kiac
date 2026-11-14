// Command compat-image creates an offline scratch OCI image for runtime tests.
// No builder VM, registry, base image, or Docker daemon is needed.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/distribution/reference"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int               `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    map[string]string `json:"platform,omitempty"`
}

func main() {
	binary := flag.String("binary", "", "static linux/arm64 traffic binary")
	name := flag.String("name", "", "unique tagged test image name")
	output := flag.String("output", "", "output OCI tar archive")
	flag.Parse()
	if flag.NArg() != 0 || *binary == "" || *name == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "compat-image requires --binary, --name and --output")
		os.Exit(2)
	}
	if err := createImage(*binary, *name, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func createImage(binary, name, output string) error {
	ref, err := reference.ParseDockerRef(name)
	if err != nil {
		return fmt.Errorf("invalid image name: %w", err)
	}
	if _, ok := ref.(reference.NamedTagged); !ok {
		return fmt.Errorf("test image needs a tag")
	}
	f, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 32<<20 {
		return fmt.Errorf("traffic binary must be a regular file between 1 byte and 32 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, 32<<20+1))
	if err != nil || len(data) != int(stat.Size()) {
		return fmt.Errorf("cannot read complete traffic binary")
	}
	var layer bytes.Buffer
	lt := tar.NewWriter(&layer)
	if err := tarFile(lt, "traffic", data, 0755); err != nil {
		return err
	}
	if err := lt.Close(); err != nil {
		return err
	}
	layerDescriptor := describe("application/vnd.oci.image.layer.v1.tar", layer.Bytes())
	config := encode(map[string]any{
		"architecture": "arm64", "os": "linux",
		"config": map[string]any{"Entrypoint": []string{"/traffic"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{layerDescriptor.Digest}},
	})
	configDescriptor := describe("application/vnd.oci.image.config.v1+json", config)
	manifest := encode(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": configDescriptor, "layers": []descriptor{layerDescriptor},
	})
	manifestDescriptor := describe("application/vnd.oci.image.manifest.v1+json", manifest)
	manifestDescriptor.Annotations = map[string]string{
		"org.opencontainers.image.ref.name":     name,
		"io.containerd.image.name":              name,
		"com.apple.containerization.image.name": name,
	}
	manifestDescriptor.Platform = map[string]string{"os": "linux", "architecture": "arm64"}
	index := encode(map[string]any{"schemaVersion": 2, "manifests": []descriptor{manifestDescriptor}})
	archive, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(archive)
	for _, item := range []struct {
		name string
		data []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
		{"blobs/sha256/" + manifestDescriptor.Digest[7:], manifest},
		{"blobs/sha256/" + configDescriptor.Digest[7:], config},
		{"blobs/sha256/" + layerDescriptor.Digest[7:], layer.Bytes()},
	} {
		if err = tarFile(tw, item.name, item.data, 0644); err != nil {
			break
		}
	}
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(output)
	}
	return err
}

func describe(mediaType string, data []byte) descriptor {
	return descriptor{MediaType: mediaType, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Size: len(data)}
}

func encode(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err) // All callers supply JSON-compatible literals.
	}
	return data
}

func tarFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
