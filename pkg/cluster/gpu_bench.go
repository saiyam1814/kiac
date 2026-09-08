package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultGPUBenchmarkModelFile = "tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf"
	defaultGPUBenchmarkModelSHA  = "9fecc3b3cd76bba89d504f29b616eedf7da85b96540e490ca5824d3f7d2776a0"
	defaultGPUBenchmarkModelURL  = "https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/52e7645ba7c309695bec7ac98f4f005b139cf465/" + defaultGPUBenchmarkModelFile
	gpuBenchmarkImage            = "quay.io/slopezpa/fedora-vgpu-llama@sha256:f58a677fd617e5a6d0b7f558fd137d63a57aaac70293956ef09a16ec69e210d4"
	gpuBenchmarkPod              = "kiac-gpu-bench"
	gpuBenchmarkRunLabel         = gpuResourceDomain + "/gpu-bench-run"
)

// GPUBenchmarkOptions controls the comparable host Metal and pod Venus run.
type GPUBenchmarkOptions struct {
	Cluster  string
	Model    string
	SkipHost bool
	Timeout  time.Duration
}

// GPUBenchmarkMeasurement is one llama-bench backend result.
type GPUBenchmarkMeasurement struct {
	Backend                 string  `json:"backend"`
	Device                  string  `json:"device"`
	BuildCommit             string  `json:"buildCommit,omitempty"`
	BuildNumber             int     `json:"buildNumber,omitempty"`
	Threads                 int     `json:"threads,omitempty"`
	GPULayers               int     `json:"gpuLayers,omitempty"`
	PromptTokens            int     `json:"promptTokens"`
	PromptTokensPerSecond   float64 `json:"promptTokensPerSecond"`
	PromptStdDev            float64 `json:"promptStdDev,omitempty"`
	GenerateTokens          int     `json:"generateTokens"`
	GenerateTokensPerSecond float64 `json:"generateTokensPerSecond"`
	GenerateStdDev          float64 `json:"generateStdDev,omitempty"`
}

// GPUBenchmarkReport records the exact artifacts and results used by
// `kiac gpu bench`. Host is absent when llama-bench is unavailable or skipped.
type GPUBenchmarkReport struct {
	Cluster        string                   `json:"cluster"`
	Model          string                   `json:"model"`
	ModelSHA256    string                   `json:"modelSHA256,omitempty"`
	ContainerImage string                   `json:"containerImage"`
	Host           *GPUBenchmarkMeasurement `json:"host,omitempty"`
	HostSkipped    string                   `json:"hostSkipped,omitempty"`
	Kubernetes     GPUBenchmarkMeasurement  `json:"kubernetes"`
}

type llamaBenchRow struct {
	BuildCommit *string `json:"build_commit"`
	BuildNumber *int    `json:"build_number"`
	GPUInfo     *string `json:"gpu_info"`
	Threads     *int    `json:"n_threads"`
	GPULayers   *int    `json:"n_gpu_layers"`
	Prompt      int     `json:"n_prompt"`
	Gen         int     `json:"n_gen"`
	Avg         float64 `json:"avg_ts"`
	StdDev      float64 `json:"stddev_ts"`
}

// RunGPUBenchmark executes the same small, fixed llama-bench workload on the
// host and in a real GPU pod. The pod result is accepted only when llama.cpp
// explicitly discovers the Venus device.
func (m *Manager) RunGPUBenchmark(ctx context.Context, opts GPUBenchmarkOptions) (GPUBenchmarkReport, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if _, err := m.GPUStatusForCluster(opts.Cluster); err != nil {
		return GPUBenchmarkReport{}, err
	}
	model, pinned, err := resolveGPUBenchmarkModel(opts.Model)
	if err != nil {
		return GPUBenchmarkReport{}, err
	}
	report := GPUBenchmarkReport{
		Cluster:        opts.Cluster,
		Model:          model,
		ContainerImage: gpuBenchmarkImage,
	}
	if pinned {
		report.ModelSHA256 = defaultGPUBenchmarkModelSHA
	}

	if opts.SkipHost {
		report.HostSkipped = "disabled by --skip-host"
	} else if hostBench, lookupErr := exec.LookPath("llama-bench"); lookupErr != nil {
		report.HostSkipped = "llama-bench is not installed on the host"
	} else {
		measurement, benchErr := runLlamaBenchmark(ctx, hostBench, []string{"-m", model}, "host-metal", "Apple Metal")
		if benchErr != nil {
			report.HostSkipped = benchErr.Error()
		} else {
			report.Host = &measurement
		}
	}

	measurement, err := runVenusBenchmark(ctx, opts.Cluster, model)
	if err != nil {
		return report, err
	}
	report.Kubernetes = measurement
	return report, nil
}

