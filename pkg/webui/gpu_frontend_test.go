package webui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Node is a development-only test runner. The dashboard itself has no Node
// dependency; installations without it still run the Go endpoint tests.
func TestGPUFrontendBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available for dashboard JavaScript behavior checks")
	}
	page := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(page, indexHTML, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join("testdata", "gpu-dashboard.cjs"), page)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dashboard behavior checks: %v\n%s", err, out)
	}
}
