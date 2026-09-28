package cluster

import (
	"fmt"
	"time"
)

// Bound the guest process group as well as the SSH client. Killing only the
// client can leave a package manager running in a VM retained after failure.
func (m *Manager) provisionGPUScript(node string, wait time.Duration, script string) error {
	// --wait historically bounded readiness, not first-boot package downloads.
	// Keep a generous floor so the default five-minute readiness setting does
	// not regress successful creates on slow Fedora mirrors.
	budget := max(wait, 10*time.Minute)
	_, err := m.rt.ExecTimeout(node, budget+15*time.Second,
		"timeout", "--signal=TERM", "--kill-after=10s", fmt.Sprintf("%ds", int64(budget/time.Second)),
		"sh", "-euc", script)
	if err != nil {
		return fmt.Errorf("provisioning GPU-backend node %s (budget %s, configured with --wait): %w", node, budget, err)
	}
	return nil
}
