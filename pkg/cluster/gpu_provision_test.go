package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestGPUProvisioningBoundsUploadsAndGuestScript(t *testing.T) {
	artifact := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(artifact, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, distro := range []string{"k3s", "kubeadm"} {
		for _, wait := range []time.Duration{time.Millisecond, 5 * time.Minute, 20 * time.Minute} {
			rt := &recordingRuntime{}
			m := &Manager{rt: rt}
			var err error
			wantUploads := 2
			if distro == "k3s" {
				err = m.provisionK3sGPUNode("gpu", k3sGPUArtifacts{Binary: artifact, SELinux: artifact}, true, wait)
			} else {
				wantUploads = 3
				err = m.provisionKubeadmGPUNode("gpu", "192.168.120.2", map[string]string{"kubeadm": artifact, "kubelet": artifact, "kubectl": artifact}, true, wait)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rt.unbounded) != 0 || len(rt.transfers) != wantUploads || len(rt.execs) != 1 {
				t.Fatalf("%s did not bound every step: %+v", distro, rt)
			}
			budget := max(wait, time.Minute)
			for _, upload := range rt.transfers {
				if upload.timeout != budget {
					t.Fatalf("upload timeout=%s want=%s", upload.timeout, budget)
				}
			}
			execution := rt.execs[0]
			if execution.timeout != max(wait, 10*time.Minute)+15*time.Second {
				t.Fatalf("host timeout=%s", execution.timeout)
			}
			args := execution.command
			if len(args) != 7 || args[0] != "timeout" || args[1] != "--signal=TERM" || args[2] != "--kill-after=10s" || args[4] != "sh" || args[5] != "-euc" || !strings.Contains(args[6], "dnf -y --setopt=ip_resolve=4 install") {
				t.Fatalf("guest process group is not bounded: %v", args[:len(args)-1])
			}
			guestBudget, err := time.ParseDuration(args[3])
			if err != nil || guestBudget != max(wait, 10*time.Minute) {
				t.Fatalf("guest timeout=%q, want %s", args[3], max(wait, 10*time.Minute))
			}
		}
	}
}

type timedOutGPUProvisionRuntime struct{ runtime.HostRuntime }

func (*timedOutGPUProvisionRuntime) ExecTimeout(string, time.Duration, ...string) (string, error) {
	return "", context.DeadlineExceeded
}

func TestGPUProvisioningPreservesTimeoutCause(t *testing.T) {
	m := &Manager{rt: &timedOutGPUProvisionRuntime{}}
	err := m.provisionGPUScript("gpu", time.Minute, "true")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "--wait") || !strings.Contains(err.Error(), "gpu") {
		t.Fatalf("unhelpful timeout: %v", err)
	}
}
