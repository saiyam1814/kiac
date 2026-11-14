package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVMStatsCPUUsesAllocatedCapacity(t *testing.T) {
	const id = "kiac-dev-control-plane"
	stats, err := parseVMStats([]byte(`[{"id":"kiac-dev-control-plane","cpuUtilization":2.5,"memoryUsageBytes":1073741824,"memoryLimitBytes":2147483648},{"id":"other","cpuUtilization":8}]`), map[string]bool{id: true}, map[string]uint64{id: 4})
	if err != nil || len(stats) != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	got := stats[0]
	if got.CPUCoreUsage == nil || *got.CPUCoreUsage != 2.5 || got.CPUPercent == nil || *got.CPUPercent != 62.5 {
		t.Fatalf("2.5 busy cores / 4 allocated must be 62.5%%: %+v", got)
	}
	if got.MemoryPercent == nil || *got.MemoryPercent != 50 {
		t.Fatalf("memory percentage = %v, want 50", got.MemoryPercent)
	}
}

func TestVMStatsMissingValuesStayUnavailable(t *testing.T) {
	const id = "kiac-dev-control-plane"
	for _, sample := range []string{
		`{"id":"kiac-dev-control-plane"}`,
		`{"id":"kiac-dev-control-plane","cpuUtilization":null,"memoryLimitBytes":0}`,
		`{"id":"kiac-dev-control-plane","cpuUtilization":-1}`,
	} {
		rows, err := parseVMStats([]byte("["+sample+"]"), map[string]bool{id: true}, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("parse %s: %v", sample, err)
		}
		if rows[0].CPUCoreUsage != nil || rows[0].CPUPercent != nil || rows[0].MemoryPercent != nil {
			t.Fatalf("missing/invalid counters became valid measurements: %+v", rows[0])
		}
	}
	rows, err := parseVMStats([]byte(`[{"id":"kiac-dev-control-plane","cpuUtilization":0,"memoryUsageBytes":0}]`), map[string]bool{id: true}, nil)
	if err != nil || rows[0].CPUCoreUsage == nil || *rows[0].CPUCoreUsage != 0 || rows[0].CPUPercent != nil || rows[0].MemoryUsageBytes == nil {
		t.Fatalf("real zero must stay distinct from absent capacity: %+v, %v", rows, err)
	}
}

func TestVMStatsRejectsInvalidJSONAndDuplicateIDs(t *testing.T) {
	wanted := map[string]bool{"kiac-dev-control-plane": true}
	for _, value := range []string{`not-json`, `[{"id":"kiac-dev-control-plane","memoryUsageBytes":-1}]`, `[{"id":"kiac-dev-control-plane"},{"id":"kiac-dev-control-plane"}]`} {
		if _, err := parseVMStats([]byte(value), wanted, nil); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestVMStatsCPUCapacityFromAppleInspect(t *testing.T) {
	wanted := map[string]bool{"kiac-dev-control-plane": true}
	got := parseVMCPUCapacity([]byte(`[{"configuration":{"id":"kiac-dev-control-plane","resources":{"cpus":4}}},{"configuration":{"id":"other","resources":{"cpus":16}}}]`), wanted)
	if len(got) != 1 || got["kiac-dev-control-plane"] != 4 {
		t.Fatalf("capacity = %v", got)
	}
	for _, input := range []string{`not-json`, `[{"configuration":{"id":"kiac-dev-control-plane","resources":{"cpus":0}}}]`, `[{"configuration":{"id":"kiac-dev-control-plane","resources":{"cpus":2.5}}}]`} {
		if got := parseVMCPUCapacity([]byte(input), wanted); len(got) != 0 {
			t.Fatalf("invalid capacity accepted: %v", got)
		}
	}
}

func TestVMStatsExplicitTargetsAndOlderRuntime(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	// JSON before 1.7.1 has memory but no CPU rate. An inspect failure
	// must not discard the available memory sample or invent a CPU rate.
	script := `#!/bin/sh
case "$1" in
  inspect) exit 1 ;;
  stats)
    test "$2" = --no-stream && test "$3" = --format && test "$4" = json && test "$5" = kiac-dev-control-plane && test "$#" = 5 || exit 4
    echo '[{"id":"kiac-dev-control-plane","memoryUsageBytes":123}]' ;;
  *) exit 5 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stats, err := (&Client{Bin: bin}).VMStats(context.Background(), []string{"kiac-dev-control-plane"})
	if err != nil || len(stats) != 1 || stats[0].CPUCoreUsage != nil || stats[0].MemoryUsageBytes == nil || *stats[0].MemoryUsageBytes != 123 {
		t.Fatalf("older runtime stats = %+v, %v", stats, err)
	}
	client := &Client{Bin: "/must-not-run"}
	if rows, err := client.VMStats(context.Background(), nil); err != nil || len(rows) != 0 {
		t.Fatalf("empty targets must not invoke host-wide stats: %+v, %v", rows, err)
	}
	for _, names := range [][]string{{"--all"}, {"a/b"}, {"node", "node"}, {""}} {
		if _, err := client.VMStats(context.Background(), names); err == nil || strings.Contains(err.Error(), "must-not-run") {
			t.Fatalf("invalid targets reached execution: %v, %v", names, err)
		}
	}
}

func TestStatsCommandDeadlineAndOutputBounds(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (&Client{Bin: bin}).statsCommand(ctx, "stats"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("stats command ignored deadline")
	}
	b := &statsOutput{remaining: 5}
	if n, err := b.Write([]byte("123456789")); n != 5 || !errors.Is(err, io.ErrShortBuffer) || b.buffer.Len() != 5 || !b.overflow {
		t.Fatalf("output bound failed: n=%d err=%v len=%d overflow=%v", n, err, b.buffer.Len(), b.overflow)
	}
}

func TestStatsCommandBoundsRealSubprocessOutput(t *testing.T) {
	for _, redirect := range []string{"", " >&2"} {
		t.Run("redirect="+redirect, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "container")
			// This runs through os/exec's io.Copy path, which must call
			// the bounded Write rather than a promoted bytes.Buffer.ReadFrom.
			script := "#!/bin/sh\ndd if=/dev/zero bs=1024 count=2048" + redirect + " 2>/dev/null\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := (&Client{Bin: bin}).statsCommand(ctx, "stats"); err == nil || !strings.Contains(err.Error(), "output exceeded limit") {
				t.Fatalf("subprocess output was not bounded: %v", err)
			}
		})
	}
}
