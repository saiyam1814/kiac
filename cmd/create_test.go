package cmd

import (
	"testing"

	"github.com/saiyam1814/kiac/pkg/cluster"
)

func TestDefaultK8sVersionUsesDistroReleaseStream(t *testing.T) {
	if got := defaultK8sVersion("kubeadm"); got != cluster.DefaultK8sVersion {
		t.Fatalf("kubeadm default = %q, want %q", got, cluster.DefaultK8sVersion)
	}
	if got := defaultK8sVersion("k3s"); got != cluster.DefaultK3sVersion {
		t.Fatalf("k3s default = %q, want %q", got, cluster.DefaultK3sVersion)
	}
}
