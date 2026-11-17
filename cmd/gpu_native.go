package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/nativegpu"
	"github.com/spf13/cobra"
)

type nativeCommandDependencies struct {
	openState        func(string, string) (*nativegpu.ServiceState, error)
	newEngine        func(context.Context, nativegpu.EngineOptions) (nativegpu.Engine, error)
	serve            func(context.Context, *nativegpu.ServiceState, *nativegpu.Broker) error
	readCredentials  func(string) (nativegpu.Credentials, error)
	adminCredentials func(string) (nativegpu.Credentials, error)
	request          func(context.Context, nativegpu.Credentials, string, string, any) (*http.Response, error)
	install          func(context.Context, cluster.NativeGPUOptions) error
	uninstall        func(context.Context, string) error
}

func defaultNativeCommandDependencies() nativeCommandDependencies {
	return nativeCommandDependencies{
		openState: nativegpu.OpenServiceState,
		newEngine: nativegpu.NewEngine,
		serve: func(ctx context.Context, state *nativegpu.ServiceState, broker *nativegpu.Broker) error {
			return state.Serve(ctx, broker)
		},
		readCredentials:  nativegpu.ReadCredentials,
		adminCredentials: nativegpu.AdminCredentials,
		request:          nativegpu.Request,
		install: func(ctx context.Context, options cluster.NativeGPUOptions) error {
			return cluster.NewManager().InstallNativeGPU(ctx, options)
		},
		uninstall: func(ctx context.Context, name string) error {
			return cluster.NewManager().UninstallNativeGPU(ctx, name)
		},
	}
}

func newGPUNativeCommand() *cobra.Command {
	return newGPUNativeCommandWithDependencies(defaultNativeCommandDependencies())
}

func newGPUNativeCommandWithDependencies(deps nativeCommandDependencies) *cobra.Command {
	var stateDir string
	command := &cobra.Command{
		Use:   "native",
		Short: "Run the alpha native Apple GPU host service",
		Long:  "Run an opt-in native Metal service and call it from the Mac or a DRA-authorized Linux Pod. The host executes native GPU work; this does not expose a Metal device or CUDA inside Linux containers.",
	}
	command.PersistentFlags().StringVar(&stateDir, "state-dir", "", "private host service state directory (default: ~/.kiac/native-gpu)")
	command.AddCommand(newNativeServeCommand(&stateDir, deps), newNativeStatusCommand(&stateDir, deps), newNativeSelfTestCommand(&stateDir, deps), newNativeChatCommand(&stateDir, deps), newNativeInstallCommand(&stateDir, deps), newNativeUninstallCommand(deps))
	return command
}

func newNativeInstallCommand(stateDir *string, deps nativeCommandDependencies) *cobra.Command {
	var name, node string
	command := &cobra.Command{
		Use: "install", Short: "Install the alpha native Metal DRA driver on one owned node", Args: cobra.NoArgs,
		Long: "Attach one running cluster node to the native Metal host service using DRA (Kubernetes 1.36 or later). Start 'kiac gpu native serve' on a private Mac VM-network IP first. One exclusive lease covers the shared host service; native GPU execution and the optional model remain on macOS, outside Linux containers.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cluster.ValidName(name) {
				return errors.New("invalid cluster name")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return err
			}
			dir, err := resolveNativeStateDir(*stateDir)
			if err != nil {
				return err
			}
			if err := deps.install(ctx, cluster.NativeGPUOptions{Cluster: name, Node: node, StateDir: dir}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Native Metal DRA installed for cluster %s (alpha).\n", name)
			return err
		},
	}
	command.Flags().StringVar(&name, "name", "dev", "cluster name")
	command.Flags().StringVar(&node, "node", "", "running owned node or worker alias (default: first running worker)")
	return command
}

func newNativeUninstallCommand(deps nativeCommandDependencies) *cobra.Command {
	var name string
	command := &cobra.Command{
		Use: "uninstall", Short: "Remove Kiac's alpha native Metal DRA integration", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cluster.ValidName(name) {
				return errors.New("invalid cluster name")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := deps.uninstall(ctx, name); err != nil {
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Native Metal DRA removed from cluster %s.\n", name)
			return err
		},
	}
	command.Flags().StringVar(&name, "name", "dev", "cluster name")
	return command
}

func resolveNativeStateDir(value string) (string, error) {
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate native GPU service state: %w", err)
		}
		value = filepath.Join(home, ".kiac", "native-gpu")
	}
	return filepath.Abs(value)
}

