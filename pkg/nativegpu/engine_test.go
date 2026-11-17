//go:build darwin || linux

package nativegpu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The fake executable is this test binary, not a model server or a GPU. It
// checks the actual child command, private key, HTTP auth, and process lifetime.
func TestNativeEngineProcess(t *testing.T) {
	mode := os.Getenv("KIAC_NATIVE_ENGINE_TEST")
	if mode == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(21)
	}
	if len(args) == 2 && args[1] == "--list-devices" {
		fmt.Fprintln(os.Stdout, "Available devices:\n  BLAS: Accelerate (0 MiB, 0 MiB free)")
		if mode != "no-device" {
			fmt.Fprintln(os.Stdout, "  MTL0: Apple test GPU (1024 MiB, 1024 MiB free)")
		}
		os.Exit(0)
	}
	flags := map[string]string{}
	for args = args[1:]; len(args) > 0; args = args[1:] {
		if len(args) > 1 && !strings.HasPrefix(args[1], "--") {
			flags[args[0]] = args[1]
			args = args[1:]
		} else {
			flags[args[0]] = ""
		}
	}
	for key, want := range map[string]string{"--host": "127.0.0.1", "--device": "MTL0", "--n-gpu-layers": "999", "--ctx-size": "2048", "--parallel": "1", "--n-predict": "512", "--fit": "off", "--log-verbosity": "4", "--log-colors": "off"} {
		if flags[key] != want {
			os.Exit(22)
		}
	}
	for _, key := range []string{"LLAMA_ARG_HOST", "LLAMA_ARG_MODEL_URL", "GGML_RPC_SERVERS", "HTTP_PROXY"} {
		if os.Getenv(key) != "" {
			os.Exit(23)
		}
	}
	keyBytes, err := os.ReadFile(flags["--api-key-file"])
	if err != nil {
		os.Exit(24)
	}
	keyInfo, err := os.Stat(flags["--api-key-file"])
	if err != nil || keyInfo.Mode().Perm() != 0600 {
		os.Exit(25)
	}
	if pidFile := os.Getenv("KIAC_NATIVE_ENGINE_PID"); pidFile != "" {
		child := exec.Command("/bin/sh", "-c", "exec sleep 60")
		if child.Start() != nil || os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
			os.Exit(26)
		}
	}
	if mode == "wait" {
		select {}
	}
	if mode == "orphan" {
		os.Exit(0)
	}
	if mode != "nonmetal" {
		fmt.Fprintln(os.Stderr, "ggml_metal_init: GPU name: Apple test GPU")
	}
	switch mode {
	case "partial":
		fmt.Fprintln(os.Stderr, "load_tensors: offloaded 22/23 layers to GPU")
	case "zero":
		fmt.Fprintln(os.Stderr, "load_tensors: offloaded 0/0 layers to GPU")
	default:
		fmt.Fprintln(os.Stderr, "load_tensors: offloaded 23/23 layers to GPU")
	}
	key := strings.TrimSpace(string(keyBytes))
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", flags["--port"]))
	if err != nil {
		os.Exit(27)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/health" {
			io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if mode != "unauthenticated" && r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"kiac-native"}]}`)
		case "/v1/chat/completions":
			if r.Header.Get("Content-Type") != "application/json" || r.Method != http.MethodPost {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if mode == "slow" {
				<-r.Context().Done()
				return
			}
			body, _ := io.ReadAll(r.Body)
			if !json.Valid(body) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"verified"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}), ReadHeaderTimeout: time.Second}
	_ = server.Serve(listener)
	os.Exit(0)
}

func fakeEngineOptions(t *testing.T, mode string) EngineOptions {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process-group lifecycle tests require Unix")
	}
	t.Setenv("KIAC_NATIVE_ENGINE_TEST", mode)
	base := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(base, "llama-server-test")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+quoted+" -test.run='^TestNativeEngineProcess$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(base, "model.gguf")
	if err := os.WriteFile(model, []byte("GGUFtest model fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return EngineOptions{Model: model, ServerBinary: launcher, CacheDir: base, StartupTimeout: time.Second}
}

func TestEngineManagedInference(t *testing.T) {
	options := fakeEngineOptions(t, "valid")
	t.Setenv("LLAMA_ARG_HOST", "0.0.0.0")
	t.Setenv("LLAMA_ARG_MODEL_URL", "https://invalid.example/model")
	t.Setenv("GGML_RPC_SERVERS", "unexpected")
	t.Setenv("HTTP_PROXY", "http://invalid.example")
	e, err := newEngine(context.Background(), options, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if e.llama.layers != 23 || !e.llama.alive() {
		t.Fatal("inference became ready without verified full offload")
	}
	if err := e.Health(context.Background()); err != nil {
		t.Fatalf("live managed server was unhealthy: %v", err)
	}
	response, err := e.Chat(context.Background(), json.RawMessage(`{"messages":[{"role":"user","content":"test"}],"max_tokens":8}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), "verified") {
		t.Fatalf("authenticated inference failed: %s, %v", body, err)
	}
	temp := e.llama.temp
	if err := e.Close(); err != nil || e.llama.alive() {
		t.Fatal("Close returned before child was reaped")
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private key directory survived shutdown")
	}
	if _, err := e.Chat(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, ErrInferenceUnavailable) {
		t.Fatal("closed engine accepted inference")
	}
	if err := e.Health(context.Background()); err == nil {
		t.Fatal("closed engine reported healthy")
	}
}

