// Command runtime-preflight validates an already installed Apple runtime.
// It never starts services, installs software, or prints runtime paths.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
)

const maxOutput = 64 << 10

var errOutputLimit = errors.New("command output limit exceeded")

type component struct {
	Version string `json:"version"`
	AppName string `json:"appName"`
}

// Deliberately omit paths, commits, resource inventories and other fields.
type statusPayload struct {
	Status string    `json:"status"`
	Client component `json:"client"`
	Server component `json:"server"`
	Host   struct {
		Architecture string `json:"architecture"`
	} `json:"host"`
}

type hostInfo struct {
	OS, Arch, Version string
}

type receipt struct {
	ClientVersion string `json:"clientVersion"`
	ServerVersion string `json:"serverVersion"`
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	OSVersion     string `json:"osVersion"`
}

func main() {
	expected := flag.String("expected-container-version", "", "required CLI and API server version (1.6.1 or 1.7.1)")
	stopped := flag.String("assert-stopped-nodes", "", "comma-separated owned node IDs that must be stopped")
	absentCluster := flag.String("assert-cluster-absent", "", "compatibility cluster name that must not exist")
	absentImage := flag.String("assert-image-absent", "", "test image reference that must not exist")
	kiacBin := flag.String("kiac-bin", "kiac", "Kiac executable for cluster inventory")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "runtime preflight takes no positional arguments")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := preflight(ctx, *expected, runtime.GOOS, runtime.GOARCH, runCommand)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runtime preflight:", err)
		os.Exit(1)
	}
	if *stopped != "" {
		nodes := strings.Split(*stopped, ",")
		for _, node := range nodes {
			if !regexp.MustCompile(`^kiac-ci-compat-[a-z0-9-]+-(control-plane|worker-1)$`).MatchString(node) {
				fmt.Fprintln(os.Stderr, "runtime preflight: stopped-state check requires owned compatibility node IDs")
				os.Exit(1)
			}
		}
		data, err := runCommand(ctx, "container", append([]string{"inspect"}, nodes...)...)
		if err != nil || validateStopped(data, nodes) != nil {
			fmt.Fprintln(os.Stderr, "runtime preflight: every owned VM must report stopped before resume")
			os.Exit(1)
		}
	}
	if *absentCluster != "" {
		data, err := runCommand(ctx, *kiacBin, "get", "clusters", "-o", "json")
		if err != nil || validateClusterAbsent(data, *absentCluster) != nil {
			fmt.Fprintln(os.Stderr, "runtime preflight: compatibility cluster name is unavailable or its inventory cannot be verified")
			os.Exit(1)
		}
		data, err = runCommand(ctx, "container", "ls", "--all", "--quiet")
		if err != nil || validateNodeNamesAbsent(data, *absentCluster) != nil {
			fmt.Fprintln(os.Stderr, "runtime preflight: compatibility node names are unavailable or runtime inventory cannot be verified")
			os.Exit(1)
		}
	}
	if *absentImage != "" {
		data, err := runCommand(ctx, "container", "image", "list", "--quiet")
		if err != nil || validateImageAbsent(data, *absentImage) != nil {
			fmt.Fprintln(os.Stderr, "runtime preflight: test image reference is unavailable or image inventory cannot be verified")
			os.Exit(1)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "runtime preflight: cannot write receipt")
		os.Exit(1)
	}
}

func validateClusterAbsent(data []byte, name string) error {
	var clusters []struct {
		Name string `json:"name"`
	}
	if len(data) > maxOutput || json.Unmarshal(data, &clusters) != nil || clusters == nil {
		return errors.New("invalid Kiac cluster inventory")
	}
	for _, cluster := range clusters {
		if cluster.Name == "" || cluster.Name == name {
			return errors.New("cluster name unavailable")
		}
	}
	return nil
}

func validateNodeNamesAbsent(data []byte, name string) error {
	if len(data) > maxOutput {
		return errOutputLimit
	}
	for _, node := range strings.Fields(string(data)) {
		if strings.HasPrefix(node, "kiac-"+name+"-") {
			return errors.New("node names unavailable")
		}
	}
	return nil
}