func newNativeServeCommand(stateDir *string, deps nativeCommandDependencies) *cobra.Command {
	var listen, model, binary string
	var startupTimeout time.Duration
	command := &cobra.Command{
		Use: "serve", Short: "Start the native Metal service in the foreground", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if startupTimeout < time.Second || startupTimeout > 10*time.Minute {
				return errors.New("startup-timeout must be between one second and ten minutes")
			}
			if model == "" && cmd.Flags().Changed("server-binary") {
				return errors.New("--server-binary requires an explicit --model")
			}
			if model != "" && strings.TrimSpace(model) == "" {
				return errors.New("--model must name a local GGUF file")
			}
			dir, err := resolveNativeStateDir(*stateDir)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// Reserve the state lock and listening interface before starting any
			// GPU/model process. A duplicate serve cannot load another model.
			state, err := deps.openState(dir, listen)
			if err != nil {
				return err
			}
			defer state.Close()
			options := nativegpu.EngineOptions{Model: model, CacheDir: filepath.Join(dir, "cache"), StartupTimeout: startupTimeout}
			if model != "" {
				options.ServerBinary = binary
			}
			engine, err := deps.newEngine(ctx, options)
			if err != nil {
				return err
			}
			defer engine.Close()
			capabilities, err := engine.Capabilities(ctx)
			if err != nil {
				return err
			}
			broker, err := nativegpu.NewBroker(engine, state.AdminToken, capabilities, model != "")
			if err != nil {
				return err
			}
			defer broker.Close()
			fmt.Fprintf(cmd.ErrOrStderr(), "Native Metal service (alpha): %s; Ctrl-C to stop\n", state.Endpoint)
			return deps.serve(ctx, state, broker)
		},
	}
	command.Flags().StringVar(&listen, "listen", "127.0.0.1:5181", "explicit loopback or private Mac interface IP and port")
	command.Flags().StringVar(&model, "model", "", "explicit local GGUF model to enable native llama.cpp Metal inference")
	command.Flags().StringVar(&binary, "server-binary", "llama-server", "Metal-enabled llama-server executable (used only with --model)")
	command.Flags().DurationVar(&startupTimeout, "startup-timeout", 2*time.Minute, "maximum model startup time (1s to 10m)")
	return command
}

func newNativeStatusCommand(stateDir *string, deps nativeCommandDependencies) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Show the running host service health as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return err
			}
			dir, err := resolveNativeStateDir(*stateDir)
			if err != nil {
				return err
			}
			credentials, err := deps.adminCredentials(dir)
			if err != nil {
				return err
			}
			response, err := deps.request(ctx, credentials, http.MethodGet, "/v1/admin/health", nil)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return copyNativeJSON(cmd.OutOrStdout(), response.Body, 64<<10)
		},
	}
}

type nativeClientFlags struct {
	credentials     string
	explicitDefault bool
}

func addNativeClientFlags(command *cobra.Command) *nativeClientFlags {
	path := os.Getenv("KIAC_METAL_CREDENTIALS")
	flags := &nativeClientFlags{credentials: path, explicitDefault: path != ""}
	if path == "" {
		flags.credentials = "/var/run/kiac-metal/credentials.json"
	}
	command.Flags().StringVar(&flags.credentials, "credentials", flags.credentials, "DRA credentials file (or KIAC_METAL_CREDENTIALS); absent default uses a temporary host lease")
	return flags
}

func nativeOperationCredentials(ctx context.Context, command *cobra.Command, stateDir string, flags *nativeClientFlags, deps nativeCommandDependencies) (nativegpu.Credentials, func() error, error) {
	if flags.credentials == "" {
		return nativegpu.Credentials{}, nil, errors.New("--credentials cannot be empty")
	}
	credentials, err := deps.readCredentials(flags.credentials)
	if err == nil {
		return credentials, func() error { return nil }, nil
	}
	if !errors.Is(err, os.ErrNotExist) || flags.explicitDefault || command.Flags().Changed("credentials") {
		return nativegpu.Credentials{}, nil, err
	}
	dir, err := resolveNativeStateDir(stateDir)
	if err != nil {
		return nativegpu.Credentials{}, nil, err
	}
	admin, err := deps.adminCredentials(dir)
	if err != nil {
		return nativegpu.Credentials{}, nil, err
	}
	randomID := make([]byte, 16)
	if _, err := rand.Read(randomID); err != nil {
		return nativegpu.Credentials{}, nil, err
	}
	response, err := deps.request(ctx, admin, http.MethodPost, "/v1/leases", nativegpu.LeaseRequest{Namespace: "host", ClaimUID: "cli-" + hex.EncodeToString(randomID), NodeName: "mac", TTLSeconds: 120})
	if err != nil {
		return nativegpu.Credentials{}, nil, err
	}
	var lease nativegpu.Lease
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 16385))
	response.Body.Close()
	if readErr != nil || len(body) > 16384 || json.Unmarshal(body, &lease) != nil {
		return nativegpu.Credentials{}, nil, errors.New("native GPU service returned an invalid temporary lease")
	}
	id, idErr := hex.DecodeString(lease.ID)
	if idErr != nil || len(id) != 32 || len(lease.Token) < 32 || !time.Now().Before(lease.ExpiresAt) {
		return nativegpu.Credentials{}, nil, errors.New("native GPU service returned an incomplete temporary lease")
	}
	credentials = nativegpu.Credentials{Version: 1, Endpoint: admin.Endpoint, CAFile: admin.CAFile, Token: lease.Token, LeaseID: lease.ID, ExpiresAt: lease.ExpiresAt}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		response, err := deps.request(cleanupCtx, admin, http.MethodDelete, "/v1/leases/"+lease.ID, nil)
		if err != nil {
			return fmt.Errorf("release temporary native GPU lease: %w", err)
		}
		return response.Body.Close()
	}
	return credentials, cleanup, nil
}