func TestEngineRejectsUnverifiedServer(t *testing.T) {
	for _, mode := range []string{"partial", "zero", "nonmetal", "unauthenticated", "no-device"} {
		t.Run(mode, func(t *testing.T) {
			options := fakeEngineOptions(t, mode)
			e, err := newEngine(context.Background(), options, false)
			if err == nil {
				e.Close()
				t.Fatal("unverified server was accepted")
			}
			paths, _ := filepath.Glob(filepath.Join(options.CacheDir, "native-gpu", ".llama-run-*"))
			if len(paths) != 0 {
				t.Fatal("failed startup leaked private key directory")
			}
		})
	}
}

func TestStartupDiagnosticsAreBoundedAndRedacted(t *testing.T) {
	key := strings.Repeat("s", 64)
	path := "/private/runtime/api-key"
	log := &startupLog{redactions: []string{key, path}}
	for i := 0; i < 100; i++ {
		_, _ = log.Write([]byte(fmt.Sprintf("old metadata %d %s\n", i, strings.Repeat("m", 1000))))
	}
	_, _ = log.Write([]byte("argument --api-key-file " + path + "\nAuthorization: Bearer " + key + "\nkey literal: " + key + "\nerror: unknown argument --unsupported"))
	message := log.diagnostic("startup failed").Error()
	if len(message) > 5000 || strings.Contains(message, key) || strings.Contains(message, path) || !strings.Contains(message, "unknown argument --unsupported") || strings.Contains(message, "old metadata 0 ") {
		t.Fatalf("unsafe or unhelpful diagnostic: %s", message)
	}
}

func TestStartupProofAcceptsActualBuild10809Trace(t *testing.T) {
	log := &startupLog{}
	// Real M1 Max startup lines from local llama.cpp build10809. Level3 emits
	// neither of these lines; the command explicitly requests tracelevel4.
	for _, part := range []string{
		"0.00.854.219 I load_tensors: offloaded 23/",
		"23 layers to GPU\n0.00.855.282 I ggml_metal_init: found device: Apple M1 Max\n",
	} {
		_, _ = log.Write([]byte(part))
	}
	if layers, ok := log.proof(); !ok || layers != 23 {
		t.Fatalf("actual Metal offload was rejected: layers=%d proof=%v", layers, ok)
	}
	log.finish()
	_, _ = log.Write([]byte("later inference request contents\n"))
	if len(log.tail) != 0 || log.pending != "" {
		t.Fatal("post-start inference logs were retained")
	}
}

