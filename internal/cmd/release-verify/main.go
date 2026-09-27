// release-verify checks the exact GitHub draft before it becomes immutable.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type release struct {
	ID              int64   `json:"id"`
	TagName         string  `json:"tag_name"`
	TargetCommitish string  `json:"target_commitish"`
	Draft           bool    `json:"draft"`
	Assets          []asset `json:"assets"`
}

type asset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
}

func verify(r release, id int64, tag, commit, dir string) error {
	if id <= 0 || r.ID != id || r.TagName != tag || r.TargetCommitish != commit || !r.Draft {
		return fmt.Errorf("release must be draft %d for %s at %s", id, tag, commit)
	}
	checksums, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return err
	}
	expected := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(checksums)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return fmt.Errorf("invalid checksum line %q", line)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return fmt.Errorf("invalid checksum: %w", err)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "." || name == ".." || filepath.Base(name) != name || strings.Contains(name, "\\") {
			return fmt.Errorf("invalid asset filename %q", name)
		}
		if _, exists := expected[name]; exists || name == "checksums.txt" {
			return fmt.Errorf("duplicate or recursive checksum for %q", name)
		}
		expected[name] = "sha256:" + strings.ToLower(fields[0])
	}
	archive := "kiac_" + strings.TrimPrefix(tag, "v") + "_darwin_arm64.tar.gz"
	if expected[archive] == "" || expected[archive+".sbom.json"] == "" {
		return fmt.Errorf("archive and SBOM must be present in checksums.txt")
	}
	expected["checksums.txt"] = fmt.Sprintf("sha256:%x", sha256.Sum256(checksums))
	if len(r.Assets) != len(expected) {
		return fmt.Errorf("draft has %d assets, expected %d", len(r.Assets), len(expected))
	}
	seen := map[string]bool{}
	for _, a := range r.Assets {
		want, ok := expected[a.Name]
		if !ok || seen[a.Name] {
			return fmt.Errorf("unexpected or duplicate uploaded asset %q", a.Name)
		}
		seen[a.Name] = true
		local, err := os.ReadFile(filepath.Join(dir, a.Name))
		if err != nil {
			return err
		}
		if fmt.Sprintf("sha256:%x", sha256.Sum256(local)) != want {
			return fmt.Errorf("local asset %s does not match its checksum", a.Name)
		}
		if a.State != "uploaded" || a.Size <= 0 || a.Size != int64(len(local)) || a.Digest != want {
			return fmt.Errorf("uploaded asset %s does not match verified local bytes", a.Name)
		}
	}
	return nil
}

func main() {
	id := flag.Int64("release-id", 0, "expected GitHub release ID")
	tag := flag.String("tag", "", "expected release tag")
	commit := flag.String("commit", "", "expected source commit")
	dir := flag.String("dir", "dist", "local artifact directory")
	flag.Parse()
	var r release
	err := json.NewDecoder(os.Stdin).Decode(&r)
	if err == nil {
		err = verify(r, *id, *tag, *commit, *dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Draft %d contains the verified archive, SBOM and checksums for %s.\n", *id, *tag)
}