func newNativeSelfTestCommand(stateDir *string, deps nativeCommandDependencies) *cobra.Command {
	var count int
	var flags *nativeClientFlags
	command := &cobra.Command{
		Use: "self-test", Short: "Run and verify fixed vector addition on the native Apple GPU", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (resultErr error) {
			if count < 1 || count > 65536 {
				return errors.New("count must be between 1 and 65536")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := ctx.Err(); err != nil {
				return err
			}
			credentials, cleanup, err := nativeOperationCredentials(ctx, cmd, *stateDir, flags, deps)
			if err != nil {
				return err
			}
			defer func() {
				if err := cleanup(); resultErr == nil {
					resultErr = err
				}
			}()
			response, err := deps.request(ctx, credentials, http.MethodPost, "/v1/compute/vector-add", map[string]int{"count": count})
			if err != nil {
				return err
			}
			defer response.Body.Close()
			return copyNativeJSON(cmd.OutOrStdout(), response.Body, 64<<10)
		},
	}
	command.Flags().IntVar(&count, "count", 65536, "number of values to compute and verify (1 to 65536)")
	flags = addNativeClientFlags(command)
	return command
}

func newNativeChatCommand(stateDir *string, deps nativeCommandDependencies) *cobra.Command {
	var prompt string
	var maxTokens int
	var stream bool
	var flags *nativeClientFlags
	command := &cobra.Command{
		Use: "chat", Short: "Send a bounded text request to native Metal inference", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (resultErr error) {
			if strings.TrimSpace(prompt) == "" || len(prompt) > 8192 {
				return errors.New("prompt must contain text and be no larger than 8192 bytes")
			}
			if maxTokens < 1 || maxTokens > 512 {
				return errors.New("max-tokens must be between 1 and 512")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return err
			}
			credentials, cleanup, err := nativeOperationCredentials(ctx, cmd, *stateDir, flags, deps)
			if err != nil {
				return err
			}
			defer func() {
				if err := cleanup(); resultErr == nil {
					resultErr = err
				}
			}()
			response, err := deps.request(ctx, credentials, http.MethodPost, "/v1/chat/completions", nativegpu.ChatRequest{Model: "kiac-native", Messages: []nativegpu.ChatMessage{{Role: "user", Content: prompt}}, MaxTokens: &maxTokens, Stream: stream})
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if !stream {
				return copyNativeJSON(cmd.OutOrStdout(), response.Body, 4<<20)
			}
			return copyNativeEventStream(cmd.OutOrStdout(), response.Body, 4<<20)
		},
	}
	command.Flags().StringVar(&prompt, "prompt", "", "text prompt (required, up to 8192 bytes)")
	command.Flags().IntVar(&maxTokens, "max-tokens", 32, "maximum generated tokens (1 to 512)")
	command.Flags().BoolVar(&stream, "stream", false, "write the server-sent event stream as it arrives")
	flags = addNativeClientFlags(command)
	return command
}

// A successful HTTP200 is not proof that streamed generation completed. A
// crashed upstream can end its body cleanly after only some tokens; require the
// terminal OpenAI SSE event while forwarding complete lines as they arrive.
func copyNativeEventStream(output io.Writer, input io.Reader, maximum int64) error {
	reader := bufio.NewReader(io.LimitReader(input, maximum+1))
	var written int64
	dataFields, terminal := 0, false
	for {
		line, err := reader.ReadString('\n')
		if written+int64(len(line)) > maximum {
			return errors.New("native inference stream exceeded its output limit")
		}
		if line != "" {
			if _, writeErr := io.WriteString(output, line); writeErr != nil {
				return writeErr
			}
			written += int64(len(line))
			field := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if field == "" {
				if terminal && dataFields == 1 {
					return nil
				}
				dataFields, terminal = 0, false
			} else if strings.HasPrefix(field, "data:") {
				dataFields++
				value := strings.TrimPrefix(field, "data:")
				value = strings.TrimPrefix(value, " ")
				terminal = dataFields == 1 && value == "[DONE]"
			}
		}
		if err != nil {
			if err == io.EOF {
				return errors.New("native inference stream ended before the terminal [DONE] event")
			}
			return fmt.Errorf("native inference stream ended before completion: %w", err)
		}
	}
}

func copyNativeJSON(output io.Writer, input io.Reader, maximum int64) error {
	body, err := io.ReadAll(io.LimitReader(input, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maximum || !json.Valid(body) {
		return errors.New("native GPU service returned invalid or oversized JSON")
	}
	if _, err := output.Write(body); err != nil {
		return err
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		_, err = io.WriteString(output, "\n")
	}
	return err
}