func TestEngineCancellationStopsInference(t *testing.T) {
	options := fakeEngineOptions(t, "slow")
	ctx, cancel := context.WithCancel(context.Background())
	e, err := newEngine(ctx, options, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer requestCancel()
	if _, err := e.Chat(requestCtx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("inference ignored request cancellation")
	}
	cancel()
	select {
	case <-e.llama.done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine lifetime cancellation did not reap child")
	}
}

func TestEngineStopsGrandchildOnTimeoutAndParentExit(t *testing.T) {
	for _, mode := range []string{"wait", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			options := fakeEngineOptions(t, mode)
			pidPath := filepath.Join(options.CacheDir, "grandchild.pid")
			t.Setenv("KIAC_NATIVE_ENGINE_PID", pidPath)
			if e, err := newEngine(context.Background(), options, false); err == nil {
				e.Close()
				t.Fatal("nonready process was accepted")
			}
			value, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(value))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				err := syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("grandchild %d survived managed parent exit: %v", pid, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestEngineConfigurationAndPrivateCache(t *testing.T) {
	for _, options := range []EngineOptions{{Model: "model"}, {ServerBinary: "server"}, {StartupTimeout: -time.Second}, {StartupTimeout: 11 * time.Minute}} {
		options.CacheDir = t.TempDir()
		if e, err := newEngine(context.Background(), options, false); err == nil {
			e.Close()
			t.Fatal("invalid configuration accepted")
		}
	}
	base := t.TempDir()
	path := filepath.Join(base, "native-gpu")
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := newEngine(context.Background(), EngineOptions{CacheDir: base}, false); err == nil {
		t.Fatal("symlink cache accepted")
	}
	options := fakeEngineOptions(t, "valid")
	if err := os.WriteFile(options.Model, []byte("not a GGUF"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newEngine(context.Background(), options, false); err == nil {
		t.Fatal("non-GGUF model accepted")
	}
}

func TestEngineComputeOnly(t *testing.T) {
	e, err := newEngine(context.Background(), EngineOptions{CacheDir: t.TempDir()}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Chat(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, ErrInferenceUnavailable) {
		t.Fatal("compute-only engine accepted inference")
	}
	for _, count := range []int{-1, 0, 65537} {
		if _, err := e.SelfTest(context.Background(), count); err == nil {
			t.Fatal("unbounded self-test accepted")
		}
	}
}

func TestEngineCloseWaitsForMetalChild(t *testing.T) {
	e, err := newEngine(context.Background(), EngineOptions{CacheDir: t.TempDir()}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	pidPath := filepath.Join(e.cacheDir, "operation.pid")
	helper := filepath.Join(e.cacheDir, "test-metal-helper")
	quoted := "'" + strings.ReplaceAll(pidPath, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho $$ > "+quoted+"\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e.helper = helper
	result := make(chan error, 1)
	go func() { _, err := e.SelfTest(context.Background(), 1); result <- err }()
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for {
		if value, err := os.ReadFile(pidPath); err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(value)))
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("test helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.Close()
	if err := <-result; err == nil {
		t.Fatal("canceled Metal operation reported success")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Close returned before Metal helper was reaped: %v", err)
	}
}

func TestEngineRejectsFIFOWithoutBlockingAndAllowsModelSymlink(t *testing.T) {
	options := fakeEngineOptions(t, "valid")
	fifo := filepath.Join(options.CacheDir, "model.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	options.Model = fifo
	result := make(chan error, 1)
	go func() {
		e, err := newEngine(context.Background(), options, false)
		if e != nil {
			e.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular GGUF") {
			t.Fatalf("FIFO error=%v", err)
		}
	case <-time.After(time.Second):
		// Unblock a regressed blocking reader so the test itself does not leave
		// a stuck goroutine or FIFO handle behind on failure.
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_, _ = writer.Write([]byte("GGUF"))
			writer.Close()
		}
		t.Fatal("FIFO model blocked before startup timeout")
	}
	regular := filepath.Join(options.CacheDir, "regular.gguf")
	if err := os.WriteFile(regular, []byte("GGUFmodel fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(options.CacheDir, "linked.gguf")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	file, err := openNativeModel(link)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("legitimate GGUF symlink rejected: %v", err)
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic); err != nil || string(magic) != "GGUF" {
		t.Fatalf("symlink read failed: %s %v", magic, err)
	}
}
