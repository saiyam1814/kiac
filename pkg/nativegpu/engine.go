package nativegpu

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Engine executes a fixed Metal self-test and, when explicitly configured, local
// llama.cpp inference. It never accepts a shader, executable, or model from a request.
type Engine interface {
	Capabilities(context.Context) (json.RawMessage, error)
	SelfTest(context.Context, int) (json.RawMessage, error)
	Chat(context.Context, json.RawMessage) (*http.Response, error)
	Close() error
}

type EngineOptions struct {
	Model          string
	ServerBinary   string
	CacheDir       string
	StartupTimeout time.Duration
}

var ErrInferenceUnavailable = errors.New("native inference is not configured or is unavailable")

//go:embed metal/MetalWorker.swift
var metalSource []byte

type nativeEngine struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cacheDir  string
	compileMu sync.Mutex
	helper    string
	llama     *llamaEngine
	closeOnce sync.Once
	workMu    sync.Mutex
	closing   bool
	work      sync.WaitGroup
}

// NewEngine supports Apple silicon macOS only. Both inference options must be
// explicit; omitting both enables only inventory and the bounded compute probe.
func NewEngine(ctx context.Context, options EngineOptions) (Engine, error) {
	return newEngine(ctx, options, true)
}

func newEngine(ctx context.Context, options EngineOptions, checkPlatform bool) (*nativeEngine, error) {
	if checkPlatform && (runtime.GOOS != "darwin" || runtime.GOARCH != "arm64") {
		return nil, errors.New("native GPU requires macOS on Apple silicon")
	}
	if (options.Model == "") != (options.ServerBinary == "") {
		return nil, errors.New("native inference requires both a local GGUF model and a llama-server executable")
	}
	if options.StartupTimeout == 0 {
		options.StartupTimeout = 2 * time.Minute
	}
	if options.StartupTimeout < time.Second || options.StartupTimeout > 10*time.Minute {
		return nil, errors.New("native inference startup timeout must be between one second and ten minutes")
	}
	cacheDir := options.CacheDir
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("locate native GPU cache: %w", err)
		}
		cacheDir = filepath.Join(base, "kiac")
	}
	cacheDir = filepath.Join(cacheDir, "native-gpu")
	if err := privateDirectory(cacheDir); err != nil {
		return nil, err
	}
	engineCtx, cancel := context.WithCancel(ctx)
	e := &nativeEngine{ctx: engineCtx, cancel: cancel, cacheDir: cacheDir}
	if options.Model != "" {
		llama, err := startLlama(engineCtx, options, cacheDir)
		if err != nil {
			cancel()
			return nil, err
		}
		e.llama = llama
	}
	return e, nil
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create private native GPU directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info) {
		return errors.New("native GPU directory must be an owned, private directory (mode 0700), not a symlink")
	}
	return nil
}

func (e *nativeEngine) Capabilities(ctx context.Context) (json.RawMessage, error) {
	result, err := e.runMetal(ctx, "capabilities", nil)
	if err != nil {
		return nil, err
	}
	var response map[string]any
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, errors.New("native Metal helper returned invalid capabilities")
	}
	inference := map[string]any{"configured": e.llama != nil}
	if e.llama != nil {
		inference["backend"] = "llama.cpp-metal"
		inference["deviceID"] = e.llama.device
		inference["ready"] = e.llama.alive()
		inference["offloadedLayers"] = e.llama.layers
		inference["totalLayers"] = e.llama.layers
		inference["contextTokens"] = 2048
		inference["maximumOutputTokens"] = 512
	}
	response["inference"] = inference
	return json.Marshal(response)
}

func (e *nativeEngine) SelfTest(ctx context.Context, count int) (json.RawMessage, error) {
	if count < 1 || count > 65536 {
		return nil, errors.New("self-test count must be between 1 and 65536")
	}
	input, _ := json.Marshal(map[string]int{"count": count})
	return e.runMetal(ctx, "self-test", input)
}

func (e *nativeEngine) Chat(ctx context.Context, request json.RawMessage) (*http.Response, error) {
	if e.llama == nil || !e.llama.alive() || e.ctx.Err() != nil {
		return nil, ErrInferenceUnavailable
	}
	return e.llama.chat(ctx, request)
}

