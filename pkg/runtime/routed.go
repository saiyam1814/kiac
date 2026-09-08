package runtime

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	BackendContainer = "container"
	BackendKrunkit   = "krunkit"
)

// BackendRoute sends matching node names to an alternate VM backend.
type BackendRoute struct {
	Name    string
	Match   func(string) bool
	Backend NodeBackend
}

// RoutedRuntime presents multiple VM implementations as one HostRuntime.
// Host-wide operations remain owned by Primary; node-scoped operations are
// dispatched by name and List merges every backend's persistent inventory.
type RoutedRuntime struct {
	Primary HostRuntime
	Routes  []BackendRoute
}

var _ HostRuntime = (*RoutedRuntime)(nil)

func NewRoutedRuntime(primary HostRuntime, routes ...BackendRoute) *RoutedRuntime {
	return &RoutedRuntime{Primary: primary, Routes: routes}
}

func (r *RoutedRuntime) backendFor(name string) NodeBackend {
	for _, route := range r.Routes {
		if route.Backend != nil && route.Match != nil && route.Match(name) {
			return route.Backend
		}
	}
	return r.Primary
}

func (r *RoutedRuntime) RunDetached(opts RunOpts) error {
	if opts.Backend != "" {
		if opts.Backend == BackendContainer {
			return r.Primary.RunDetached(opts)
		}
		for _, route := range r.Routes {
			if route.Name == opts.Backend && route.Backend != nil {
				return route.Backend.RunDetached(opts)
			}
		}
		return fmt.Errorf("unknown runtime backend %q", opts.Backend)
	}
	return r.backendFor(opts.Name).RunDetached(opts)
}

func (r *RoutedRuntime) Exec(name string, command ...string) (string, error) {
	return r.backendFor(name).Exec(name, command...)
}

func (r *RoutedRuntime) ExecTimeout(name string, timeout time.Duration, command ...string) (string, error) {
	return r.backendFor(name).ExecTimeout(name, timeout, command...)
}

func (r *RoutedRuntime) ExecStdin(name string, input io.Reader, command ...string) error {
	return r.backendFor(name).ExecStdin(name, input, command...)
}

func (r *RoutedRuntime) WaitReady(name string, timeout time.Duration) error {
	return r.backendFor(name).WaitReady(name, timeout)
}

func (r *RoutedRuntime) Logs(name string, timeout time.Duration) (string, error) {
	return r.backendFor(name).Logs(name, timeout)
}

func (r *RoutedRuntime) IP(name string) (string, error) {
	return r.backendFor(name).IP(name)
}

func (r *RoutedRuntime) IPv6(name string) (string, error) {
	return r.backendFor(name).IPv6(name)
}

func (r *RoutedRuntime) List(prefix string) ([]Info, error) {
	infos, primaryErr := r.Primary.List(prefix)
	for i := range infos {
		if infos[i].Backend == "" {
			infos[i].Backend = BackendContainer
		}
	}
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		seen[info.Name] = struct{}{}
	}
	alternateRows := 0
	for _, route := range r.Routes {
		if route.Backend == nil {
			continue
		}
		rows, err := route.Backend.List(prefix)
		if err != nil {
			return nil, fmt.Errorf("listing %s nodes: %w", route.Name, err)
		}
		for _, info := range rows {
			if _, duplicate := seen[info.Name]; duplicate {
				return nil, fmt.Errorf("node %q appears in more than one runtime backend", info.Name)
			}
			if info.Backend == "" {
				info.Backend = route.Name
			}
			infos = append(infos, info)
			alternateRows++
			seen[info.Name] = struct{}{}
		}
	}
	// A GPU-only cluster remains manageable even when apple/container is not
	// installed or its service is unavailable. Preserve the primary error when
	// no alternate inventory can answer, so ordinary clusters are not hidden.
	if primaryErr != nil && alternateRows == 0 {
		return nil, primaryErr
	}
	return infos, nil
}

func (r *RoutedRuntime) Stop(name string) error { return r.backendFor(name).Stop(name) }

func (r *RoutedRuntime) Start(name string) error { return r.backendFor(name).Start(name) }

func (r *RoutedRuntime) Remove(names ...string) error {
	groups := make(map[NodeBackend][]string)
	for _, name := range names {
		backend := r.backendFor(name)
		groups[backend] = append(groups[backend], name)
	}
	var removeErrs []error
	for backend, group := range groups {
		if err := backend.Remove(group...); err != nil {
			removeErrs = append(removeErrs, fmt.Errorf("removing nodes %s: %w", strings.Join(group, ", "), err))
		}
	}
	return errors.Join(removeErrs...)
}

func (r *RoutedRuntime) Available() bool { return r.Primary.Available() }

func (r *RoutedRuntime) Version() (string, error) { return r.Primary.Version() }

func (r *RoutedRuntime) SystemRunning() bool { return r.Primary.SystemRunning() }

func (r *RoutedRuntime) SystemStatus(timeout time.Duration) (string, error) {
	return r.Primary.SystemStatus(timeout)
}

func (r *RoutedRuntime) SystemStart(installDefaultKernel bool) error {
	return r.Primary.SystemStart(installDefaultKernel)
}

func (r *RoutedRuntime) NetworkHasIPv6(network string) (bool, error) {
	return r.Primary.NetworkHasIPv6(network)
}

func (r *RoutedRuntime) ImagePull(image string) error { return r.Primary.ImagePull(image) }

func (r *RoutedRuntime) ImageSave(image, path string) error {
	return r.Primary.ImageSave(image, path)
}
