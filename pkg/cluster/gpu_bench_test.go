package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLlamaBenchmark(t *testing.T) {
	raw := `[
  {"n_prompt":128,"n_gen":0,"avg_ts":321.5},
  {"n_prompt":0,"n_gen":64,"avg_ts":42.25}
]`
	result, err := parseLlamaBenchmark("venus", "Apple M1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.PromptTokens != 128 || result.PromptTokensPerSecond != 321.5 {
		t.Fatalf("prompt result = %#v", result)
	}
	if result.GenerateTokens != 64 || result.GenerateTokensPerSecond != 42.25 {
		t.Fatalf("generation result = %#v", result)
	}
}

func TestParseLlamaBenchmarkRequiresBothRows(t *testing.T) {
	if _, err := parseLlamaBenchmark("venus", "device", `[{"n_prompt":128,"avg_ts":1}]`); err == nil {
		t.Fatal("accepted output without generation result")
	}
}

func TestParseLlamaBenchmarkPreservesMetadata(t *testing.T) {
	raw := `[
  {"build_commit":"abc1234","build_number":123,"gpu_info":"Apple M1 Max","n_threads":4,"n_gpu_layers":99,"n_prompt":128,"n_gen":0,"avg_ts":321.5,"stddev_ts":1.25},
  {"build_commit":"abc1234","build_number":123,"gpu_info":"Apple M1 Max","n_threads":4,"n_gpu_layers":99,"n_prompt":0,"n_gen":64,"avg_ts":42.25,"stddev_ts":0.5}
]`
	result, err := parseLlamaBenchmark("host-metal", "Apple Metal", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"device":"Apple M1 Max"`,
		`"buildCommit":"abc1234"`,
		`"buildNumber":123`,
		`"threads":4`,
		`"gpuLayers":99`,
		`"promptStdDev":1.25`,
		`"generateStdDev":0.5`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("benchmark report missing %s: %s", want, encoded)
		}
	}
}

func TestParseLlamaBenchmarkRejectsDuplicateMeasurements(t *testing.T) {
	for _, raw := range []string{
		`[{"n_prompt":128,"avg_ts":1},{"n_prompt":256,"avg_ts":2},{"n_gen":64,"avg_ts":3}]`,
		`[{"n_prompt":128,"avg_ts":1},{"n_gen":64,"avg_ts":2},{"n_gen":128,"avg_ts":3}]`,
	} {
		if _, err := parseLlamaBenchmark("venus", "device", raw); err == nil {
			t.Errorf("silently discarded duplicate measurements: %s", raw)
		}
	}
}

func TestParseLlamaBenchmarkKeepsPartialMetadata(t *testing.T) {
	for _, metadataOnPrompt := range []bool{true, false} {
		prompt := `{"n_prompt":128,"avg_ts":1}`
		generation := `{"n_gen":64,"avg_ts":2}`
		metadata := `,"build_commit":"abc1234","build_number":123,"gpu_info":"Apple M1 Max","n_threads":4,"n_gpu_layers":99}`
		if metadataOnPrompt {
			prompt = strings.TrimSuffix(prompt, "}") + metadata
		} else {
			generation = strings.TrimSuffix(generation, "}") + metadata
		}
		result, err := parseLlamaBenchmark("host-metal", "Apple Metal", "["+prompt+","+generation+"]")
		if err != nil {
			t.Fatal(err)
		}
		if result.BuildCommit != "abc1234" || result.BuildNumber != 123 || result.Device != "Apple M1 Max" || result.Threads != 4 || result.GPULayers != 99 {
			t.Errorf("available metadata was erased: %+v", result)
		}
	}
}

func TestParseLlamaBenchmarkRejectsConflictingMetadata(t *testing.T) {
	for _, conflict := range []string{
		`"build_commit":"different"`,
		`"build_number":124`,
		`"gpu_info":"another GPU"`,
		`"n_threads":8`,
		`"n_gpu_layers":0`,
	} {
		raw := fmt.Sprintf(`[{"n_prompt":128,"avg_ts":1,"build_commit":"abc1234","build_number":123,"gpu_info":"Apple M1 Max","n_threads":4,"n_gpu_layers":99},{"n_gen":64,"avg_ts":2,%s}]`, conflict)
		if _, err := parseLlamaBenchmark("host-metal", "Apple Metal", raw); err == nil {
			t.Errorf("accepted conflicting metadata: %s", conflict)
		}
	}
}

func TestGPUBenchmarkUsesFixedThreads(t *testing.T) {
	if args := strings.Join(benchmarkArguments(), " "); !strings.Contains(args, "-t 4") {
		t.Fatalf("benchmark thread count depends on host/guest defaults: %s", args)
	}
}

func TestGPUBenchmarkPodContract(t *testing.T) {
	manifest := gpuBenchmarkPodManifest("test-run", 1200)
	if got := decodeAll(t, "gpu-benchmark", manifest); got != 1 {
		t.Fatalf("parsed %d YAML documents, want 1", got)
	}
	for _, want := range []string{
		"runtimeClassName: nvidia",
		"kiac.dev/gpu: \"1\"",
		"kiac.dev/gpu.present: \"true\"",
		"@sha256:",
		"GGML_VK_VISIBLE_DEVICES",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("benchmark manifest missing %q", want)
		}
	}
	if strings.Contains(manifest, "nvidia.com/gpu") || strings.Contains(manifest, ":latest") {
		t.Fatal("benchmark manifest contains a fake or unpinned GPU contract")
	}
}

func TestVenusDeviceLine(t *testing.T) {
	raw := "ggml_vulkan: 0 = Virtio-GPU Venus (Apple M1 Max) (venus) | uma: 1"
	if got := venusDeviceLine(raw); got != "Virtio-GPU Venus (Apple M1 Max)" {
		t.Fatalf("device = %q", got)
	}
}

func TestRunVenusBenchmarkOwnsPodInDefaultNamespace(t *testing.T) {
	for _, outcome := range []string{"success", "rejected", "lost-response", "malformed-name", "exec-failure", "cleanup-failure", "exec-and-cleanup-failure"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "kubectl.log")
			manifestPath := filepath.Join(dir, "pod.yaml")
			created := filepath.Join(dir, "created")
			t.Setenv("KIAC_BENCH_TEST_LOG", log)
			t.Setenv("KIAC_BENCH_TEST_MANIFEST", manifestPath)
			t.Setenv("KIAC_BENCH_TEST_CREATED", created)
			t.Setenv("KIAC_BENCH_TEST_OUTCOME", outcome)
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_BENCH_TEST_LOG"
case "$*" in
  *" create "*)
    /bin/cat > "$KIAC_BENCH_TEST_MANIFEST"
    if [ "$KIAC_BENCH_TEST_OUTCOME" = rejected ]; then exit 1; fi
    : > "$KIAC_BENCH_TEST_CREATED"
    if [ "$KIAC_BENCH_TEST_OUTCOME" = lost-response ]; then exit 1; fi
    if [ "$KIAC_BENCH_TEST_OUTCOME" = malformed-name ]; then printf 'unexpected-name'; else printf 'kiac-gpu-bench-generated'; fi
    ;;
  *" exec "*)
    case "$KIAC_BENCH_TEST_OUTCOME" in
      exec-failure|exec-and-cleanup-failure) printf 'benchmark execution failed\n' >&2; exit 1 ;;
    esac
    printf '%s\n' 'ggml_vulkan: 0 = Virtio-GPU Venus (Apple M1 Max) (venus) | uma: 1' >&2
    printf '%s\n' '[{"n_prompt":128,"n_gen":0,"avg_ts":321.5},{"n_prompt":0,"n_gen":64,"avg_ts":42.25}]'
    ;;
  *" delete pods -l "*)
    case "$KIAC_BENCH_TEST_OUTCOME" in
      cleanup-failure|exec-and-cleanup-failure) printf 'benchmark cleanup failed\n' >&2; exit 1 ;;
    esac
    /bin/rm -f "$KIAC_BENCH_TEST_CREATED"
    ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, runErr := runVenusBenchmark(ctx, "test", filepath.Join(dir, "model.gguf"))
			if (runErr != nil) != (outcome != "success") {
				t.Errorf("benchmark error = %v, outcome = %s", runErr, outcome)
			}
			if outcome == "exec-and-cleanup-failure" && (runErr == nil || !strings.Contains(runErr.Error(), "benchmark execution failed") || !strings.Contains(runErr.Error(), "benchmark cleanup failed")) {
				t.Errorf("benchmark did not preserve execution and cleanup failures: %v", runErr)
			}
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			runID := ""
			for _, line := range strings.Split(string(manifest), "\n") {
				if value, ok := strings.CutPrefix(strings.TrimSpace(line), "kiac.dev/gpu-bench-run: "); ok {
					runID = value
				}
			}
			if runID == "" || !strings.Contains(string(manifest), "activeDeadlineSeconds:") {
				t.Errorf("benchmark pod lacks per-run ownership or a deadline:\n%s", manifest)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			cleanup := "delete pods -l kiac.dev/gpu-bench-run=" + runID + " "
			for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if !strings.Contains(call, "--context kiac-test --namespace default ") {
					t.Errorf("command does not pin benchmark context and namespace: %s", call)
				}
				if strings.Contains(call, " delete ") && (runID == "" || !strings.Contains(call, cleanup)) {
					t.Errorf("benchmark cleanup is not scoped to this invocation: %s", call)
				}
			}
			if !strings.Contains(string(calls), cleanup) {
				t.Errorf("benchmark did not attempt ownership-scoped cleanup: %s", calls)
			}
			if !strings.Contains(outcome, "cleanup-failure") {
				if _, err := os.Stat(created); !os.IsNotExist(err) {
					t.Errorf("benchmark left its pod behind: %v", err)
				}
			}
			if outcome == "success" {
				for _, want := range []string{
					"pod/kiac-gpu-bench-generated",
					"default/kiac-gpu-bench-generated:/models/",
					"exec kiac-gpu-bench-generated --",
				} {
					if !strings.Contains(string(calls), want) {
						t.Errorf("benchmark commands missing %q:\n%s", want, calls)
					}
				}
			}
		})
	}
}