func resolveGPUBenchmarkModel(path string) (string, bool, error) {
	if path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return "", false, fmt.Errorf("reading benchmark model: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf("benchmark model %q is not a regular file", path)
		}
		absolute, err := filepath.Abs(path)
		return absolute, false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolving benchmark model cache: %w", err)
	}
	destination := filepath.Join(home, ".kiac", "models", defaultGPUBenchmarkModelFile)
	if err := verifyFileSHA256(destination, defaultGPUBenchmarkModelSHA); err != nil {
		if err := downloadVerified(defaultGPUBenchmarkModelURL, destination, defaultGPUBenchmarkModelSHA); err != nil {
			return "", false, fmt.Errorf("downloading GPU benchmark model: %w", err)
		}
	}
	return destination, true, nil
}

func runVenusBenchmark(ctx context.Context, cluster, model string) (measurement GPUBenchmarkMeasurement, retErr error) {
	if err := ctx.Err(); err != nil {
		return measurement, err
	}
	runID := strings.ToLower(rand.Text())
	contextArgs := []string{"--context", "kiac-" + cluster, "--namespace", "default"}
	runKubectl := func(stdin []byte, args ...string) (string, string, error) {
		commandArgs := append(append([]string{}, contextArgs...), args...)
		cmd := exec.CommandContext(ctx, "kubectl", commandArgs...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), stderr.String(), nil
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args := append(append([]string{}, contextArgs...), "delete", "pods", "-l", gpuBenchmarkRunLabel+"="+runID, "--ignore-not-found", "--wait=false")
		cmd := exec.CommandContext(cleanupCtx, "kubectl", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleaning up benchmark run %s: %w: %s", runID, err, strings.TrimSpace(string(output))))
		}
	}()
	lifetime := int64(20 * 60)
	if deadline, ok := ctx.Deadline(); ok {
		lifetime = max(1, int64(math.Ceil(time.Until(deadline).Seconds())))
	}
	manifest := gpuBenchmarkPodManifest(runID, lifetime)
	created, _, err := runKubectl([]byte(manifest), "create", "-f", "-", "-o", "jsonpath={.metadata.name}")
	if err != nil {
		return measurement, err
	}
	pod := strings.TrimSpace(created)
	if !strings.HasPrefix(pod, gpuBenchmarkPod+"-") || !ValidName(pod) {
		return measurement, fmt.Errorf("unexpected benchmark pod name %q", pod)
	}

	if _, _, err := runKubectl(nil, "wait", "--for=condition=Ready", "pod/"+pod, "--timeout=5m"); err != nil {
		return measurement, err
	}
	if _, _, err := runKubectl(nil, "cp", model, "default/"+pod+":/models/"+defaultGPUBenchmarkModelFile); err != nil {
		return measurement, err
	}

	args := []string{"exec", pod, "--", "/usr/bin/llama-bench", "-m", "/models/" + defaultGPUBenchmarkModelFile}
	stdout, stderr, err := runKubectl(nil, append(args, benchmarkArguments()...)...)
	if err != nil {
		return measurement, err
	}
	if !strings.Contains(stderr, "Virtio-GPU Venus") {
		return measurement, fmt.Errorf("llama-bench did not discover the real Venus device; output: %s", strings.TrimSpace(stderr))
	}
	return parseLlamaBenchmark("kubernetes-venus", venusDeviceLine(stderr), stdout)
}

func runLlamaBenchmark(ctx context.Context, binary string, prefix []string, backend, device string) (GPUBenchmarkMeasurement, error) {
	args := append(append([]string{}, prefix...), benchmarkArguments()...)
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return GPUBenchmarkMeasurement{}, ctx.Err()
		}
		return GPUBenchmarkMeasurement{}, fmt.Errorf("%s failed: %w: %s", backend, err, strings.TrimSpace(stderr.String()))
	}
	backendLog := strings.ToLower(stderr.String())
	if backend == "host-metal" && !strings.Contains(backendLog, "metal") && !strings.Contains(backendLog, "mtl") {
		return GPUBenchmarkMeasurement{}, fmt.Errorf("llama-bench completed without reporting a Metal backend")
	}
	return parseLlamaBenchmark(backend, device, stdout.String())
}