// Health is an optional broker probe. Unlike initial startup proof, readiness is
// checked again against the live child, so a crashed model server is not reported
// ready forever. Compute-only mode verifies that the native device is available.
func (e *nativeEngine) Health(ctx context.Context) error {
	if e.ctx.Err() != nil {
		return errors.New("native GPU engine is closed")
	}
	if e.llama == nil {
		_, err := e.runMetal(ctx, "capabilities", nil)
		return err
	}
	ctx, cancel := mergedContext(ctx, e.ctx, 4*time.Second)
	defer cancel()
	if !e.llama.alive() || !e.llama.ready(ctx) {
		return ErrInferenceUnavailable
	}
	return nil
}

func (e *nativeEngine) Close() error {
	e.closeOnce.Do(func() {
		e.workMu.Lock()
		e.closing = true
		e.cancel()
		e.workMu.Unlock()
		if e.llama != nil {
			e.llama.close()
		}
		e.work.Wait()
	})
	return nil
}

func (e *nativeEngine) runMetal(ctx context.Context, operation string, input []byte) (json.RawMessage, error) {
	e.workMu.Lock()
	if e.closing || e.ctx.Err() != nil {
		e.workMu.Unlock()
		return nil, errors.New("native GPU engine is closed")
	}
	e.work.Add(1)
	e.workMu.Unlock()
	defer e.work.Done()
	// Compile has a separate deadline because Xcode's first module-cache fill can
	// take longer than one compute operation. It is still canceled with the caller.
	compileCtx, compileCancel := mergedContext(ctx, e.ctx, 90*time.Second)
	helper, err := e.metalHelper(compileCtx)
	compileCancel()
	if err != nil {
		return nil, err
	}
	ctx, cancel := mergedContext(ctx, e.ctx, 15*time.Second)
	defer cancel()
	cmd := managedCommand(ctx, helper, operation)
	cmd.Stdin = bytes.NewReader(input)
	out := &limitedOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	if err := cmd.Run(); err != nil || out.overflow || !json.Valid(out.Bytes()) {
		return nil, errors.New("native Metal operation failed; check GPU access and Xcode command line tools")
	}
	return append(json.RawMessage(nil), out.Bytes()...), nil
}

func (e *nativeEngine) metalHelper(ctx context.Context) (string, error) {
	e.compileMu.Lock()
	defer e.compileMu.Unlock()
	if e.helper != "" {
		return e.helper, nil
	}
	compiler := "/usr/bin/xcrun"
	version := &limitedOutput{limit: 4096}
	check := managedCommand(ctx, compiler, "swiftc", "--version")
	check.Stdout, check.Stderr = version, io.Discard
	if err := check.Run(); err != nil || version.overflow {
		return "", errors.New("native GPU requires Xcode command line tools with the Swift compiler")
	}
	hash := sha256.New()
	hash.Write(metalSource)
	hash.Write(version.Bytes())
	hash.Write([]byte(runtime.GOARCH))
	path := filepath.Join(e.cacheDir, "metal-"+hex.EncodeToString(hash.Sum(nil))[:24])
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
			return "", errors.New("native Metal cached executable must be an owned regular file with mode 0700")
		}
		e.helper = path
		return path, nil
	}
	tmp, err := os.MkdirTemp(e.cacheDir, ".metal-build-")
	if err != nil {
		return "", fmt.Errorf("create Metal build directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	source := filepath.Join(tmp, "MetalWorker.swift")
	if err := os.WriteFile(source, metalSource, 0600); err != nil {
		return "", err
	}
	output := filepath.Join(tmp, "worker")
	cmd := managedCommand(ctx, compiler, "swiftc", "-O", "-framework", "Metal", "-module-cache-path", filepath.Join(tmp, "modules"), source, "-o", output)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return "", errors.New("compile native Metal helper failed; verify Xcode command line tools are installed")
	}
	if err := os.Chmod(output, 0700); err != nil {
		return "", err
	}
	if err := os.Rename(output, path); err != nil {
		return "", fmt.Errorf("cache native Metal helper: %w", err)
	}
	e.helper = path
	return path, nil
}

// Native children inherit ordinary OS configuration, but not llama.cpp/GGML,
// loader injection, proxy, or remote-model configuration from a parent shell.
func engineEnvironment() []string {
	var result []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "LLAMA_") || strings.HasPrefix(upper, "GGML_") || strings.HasPrefix(upper, "DYLD_") || strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "HF_") || strings.Contains(upper, "PROXY") {
			continue
		}
		result = append(result, value)
	}
	return result
}