func validateImageAbsent(data []byte, name string) error {
	if len(data) > maxOutput {
		return errOutputLimit
	}
	want, err := reference.ParseDockerRef(name)
	if err != nil {
		return errors.New("invalid test image name")
	}
	for _, image := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if image == "" {
			continue
		}
		ref, err := reference.ParseDockerRef(image)
		if err != nil || ref.String() == want.String() {
			return errors.New("image reference unavailable or unreadable")
		}
	}
	return nil
}

func validateStopped(data []byte, nodes []string) error {
	var containers []struct {
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if len(data) > maxOutput || json.Unmarshal(data, &containers) != nil || len(containers) != len(nodes) {
		return errors.New("invalid inspected node states")
	}
	wanted := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		wanted[node] = true
	}
	for _, container := range containers {
		if !wanted[container.Configuration.ID] || container.Status.State != "stopped" {
			return errors.New("owned node is missing, duplicated or not stopped")
		}
		delete(wanted, container.Configuration.ID)
	}
	if len(wanted) != 0 {
		return errors.New("owned node is missing")
	}
	return nil
}

func preflight(ctx context.Context, expected, hostOS, arch string, run func(context.Context, string, ...string) ([]byte, error)) (receipt, error) {
	if expected != "1.6.1" && expected != "1.7.1" {
		return receipt{}, errors.New("expected container version must be explicitly set to 1.6.1 or 1.7.1")
	}
	if hostOS != "darwin" || arch != "arm64" {
		return receipt{}, errors.New("requires an Apple silicon Mac (darwin/arm64)")
	}
	osVersion, err := run(ctx, "sw_vers", "-productVersion")
	if err != nil {
		return receipt{}, errors.New("cannot read macOS version")
	}
	host := hostInfo{OS: hostOS, Arch: arch, Version: strings.TrimSpace(string(osVersion))}
	if err := validateHost(host); err != nil {
		return receipt{}, err
	}
	data, err := run(ctx, "container", "system", "status", "--format", "json")
	if err != nil {
		return receipt{}, errors.New("container system status failed, exceeded its output limit, or timed out; start the expected runtime separately")
	}
	return validateStatus(data, expected, host)
}

func validateHost(host hostInfo) error {
	if host.OS != "darwin" || host.Arch != "arm64" {
		return errors.New("requires an Apple silicon Mac (darwin/arm64)")
	}
	parts := strings.Split(host.Version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return errors.New("cannot validate macOS version; requires macOS 26 or newer")
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return errors.New("cannot validate macOS version; requires macOS 26 or newer")
		}
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 26 {
		return errors.New("requires macOS 26 or newer")
	}
	return nil
}

func validateStatus(data []byte, expected string, host hostInfo) (receipt, error) {
	if len(data) > maxOutput {
		return receipt{}, errors.New("container status exceeds output limit")
	}
	var status statusPayload
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&status); err != nil {
		return receipt{}, errors.New("container status is not a valid JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return receipt{}, errors.New("container status contains trailing data")
	}
	if status.Status != "running" {
		return receipt{}, errors.New("container API server is not running")
	}
	if status.Client.Version != expected || status.Server.Version != expected {
		return receipt{}, fmt.Errorf("CLI and running API server must both be container %s; mismatched or missing version", expected)
	}
	if status.Client.AppName != "container" || status.Server.AppName != "container-apiserver" {
		return receipt{}, errors.New("container status has missing or unexpected component identities")
	}
	if status.Host.Architecture != "arm64" {
		return receipt{}, errors.New("container API server must report arm64 host architecture")
	}
	return receipt{ClientVersion: expected, ServerVersion: expected, OS: host.OS, Architecture: host.Arch, OSVersion: host.Version}, nil
}

type limitedOutput struct {
	buffer   bytes.Buffer
	overflow bool
	cancel   context.CancelFunc
}

func (output *limitedOutput) Write(data []byte) (int, error) {
	if len(data) > maxOutput-output.buffer.Len() {
		output.overflow = true
		output.cancel()
		return 0, errOutputLimit
	}
	return output.buffer.Write(data)
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	output := &limitedOutput{cancel: cancel}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	// Bound inherited pipes even if a child process outlives its CLI.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if output.overflow {
		return nil, errOutputLimit
	}
	if err != nil {
		return nil, errors.New("command failed or timed out")
	}
	return output.buffer.Bytes(), nil
}
