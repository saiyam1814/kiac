package cluster

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRedactSupportText(t *testing.T) {
	raw := `/Users/test/kiac
token: secret-token
"password": "hunter2"
client-key-data: c2VjcmV0
Authorization: Bearer bearer-secret
command --token=flag-secret
K10abcdef::server:k3s-secret
tunnel_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
https://alice:url-secret@example.test/path
-----BEGIN PRIVATE KEY-----
private-secret
-----END PRIVATE KEY-----
ordinary diagnostic text
`
	got := redactSupportText(raw, "/Users/test")
	for _, secret := range []string{"secret-token", "hunter2", "c2VjcmV0", "bearer-secret", "flag-secret", "k3s-secret", strings.Repeat("a", 64), "url-secret", "private-secret", "/Users/test"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted output still contains %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"$HOME/kiac", "[REDACTED]", "ordinary diagnostic text"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted output missing %q:\n%s", want, got)
		}
	}
}

func TestWriteSupportArchive(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	files := []supportFile{{name: "z.txt", data: []byte("last\n")}, {name: "a/info.txt", data: []byte("first\n")}}
	if err := writeSupportArchive(out, "kiac-support-dev", now, files); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("archive mode = %o, want 600", got)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	contents := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		contents[header.Name] = string(raw)
		if header.Mode != 0o600 {
			t.Errorf("entry %s mode = %o, want 600", header.Name, header.Mode)
		}
	}
	wantNames := []string{"kiac-support-dev/a/info.txt", "kiac-support-dev/z.txt"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Errorf("archive names = %v, want %v", names, wantNames)
	}
	if contents[wantNames[0]] != "first\n" || contents[wantNames[1]] != "last\n" {
		t.Errorf("archive contents = %#v", contents)
	}
	if err := writeSupportArchive(out, "kiac-support-dev", now, files); err == nil {
		t.Fatal("archive writer overwrote an existing bundle")
	}
}

func TestSupportCollectorBoundsAndRejectsUnsafePath(t *testing.T) {
	c := &supportCollector{}
	c.addText("../secret.txt", "must not be added")
	c.addText("logs/large.txt", strings.Repeat("\u00e9", maxSupportFileBytes))
	if len(c.files) != 1 || c.files[0].name != "logs/large.txt" {
		t.Fatalf("collector files = %+v", c.files)
	}
	if len(c.files[0].data) > maxSupportFileBytes {
		t.Errorf("bounded file has %d bytes, limit %d", len(c.files[0].data), maxSupportFileBytes)
	}
	if !utf8.Valid(c.files[0].data) {
		t.Fatal("truncation split a UTF-8 rune")
	}
	if len(c.warnings) == 0 {
		t.Fatal("unsafe path did not produce a warning")
	}
}

func TestSupportOutputPath(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 8, 12, 34, 56, 0, time.UTC)
	got, err := supportOutputPath("dev", now, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "kiac-support-dev-20260808T123456Z.tar.gz")
	if got != want {
		t.Errorf("output path = %q, want %q", got, want)
	}
}