type limitedOutput struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *limitedOutput) Bytes() []byte { return b.buf.Bytes() }

func mergedContext(caller, lifetime context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(caller, timeout)
	stop := context.AfterFunc(lifetime, cancel)
	return ctx, func() { stop(); cancel() }
}

type llamaEngine struct {
	client *http.Client
	url    string
	key    string
	layers int
	device string
	cancel context.CancelFunc
	done   chan struct{}
	temp   string
	cmd    *exec.Cmd
}

var offloadLine = regexp.MustCompile(`offloaded ([0-9]+)/([0-9]+) layers to GPU`)

type startupLog struct {
	mu         sync.Mutex
	pending    string
	metal      bool
	layers     int
	invalid    bool
	tail       []string
	redactions []string
	complete   bool
}

func (l *startupLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.complete {
		return len(p), nil
	}
	l.pending += string(p)
	for {
		line, rest, found := strings.Cut(l.pending, "\n")
		if !found {
			// A misbehaving child cannot grow the in-memory log without bound.
			if len(l.pending) > 8192 {
				l.pending = l.pending[len(l.pending)-8192:]
			}
			break
		}
		l.pending = rest
		l.remember(line)
		if strings.Contains(line, "ggml_metal") && (strings.Contains(line, "Apple") || strings.Contains(line, "GPU name") || strings.Contains(line, "Metal")) {
			l.metal = true
		}
		if match := offloadLine.FindStringSubmatch(line); match != nil {
			n, _ := strconv.Atoi(match[1])
			total, _ := strconv.Atoi(match[2])
			if n > 0 && n == total {
				l.layers = n
			} else {
				l.invalid = true
			}
		}
	}
	return len(p), nil
}

// Keep only a small diagnostic tail, never the complete model metadata or any
// authentication configuration. This is used for local startup errors only.
func (l *startupLog) remember(line string) {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "api-key") || strings.Contains(lower, "api_key") || strings.Contains(lower, "authorization") || strings.Contains(lower, "bearer ") {
		line = "[private authentication configuration redacted]"
	}
	for _, value := range l.redactions {
		if value != "" {
			line = strings.ReplaceAll(line, value, "[redacted]")
		}
	}
	line = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, line)
	if len(line) > 512 {
		line = line[len(line)-512:]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	l.tail = append(l.tail, line)
	if len(l.tail) > 8 {
		l.tail = l.tail[len(l.tail)-8:]
	}
}

func (l *startupLog) diagnostic(prefix string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != "" {
		l.remember(l.pending)
		l.pending = ""
	}
	if len(l.tail) == 0 {
		return errors.New(prefix)
	}
	return fmt.Errorf("%s; llama-server: %s", prefix, strings.Join(l.tail, " | "))
}

func (l *startupLog) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.complete = true
	l.pending = ""
	l.tail = nil
	l.redactions = nil
}

func (l *startupLog) proof() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.layers, l.metal && l.layers > 0 && !l.invalid
}

