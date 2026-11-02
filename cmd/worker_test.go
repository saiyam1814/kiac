package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/spf13/cobra"
)

type workerCLIService struct {
	cfg          cluster.ExternalWorkerConfig
	action, name string
	wait         time.Duration
}

func (s *workerCLIService) Join(cfg cluster.ExternalWorkerConfig) (cluster.ExternalWorkerState, error) {
	s.cfg = cfg
	return cluster.ExternalWorkerState{Name: cfg.Name, VMName: "owned-node", NodeIP: cfg.NodeIP}, nil
}
func (*workerCLIService) Workers() ([]cluster.ExternalWorkerInfo, error) { return nil, nil }
func (s *workerCLIService) Start(name string, wait time.Duration) error {
	s.action, s.name, s.wait = "start", name, wait
	return nil
}
func (s *workerCLIService) Stop(name string) error    { s.action, s.name = "stop", name; return nil }
func (s *workerCLIService) Delete(name string) error  { s.action, s.name = "delete", name; return nil }
func (*workerCLIService) Logs(string) (string, error) { return "redacted guest logs", nil }

func TestExternalWorkerCLIUsesDedicatedLifecycleAndPrivateFileInputs(t *testing.T) {
	oldFactory, oldCfg, oldWait, oldOutput := externalWorkerManager, externalWorkerCfg, externalWorkerStartWait, externalWorkerOutput
	t.Cleanup(func() {
		externalWorkerManager, externalWorkerCfg, externalWorkerStartWait, externalWorkerOutput = oldFactory, oldCfg, oldWait, oldOutput
	})
	service := &workerCLIService{}
	externalWorkerManager = func() externalWorkerService { return service }
	externalWorkerCfg = cluster.ExternalWorkerConfig{Name: "alpha", Server: "https://10.250.76.1:6443", TokenFile: "private-token", WireGuardFile: "private-wg", NodeIP: "10.250.76.2", K8sVersion: "1.37", WaitTimeout: time.Minute}
	if err := joinWorkerCmd.RunE(&cobra.Command{}, nil); err != nil {
		t.Fatal(err)
	}
	if service.cfg != externalWorkerCfg {
		t.Fatalf("CLI dropped join options: %+v", service.cfg)
	}
	for _, name := range []string{"name", "server", "token-file", "wireguard-config", "node-ip", "k8s-version"} {
		flag := joinWorkerCmd.Flags().Lookup(name)
		if flag == nil || len(flag.Annotations[cobra.BashCompOneRequiredFlag]) == 0 {
			t.Fatalf("missing required flag %s", name)
		}
	}
	for _, command := range []*cobra.Command{startWorkerCmd, stopWorkerCmd, deleteWorkerCmd, logsWorkerCmd} {
		if err := command.ValidateArgs(nil); err == nil {
			t.Fatalf("%s accepted missing name", command.CommandPath())
		}
		if err := command.ValidateArgs([]string{"alpha", "another"}); err == nil {
			t.Fatal("extra names accepted")
		}
	}
	externalWorkerStartWait = time.Minute
	for _, tc := range []struct {
		command *cobra.Command
		action  string
	}{{startWorkerCmd, "start"}, {stopWorkerCmd, "stop"}, {deleteWorkerCmd, "delete"}} {
		if err := tc.command.RunE(&cobra.Command{}, []string{"alpha"}); err != nil {
			t.Fatal(err)
		}
		if service.action != tc.action || service.name != "alpha" {
			t.Fatal("lifecycle command used wrong resource")
		}
	}
	if service.wait != time.Minute || !strings.Contains(deleteWorkerCmd.Long, "does not") || !strings.Contains(deleteWorkerCmd.Long, "remote Node") {
		t.Fatal("local-only lifecycle contract lost")
	}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	externalWorkerOutput = "json"
	if err := getWorkersCmd.RunE(command, nil); err != nil {
		t.Fatal(err)
	}
	var rows []cluster.ExternalWorkerInfo
	if err := json.Unmarshal(output.Bytes(), &rows); err != nil || rows == nil {
		t.Fatalf("empty JSON inventory=%q %v", output.String(), err)
	}
}

func TestExternalWorkerCLIRejectsInvalidWaitAndOutputBeforeBackend(t *testing.T) {
	oldFactory, oldCfg, oldWait, oldOutput := externalWorkerManager, externalWorkerCfg, externalWorkerStartWait, externalWorkerOutput
	t.Cleanup(func() {
		externalWorkerManager, externalWorkerCfg, externalWorkerStartWait, externalWorkerOutput = oldFactory, oldCfg, oldWait, oldOutput
	})
	externalWorkerManager = func() externalWorkerService { t.Fatal("backend created before CLI validation"); return nil }
	externalWorkerCfg.WaitTimeout = 0
	if err := joinWorkerCmd.RunE(&cobra.Command{}, nil); err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("zero wait=%v", err)
	}
	externalWorkerStartWait = -time.Second
	if err := startWorkerCmd.RunE(&cobra.Command{}, []string{"alpha"}); err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("negative wait=%v", err)
	}
	externalWorkerOutput = "yaml"
	if err := getWorkersCmd.RunE(&cobra.Command{}, nil); err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("bad output=%v", err)
	}
}
