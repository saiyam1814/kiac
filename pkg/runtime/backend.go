package runtime

import (
	"io"
	"time"
)

// NodeBackend is the lifecycle contract shared by every VM implementation.
// The apple/container client and the krunkit GPU backend intentionally meet
// the same contract so cluster code can route by node name instead of growing
// backend-specific branches throughout every lifecycle operation.
type NodeBackend interface {
	RunDetached(RunOpts) error
	Exec(name string, command ...string) (string, error)
	ExecTimeout(name string, timeout time.Duration, command ...string) (string, error)
	ExecStdin(name string, r io.Reader, command ...string) error
	WaitReady(name string, timeout time.Duration) error
	Logs(name string, timeout time.Duration) (string, error)
	IP(name string) (string, error)
	IPv6(name string) (string, error)
	List(prefix string) ([]Info, error)
	Stop(name string) error
	Start(name string) error
	Remove(names ...string) error
}

// HostRuntime adds operations owned by the primary apple/container runtime.
// A routed runtime still delegates these host-wide and image operations to its
// primary backend; alternate node backends only implement NodeBackend.
type HostRuntime interface {
	NodeBackend
	Available() bool
	Version() (string, error)
	SystemRunning() bool
	SystemStatus(timeout time.Duration) (string, error)
	SystemStart(installDefaultKernel bool) error
	NetworkHasIPv6(network string) (bool, error)
	ImagePull(image string) error
	ImageSave(image, path string) error
}

var _ HostRuntime = (*Client)(nil)