func startLlama(ctx context.Context, options EngineOptions, cacheDir string) (_ *llamaEngine, resultErr error) {
	model, err := filepath.Abs(options.Model)
	if err != nil {
		return nil, err
	}
	file, err := openNativeModel(model)
	if err != nil {
		return nil, errors.New("open native inference local GGUF model failed")
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 4 || info.Size() > 16<<30 {
		file.Close()
		return nil, errors.New("native inference model must be a local regular GGUF file no larger than 16 GiB")
	}
	magic := make([]byte, 4)
	_, readErr := io.ReadFull(file, magic)
	file.Close()
	if readErr != nil || string(magic) != "GGUF" {
		return nil, errors.New("native inference model must be a local regular GGUF file no larger than 16 GiB")
	}
	binary, err := exec.LookPath(options.ServerBinary)
	if err != nil {
		return nil, errors.New("native inference llama-server executable was not found")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	device, err := discoverMetalDevice(ctx, binary)
	if err != nil {
		return nil, err
	}
	temp, err := os.MkdirTemp(cacheDir, ".llama-run-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			os.RemoveAll(temp)
		}
	}()
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := hex.EncodeToString(keyBytes)
	keyFile := filepath.Join(temp, "api-key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0600); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("reserve native inference loopback port failed")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	lifetime, cancel := context.WithCancel(ctx)
	cmd := managedCommand(lifetime, binary,
		"--model", model, "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--api-key-file", keyFile, "--device", device, "--n-gpu-layers", "999",
		"--ctx-size", "2048", "--parallel", "1", "--threads", "4", "--n-predict", "512",
		"--fit", "off", "--no-webui", "--no-slots", "--timeout", "180", "--alias", "kiac-native",
		// Recent llama.cpp builds hide library offload/Metal evidence at the
		// default level3. Trace4 emits the proof without debug/token logging.
		"--log-verbosity", "4", "--log-colors", "off")
	log := &startupLog{redactions: []string{key, keyFile}}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("start native inference llama-server failed")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: 30 * time.Second, MaxIdleConnsPerHost: 2}
	l := &llamaEngine{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, url: fmt.Sprintf("http://127.0.0.1:%d", port), key: key, device: device, cancel: cancel, done: make(chan struct{}), temp: temp, cmd: cmd}
	go func() {
		_ = cmd.Wait()
		// A wrapper may exit while its child still owns the listener. Also kill
		// the process group on a natural parent exit, before reporting it dead.
		killProcessGroup(cmd)
		transport.CloseIdleConnections()
		os.RemoveAll(temp)
		close(l.done)
	}()
	startup, startupCancel := context.WithTimeout(ctx, options.StartupTimeout)
	defer startupCancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-startup.Done():
			l.close()
			return nil, log.diagnostic(fmt.Sprintf("native inference did not become ready with full Metal offload: %v", startup.Err()))
		case <-l.done:
			cancel()
			return nil, log.diagnostic("native inference exited before full Metal offload and readiness; verify the local GGUF model and Metal-enabled llama-server flags/version")
		case <-tick.C:
			layers, valid := log.proof()
			if !valid {
				continue
			}
			// /health is intentionally unauthenticated upstream. /v1/models
			// additionally proves that this instance loaded our private key.
			if l.ready(startup) {
				l.layers = layers
				log.finish()
				return l, nil
			}
		}
	}
}

var metalDeviceLine = regexp.MustCompile(`(?m)^\s*((?:MTL|Metal)[0-9]*):\s+([^\r\n]+)$`)

func discoverMetalDevice(ctx context.Context, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output := &limitedOutput{limit: 64 << 10}
	cmd := managedCommand(ctx, binary, "--list-devices")
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	killProcessGroup(cmd)
	if err != nil || output.overflow {
		return "", errors.New("llama-server --list-devices failed; verify a Metal-enabled llama.cpp build and native GPU access")
	}
	for _, match := range metalDeviceLine.FindAllStringSubmatch(string(output.Bytes()), -1) {
		if strings.Contains(match[2], "Apple") {
			return match[1], nil
		}
	}
	return "", errors.New("llama-server did not list an Apple Metal GPU; inspect 'llama-server --list-devices' and verify native GPU access")
}

func (l *llamaEngine) alive() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func (l *llamaEngine) ready(ctx context.Context) bool {
	// Do not accidentally adopt an unrelated listener which raced the ephemeral
	// port reservation. The inference route must reject an unauthenticated probe.
	probe, cancel := context.WithTimeout(ctx, time.Second)
	request, _ := http.NewRequestWithContext(probe, http.MethodGet, l.url+"/v1/models", nil)
	response, err := l.client.Do(request)
	if err != nil {
		cancel()
		return false
	}
	response.Body.Close()
	cancel()
	if response.StatusCode != http.StatusUnauthorized {
		return false
	}
	for _, path := range []string{"/health", "/v1/models"} {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		request, _ := http.NewRequestWithContext(probe, http.MethodGet, l.url+path, nil)
		request.Header.Set("Authorization", "Bearer "+l.key)
		response, err := l.client.Do(request)
		if err != nil {
			cancel()
			return false
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
		response.Body.Close()
		cancel()
		if readErr != nil || response.StatusCode != http.StatusOK || len(body) > 65536 || !json.Valid(body) {
			return false
		}
	}
	return l.alive()
}

func (l *llamaEngine) chat(ctx context.Context, input json.RawMessage) (*http.Response, error) {
	if !json.Valid(input) || len(input) > 64<<10 {
		return nil, errors.New("native inference request must be valid JSON no larger than 64 KiB")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.url+"/v1/chat/completions", bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+l.key)
	request.Header.Set("Content-Type", "application/json")
	return l.client.Do(request)
}

func (l *llamaEngine) close() {
	l.cancel()
	<-l.done
}
