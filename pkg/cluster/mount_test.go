package cluster

import (
	"slices"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestKubeadmNodeRunOptsCarryMounts(t *testing.T) {
	cfg := Config{
		Image:    "docker.io/kindest/node:v1.36.0",
		CPUs:     "4",
		Memory:   "2G",
		CPMemory: "4G",
		Kernel:   "/tmp/kernel",
		Mounts:   runtime.Mounts{{Source: "/host", Target: "/workspace", ReadOnly: true}},
		Publish:  runtime.Publishes{"127.0.0.1:8080:80"},
	}
	dns := []string{"1.1.1.1"}

	controlPlane := kubeadmNodeRunOpts(cfg, "kiac-dev-control-plane", cfg.CPMemory, dns)
	worker := kubeadmNodeRunOpts(cfg, "kiac-dev-worker-1", cfg.Memory, dns)
	for name, opts := range map[string]runtime.RunOpts{"control plane": controlPlane, "worker": worker} {
		if !slices.Equal(opts.Mounts, cfg.Mounts) {
			t.Errorf("%s Mounts = %+v, want %+v", name, opts.Mounts, cfg.Mounts)
		}
		if !slices.Equal(opts.DNS, dns) {
			t.Errorf("%s DNS = %q, want %q", name, opts.DNS, dns)
		}
	}
	if !slices.Equal(controlPlane.Publish, []string{"127.0.0.1:8080:80"}) {
		t.Errorf("control plane Publish = %q, want %q", controlPlane.Publish, []string{"127.0.0.1:8080:80"})
	}
	if len(worker.Publish) != 0 {
		t.Errorf("worker Publish = %q, want empty", worker.Publish)
	}
	if controlPlane.Memory != "4G" || worker.Memory != "2G" {
		t.Errorf("memory = control plane %q, worker %q", controlPlane.Memory, worker.Memory)
	}
}