func benchmarkArguments() []string {
	return []string{"-p", "128", "-n", "64", "-r", "3", "-t", "4", "-ngl", "99", "-o", "json"}
}

func parseLlamaBenchmark(backend, device, raw string) (GPUBenchmarkMeasurement, error) {
	var rows []llamaBenchRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return GPUBenchmarkMeasurement{}, fmt.Errorf("parse %s llama-bench output: %w", backend, err)
	}
	result := GPUBenchmarkMeasurement{Backend: backend, Device: device}
	var metadata llamaBenchRow
	for _, row := range rows {
		switch {
		case row.Prompt > 0 && row.Gen == 0:
			if result.PromptTokens != 0 {
				return GPUBenchmarkMeasurement{}, fmt.Errorf("%s llama-bench output contains multiple prompt measurements", backend)
			}
			result.PromptTokens = row.Prompt
			result.PromptTokensPerSecond = row.Avg
			result.PromptStdDev = row.StdDev
		case row.Gen > 0 && row.Prompt == 0:
			if result.GenerateTokens != 0 {
				return GPUBenchmarkMeasurement{}, fmt.Errorf("%s llama-bench output contains multiple generation measurements", backend)
			}
			result.GenerateTokens = row.Gen
			result.GenerateTokensPerSecond = row.Avg
			result.GenerateStdDev = row.StdDev
		default:
			continue
		}
		if !mergeBenchmarkField(&metadata.BuildCommit, row.BuildCommit) ||
			!mergeBenchmarkField(&metadata.BuildNumber, row.BuildNumber) ||
			!mergeBenchmarkField(&metadata.GPUInfo, row.GPUInfo) ||
			!mergeBenchmarkField(&metadata.Threads, row.Threads) ||
			!mergeBenchmarkField(&metadata.GPULayers, row.GPULayers) {
			return GPUBenchmarkMeasurement{}, fmt.Errorf("%s llama-bench output contains inconsistent build, device, thread, or GPU-layer metadata", backend)
		}
	}
	if metadata.GPUInfo != nil && *metadata.GPUInfo != "" {
		result.Device = *metadata.GPUInfo
	}
	if metadata.BuildCommit != nil {
		result.BuildCommit = *metadata.BuildCommit
	}
	if metadata.BuildNumber != nil {
		result.BuildNumber = *metadata.BuildNumber
	}
	if metadata.Threads != nil {
		result.Threads = *metadata.Threads
	}
	if metadata.GPULayers != nil {
		result.GPULayers = *metadata.GPULayers
	}
	if result.PromptTokensPerSecond <= 0 || result.GenerateTokensPerSecond <= 0 {
		return GPUBenchmarkMeasurement{}, fmt.Errorf("%s llama-bench output did not contain prompt and generation results", backend)
	}
	return result, nil
}

func mergeBenchmarkField[T comparable](current **T, next *T) bool {
	if next == nil {
		return true
	}
	if *current != nil && **current != *next {
		return false
	}
	*current = next
	return true
}

func venusDeviceLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if index := strings.Index(line, "Virtio-GPU Venus"); index >= 0 {
			device, _, _ := strings.Cut(line[index:], " |")
			return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(device), " (venus)"))
		}
	}
	return "Virtio-GPU Venus"
}

func gpuBenchmarkPodManifest(runID string, lifetime int64) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  generateName: %s-
  namespace: default
  labels:
    app.kubernetes.io/name: kiac-gpu-bench
    app.kubernetes.io/part-of: kiac
    %s: %s
spec:
  activeDeadlineSeconds: %d
  restartPolicy: Never
  runtimeClassName: nvidia
  nodeSelector:
    kiac.dev/gpu.present: "true"
  tolerations:
    - key: kiac.dev/gpu
      operator: Equal
      value: "true"
      effect: NoSchedule
  containers:
    - name: bench
      image: %s
      imagePullPolicy: IfNotPresent
      command: [sh, -c, "sleep %d"]
      env:
        - name: XDG_RUNTIME_DIR
          value: /tmp
        - name: GGML_VK_VISIBLE_DEVICES
          value: "0"
      resources:
        requests:
          cpu: "2"
          memory: 2Gi
          kiac.dev/gpu: "1"
        limits:
          memory: 4Gi
          kiac.dev/gpu: "1"
      volumeMounts:
        - name: model
          mountPath: /models
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: model
      emptyDir: {}
    - name: tmp
      emptyDir: {}
`, gpuBenchmarkPod, gpuBenchmarkRunLabel, runID, lifetime, gpuBenchmarkImage, lifetime)
}
