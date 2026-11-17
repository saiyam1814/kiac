package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/nativegpu"
)

func executeNativeCommand(t *testing.T, deps nativeCommandDependencies, ctx context.Context, args ...string) (string, string, error) {
	t.Helper()
	command := newGPUNativeCommandWithDependencies(deps)
	var output, diagnostics bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&diagnostics)
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	return output.String(), diagnostics.String(), err
}

func nativeTestResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestNativeCommandValidationBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{
		{"self-test", "--count=0"}, {"self-test", "--count=65537"},
		{"chat"}, {"chat", "--prompt=   "}, {"chat", "--prompt=test", "--max-tokens=513"},
		{"chat", "--prompt=" + strings.Repeat("a", 8193)},
		{"serve", "--startup-timeout=0s"}, {"serve", "--startup-timeout=11m"},
		{"serve", "--server-binary=llama-server"}, {"serve", "--model=   "},
		{"status", "unexpected"},
		{"install", "--name="}, {"uninstall", "--name=../invalid"},
	} {
		t.Run(strings.Join(args, " ")[:min(80, len(strings.Join(args, " ")))], func(t *testing.T) {
			if _, _, err := executeNativeCommand(t, nativeCommandDependencies{}, context.Background(), args...); err == nil {
				t.Fatalf("invalid args accepted: %v", args)
			}
		})
	}
}

