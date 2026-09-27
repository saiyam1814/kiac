package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) (release, string) {
	t.Helper()
	dir := t.TempDir()
	archive := "kiac_0.8.1_darwin_arm64.tar.gz"
	files := map[string][]byte{archive: []byte("archive"), archive + ".sbom.json": []byte(`{"spdxVersion":"SPDX-2.3"}`)}
	checksums := ""
	for name, data := range files {
		checksums += fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
	}
	files["checksums.txt"] = []byte(checksums)
	r := release{ID: 42, TagName: "v0.8.1", TargetCommitish: "commit", Draft: true}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		r.Assets = append(r.Assets, asset{Name: name, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Size: int64(len(data)), State: "uploaded"})
	}
	return r, dir
}

func TestVerifyDraftAssets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*release)
	}{
		{"empty draft from duplicate release", func(r *release) { r.Assets = nil }},
		{"wrong draft", func(r *release) { r.ID++ }},
		{"wrong tag", func(r *release) { r.TagName = "v0.8.0" }},
		{"moving target", func(r *release) { r.TargetCommitish = "main" }},
		{"already published", func(r *release) { r.Draft = false }},
		{"wrong uploaded bytes", func(r *release) { r.Assets[0].Digest = "sha256:bad" }},
		{"missing digest", func(r *release) { r.Assets[0].Digest = "" }},
		{"unfinished upload", func(r *release) { r.Assets[0].State = "new" }},
		{"wrong size", func(r *release) { r.Assets[0].Size++ }},
		{"duplicate asset", func(r *release) { r.Assets[1] = r.Assets[0] }},
	}
	r, dir := fixture(t)
	if err := verify(r, 42, "v0.8.1", "commit", dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, dir := fixture(t)
			tc.mutate(&r)
			if err := verify(r, 42, "v0.8.1", "commit", dir); err == nil {
				t.Fatal("invalid release was accepted")
			}
		})
	}
}

func TestVerifyRejectsChangedLocalArtifact(t *testing.T) {
	r, dir := fixture(t)
	if err := os.WriteFile(filepath.Join(dir, "kiac_0.8.1_darwin_arm64.tar.gz"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(r, 42, "v0.8.1", "commit", dir); err == nil {
		t.Fatal("changed local artifact was accepted")
	}
}
