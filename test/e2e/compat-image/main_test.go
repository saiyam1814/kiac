package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineImageContentAndDigests(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "traffic")
	payload := []byte("unique local traffic binary")
	if err := os.WriteFile(binary, payload, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "image.tar")
	if err := createImage(binary, "kiac-e2e:fixture", output); err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	files := readTar(t, archive)
	var index struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "kiac-e2e:fixture" {
		t.Fatalf("incorrect exact image name: %+v", index)
	}
	if index.Manifests[0].Platform["architecture"] != "arm64" {
		t.Fatal("wrong image platform")
	}
	var manifest struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(blob(t, files, index.Manifests[0]), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatal("unexpected image layers")
	}
	var config struct {
		Architecture, OS string
		Config           struct{ Entrypoint []string }
		Rootfs           struct {
			DiffIDs []string `json:"diff_ids"`
		}
	}
	if err := json.Unmarshal(blob(t, files, manifest.Config), &config); err != nil {
		t.Fatal(err)
	}
	if config.Architecture != "arm64" || config.OS != "linux" || len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != "/traffic" || len(config.Rootfs.DiffIDs) != 1 || config.Rootfs.DiffIDs[0] != manifest.Layers[0].Digest {
		t.Fatalf("invalid scratch image config: %+v", config)
	}
	if !bytes.Equal(readTar(t, blob(t, files, manifest.Layers[0]))["traffic"], payload) {
		t.Fatal("layer did not preserve the local executable")
	}
	if err := createImage(binary, "kiac-e2e:fixture", output); err == nil {
		t.Fatal("existing archive overwritten")
	}
}

func blob(t *testing.T, files map[string][]byte, desc descriptor) []byte {
	t.Helper()
	data := files["blobs/sha256/"+desc.Digest[7:]]
	if len(data) != desc.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != desc.Digest {
		t.Fatal("OCI descriptor digest/size does not match blob")
	}
	return data
}

func readTar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name], err = io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
	}
}
