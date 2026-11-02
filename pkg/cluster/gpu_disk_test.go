package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

func TestGPUCreateChecksHostDiskBeforeInventoryOrDownloads(t *testing.T) {
	for _, distro := range []string{"k3s", "kubeadm"} {
		t.Run(distro, func(t *testing.T) {
			// A state-root file is an invalid target filesystem. No runtime interface
			// is installed: reaching inventory, image preparation or boot would fail.
			root := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(root, nil, 0600); err != nil {
				t.Fatal(err)
			}
			m := &Manager{krunkit: &runtime.KrunkitClient{RootDir: root}}
			cfg := Config{Name: "disk-check", GPUWorkers: 1, K8sVersion: "1.37", IPFamily: IPv4}
			var err error
			if distro == "k3s" {
				err = m.createK3sGPU(cfg)
			} else {
				err = m.createKubeadmGPU(cfg)
			}
			if err == nil || !strings.Contains(err.Error(), "check host disk headroom") {
				t.Fatalf("did not reject before inventory/downloads: %v", err)
			}
		})
	}
}
func TestExternalWorkerChecksHostDiskBeforeInventoryOrDownloads(t *testing.T) {
	cfg, _, _ := workerFixture(t)
	m, r := workerManagerFixture(t)
	m.root = filepath.Join(t.TempDir(), "not-created")
	rejected := errors.New("insufficient host disk headroom")
	calls := 0
	m.headroom = func(nodes int) error {
		calls++
		if nodes != 1 {
			t.Fatalf("headroom nodes=%d", nodes)
		}
		return rejected
	}
	m.preflight = func() error { t.Fatal("disk rejection reached host preflight"); return nil }
	m.image = func() (string, error) { t.Fatal("disk rejection prepared image"); return "", nil }
	if _, err := m.Join(cfg); !errors.Is(err, rejected) {
		t.Fatalf("headroom rejection=%v", err)
	}
	if calls != 1 || len(r.events) != 0 {
		t.Fatalf("calls=%d runtime effects=%v", calls, r.events)
	}
	if _, err := os.Lstat(m.root); !os.IsNotExist(err) {
		t.Fatalf("disk rejection initialized worker inventory: %v", err)
	}
}