func TestNativeCommandHelpAndCancellationDoNotAccessHost(t *testing.T) {
	for _, subcommand := range []string{"serve", "status", "self-test", "chat", "install", "uninstall"} {
		output, _, err := executeNativeCommand(t, nativeCommandDependencies{}, context.Background(), subcommand, "--help")
		if err != nil || !strings.Contains(output, subcommand) {
			t.Fatalf("help %s: %v", subcommand, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		args := []string{subcommand}
		if subcommand == "chat" {
			args = append(args, "--prompt=test")
		}
		if _, _, err := executeNativeCommand(t, nativeCommandDependencies{}, ctx, args...); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled %s accessed host or returned %v", subcommand, err)
		}
	}
}

func TestNativeSelfTestUsesPodCredentialsWithoutAdmin(t *testing.T) {
	t.Setenv("KIAC_METAL_CREDENTIALS", "/pod/credentials.json")
	deps := nativeCommandDependencies{
		readCredentials: func(path string) (nativegpu.Credentials, error) {
			if path != "/pod/credentials.json" {
				t.Fatalf("credentials=%s", path)
			}
			return nativegpu.Credentials{Token: "pod-secret"}, nil
		},
		adminCredentials: func(string) (nativegpu.Credentials, error) {
			t.Fatal("Pod client requested host admin credentials")
			return nativegpu.Credentials{}, nil
		},
		request: func(_ context.Context, credentials nativegpu.Credentials, method, path string, body any) (*http.Response, error) {
			if credentials.Token != "pod-secret" || method != http.MethodPost || path != "/v1/compute/vector-add" || !reflect.DeepEqual(body, map[string]int{"count": 17}) {
				t.Fatal("wrong leased request")
			}
			return nativeTestResponse(`{"verifiedElements":17,"maximumError":0}`), nil
		},
	}
	output, diagnostics, err := executeNativeCommand(t, deps, context.Background(), "self-test", "--count=17")
	if err != nil || !json.Valid([]byte(output)) || strings.Contains(output+diagnostics, "pod-secret") {
		t.Fatalf("output=%s diagnostics=%s error=%v", output, diagnostics, err)
	}
}

func TestNativeHostLeaseIsReleasedAfterCanceledRequest(t *testing.T) {
	t.Setenv("KIAC_METAL_CREDENTIALS", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	admin := nativegpu.Credentials{Token: strings.Repeat("a", 64), Endpoint: "https://127.0.0.1:5181", CAFile: "/host/ca.crt"}
	lease := nativegpu.Lease{ID: strings.Repeat("b", 64), Token: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(2 * time.Minute)}
	deps := nativeCommandDependencies{
		readCredentials:  func(string) (nativegpu.Credentials, error) { return nativegpu.Credentials{}, os.ErrNotExist },
		adminCredentials: func(string) (nativegpu.Credentials, error) { return admin, nil },
		request: func(requestCtx context.Context, credentials nativegpu.Credentials, method, path string, body any) (*http.Response, error) {
			calls = append(calls, method+" "+path)
			switch method {
			case http.MethodPost:
				if path == "/v1/leases" {
					request, ok := body.(nativegpu.LeaseRequest)
					if !ok || request.Namespace != "host" || request.NodeName != "mac" || request.TTLSeconds != 120 || !strings.HasPrefix(request.ClaimUID, "cli-") || credentials.Token != admin.Token {
						t.Fatal("invalid temporary lease identity")
					}
					encoded, _ := json.Marshal(lease)
					return nativeTestResponse(string(encoded)), nil
				}
				if credentials.Token != lease.Token {
					t.Fatal("compute request used admin credential")
				}
				cancel()
				return nil, context.Canceled
			case http.MethodDelete:
				if requestCtx.Err() != nil || credentials.Token != admin.Token || path != "/v1/leases/"+lease.ID {
					t.Fatal("cleanup reused canceled context or wrong credentials")
				}
				return nativeTestResponse(""), nil
			}
			return nil, errors.New("unexpected request")
		},
	}
	output, diagnostics, err := executeNativeCommand(t, deps, ctx, "self-test")
	if !errors.Is(err, context.Canceled) || len(calls) != 3 || !strings.HasPrefix(calls[2], "DELETE ") {
		t.Fatalf("error=%v calls=%v", err, calls)
	}
	if strings.Contains(output+diagnostics, lease.Token) || strings.Contains(output+diagnostics, admin.Token) {
		t.Fatal("credentials were printed")
	}
}

func TestNativeExplicitMissingCredentialsDoesNotFallBack(t *testing.T) {
	for _, useEnvironment := range []bool{false, true} {
		t.Run(map[bool]string{false: "flag", true: "environment"}[useEnvironment], func(t *testing.T) {
			t.Setenv("KIAC_METAL_CREDENTIALS", "")
			args := []string{"self-test", "--credentials=/missing/explicit.json"}
			if useEnvironment {
				t.Setenv("KIAC_METAL_CREDENTIALS", "/missing/explicit.json")
				args = []string{"self-test"}
			}
			deps := nativeCommandDependencies{readCredentials: func(string) (nativegpu.Credentials, error) { return nativegpu.Credentials{}, os.ErrNotExist }, adminCredentials: func(string) (nativegpu.Credentials, error) {
				t.Fatal("explicit missing credentials fell back to admin")
				return nativegpu.Credentials{}, nil
			}}
			if _, _, err := executeNativeCommand(t, deps, context.Background(), args...); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestNativeChatPassesBoundedRequestAndStream(t *testing.T) {
	t.Setenv("KIAC_METAL_CREDENTIALS", "/pod/credentials.json")
	for _, stream := range []bool{false, true} {
		deps := nativeCommandDependencies{readCredentials: func(string) (nativegpu.Credentials, error) { return nativegpu.Credentials{Token: "pod"}, nil }, request: func(_ context.Context, _ nativegpu.Credentials, method, path string, body any) (*http.Response, error) {
			request, ok := body.(nativegpu.ChatRequest)
			if !ok || method != http.MethodPost || path != "/v1/chat/completions" || request.Model != "kiac-native" || request.MaxTokens == nil || *request.MaxTokens != 32 || request.Stream != stream || len(request.Messages) != 1 || request.Messages[0].Content != "test" {
				t.Fatal("invalid chat request")
			}
			if stream {
				return nativeTestResponse("data: {\"content\":\"test\"}\n\ndata: [DONE]\n\n"), nil
			}
			return nativeTestResponse(`{"choices":[]}`), nil
		}}
		args := []string{"chat", "--prompt=test"}
		if stream {
			args = append(args, "--stream")
		}
		output, _, err := executeNativeCommand(t, deps, context.Background(), args...)
		if err != nil || output == "" {
			t.Fatalf("stream=%v error=%v output=%s", stream, err, output)
		}
	}
}

func TestNativeChatRejectsTruncatedStream(t *testing.T) {
	t.Setenv("KIAC_METAL_CREDENTIALS", "/pod/credentials.json")
	deps := nativeCommandDependencies{
		readCredentials: func(string) (nativegpu.Credentials, error) { return nativegpu.Credentials{Token: "pod"}, nil },
		request: func(context.Context, nativegpu.Credentials, string, string, any) (*http.Response, error) {
			return nativeTestResponse("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), nil
		},
	}
	output, _, err := executeNativeCommand(t, deps, context.Background(), "chat", "--prompt=test", "--stream")
	if err == nil || !strings.Contains(err.Error(), "[DONE]") || !strings.Contains(output, "partial") {
		t.Fatalf("truncated stream error=%v output=%s", err, output)
	}
}

func TestNativeEventStreamRequiresCompleteTerminalEvent(t *testing.T) {
	for _, sample := range []struct {
		name, input string
		valid       bool
	}{
		{"normal", "data: {\"content\":\"hello\"}\n\ndata: [DONE]\n\n", true},
		{"crlf", "event: completion\r\ndata:[DONE]\r\n\r\n", true},
		{"truncated", "data: {\"content\":\"partial\"}\n\n", false},
		{"unterminated", "data: [DONE]\n", false},
		{"embedded", "data: {\"content\":\"data: [DONE]\"}\n\n", false},
		{"multipledata", "data: [DONE]\ndata: more\n\n", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var output bytes.Buffer
			err := copyNativeEventStream(&output, strings.NewReader(sample.input), 1024)
			if (err == nil) != sample.valid {
				t.Fatalf("valid=%v error=%v", sample.valid, err)
			}
			if output.String() != sample.input {
				t.Fatal("stream bytes were changed")
			}
		})
	}
	var output bytes.Buffer
	if err := copyNativeEventStream(&output, strings.NewReader("data: "+strings.Repeat("x", 100)+"\n\n"), 32); err == nil || output.Len() > 32 {
		t.Fatal("oversized stream was accepted")
	}
}

type nativeCommandEngine struct{ closed bool }

func (*nativeCommandEngine) Capabilities(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"backend":"native-metal"}`), nil
}
func (*nativeCommandEngine) SelfTest(context.Context, int) (json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (*nativeCommandEngine) Chat(context.Context, json.RawMessage) (*http.Response, error) {
	return nil, errors.New("unused")
}
func (e *nativeCommandEngine) Close() error { e.closed = true; return nil }

func TestNativeServeLocksBeforeEngineAndCleansUp(t *testing.T) {
	for _, model := range []string{"", "/explicit/model.gguf"} {
		var calls []string
		engine := &nativeCommandEngine{}
		base := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		deps := nativeCommandDependencies{
			openState: func(dir, listen string) (*nativegpu.ServiceState, error) {
				calls = append(calls, "state")
				if dir != base || listen != "127.0.0.1:5181" {
					t.Fatal("wrong state/listener")
				}
				return &nativegpu.ServiceState{Dir: dir, Endpoint: "https://127.0.0.1:5181", AdminToken: strings.Repeat("s", 64)}, nil
			},
			newEngine: func(_ context.Context, options nativegpu.EngineOptions) (nativegpu.Engine, error) {
				calls = append(calls, "engine")
				if len(calls) != 2 || options.CacheDir != filepath.Join(base, "cache") || options.Model != model || options.StartupTimeout != 2*time.Minute {
					t.Fatal("wrong startup ordering/options")
				}
				if (model == "") != (options.ServerBinary == "") {
					t.Fatal("compute-only configured a model server")
				}
				return engine, nil
			},
			serve: func(ctx context.Context, _ *nativegpu.ServiceState, _ *nativegpu.Broker) error {
				calls = append(calls, "serve")
				cancel()
				<-ctx.Done()
				return nil
			},
		}
		args := []string{"serve", "--state-dir", base}
		if model != "" {
			args = append(args, "--model", model)
		}
		output, diagnostics, err := executeNativeCommand(t, deps, ctx, args...)
		cancel()
		if err != nil || !engine.closed || !reflect.DeepEqual(calls, []string{"state", "engine", "serve"}) || output != "" || strings.Contains(diagnostics, strings.Repeat("s", 64)) {
			t.Fatalf("error=%v calls=%v closed=%v output=%s diagnostics=%s", err, calls, engine.closed, output, diagnostics)
		}
	}
}

func TestNativeJSONOutputRejectsMalformedAndOversizedResponse(t *testing.T) {
	for _, body := range []string{"invalid", "{}{}", `{"value":"` + strings.Repeat("x", 100) + `"}`} {
		var output bytes.Buffer
		if err := copyNativeJSON(&output, strings.NewReader(body), 32); err == nil || output.Len() != 0 {
			t.Fatal("invalid response was written")
		}
	}
}

func TestNativeInstallAndUninstallUseClusterManager(t *testing.T) {
	base := t.TempDir()
	installed, removed := false, false
	deps := nativeCommandDependencies{
		install: func(ctx context.Context, options cluster.NativeGPUOptions) error {
			if ctx.Err() != nil || options.Cluster != "demo" || options.Node != "worker-2" || options.StateDir != base {
				t.Fatalf("invalid install options: %+v", options)
			}
			installed = true
			return nil
		},
		uninstall: func(ctx context.Context, name string) error {
			if ctx.Err() != nil || name != "demo" {
				t.Fatal("invalid uninstall")
			}
			removed = true
			return nil
		},
	}
	output, _, err := executeNativeCommand(t, deps, context.Background(), "install", "--name=demo", "--node=worker-2", "--state-dir", base)
	if err != nil || !installed || !strings.Contains(output, "alpha") {
		t.Fatalf("install error=%v output=%s", err, output)
	}
	output, _, err = executeNativeCommand(t, deps, context.Background(), "uninstall", "--name=demo")
	if err != nil || !removed || !strings.Contains(output, "removed") {
		t.Fatalf("uninstall error=%v output=%s", err, output)
	}
}
