package runtime

import (
	"io"
	"strings"
	"testing"
	"time"
)

type routedTestBackend struct {
	NodeBackend
	name    string
	infos   []Info
	calls   []string
	removed []string
}

func (b *routedTestBackend) RunDetached(opts RunOpts) error {
	b.calls = append(b.calls, "run:"+opts.Name)
	return nil
}

func (b *routedTestBackend) Exec(name string, command ...string) (string, error) {
	b.calls = append(b.calls, "exec:"+name+":"+strings.Join(command, " "))
	return b.name, nil
}

func (b *routedTestBackend) ExecTimeout(name string, _ time.Duration, command ...string) (string, error) {
	return b.Exec(name, command...)
}

func (b *routedTestBackend) ExecStdin(name string, input io.Reader, command ...string) error {
	_, _ = io.ReadAll(input)
	_, err := b.Exec(name, command...)
	return err
}

func (b *routedTestBackend) WaitReady(string, time.Duration) error { return nil }

func (b *routedTestBackend) Logs(name string, _ time.Duration) (string, error) {
	return b.name + ":" + name, nil
}

func (b *routedTestBackend) IP(string) (string, error)   { return "192.0.2.1", nil }
func (b *routedTestBackend) IPv6(string) (string, error) { return "", nil }

func (b *routedTestBackend) List(prefix string) ([]Info, error) {
	var result []Info
	for _, info := range b.infos {
		if strings.HasPrefix(info.Name, prefix) {
			result = append(result, info)
		}
	}
	return result, nil
}

func (b *routedTestBackend) Stop(name string) error {
	b.calls = append(b.calls, "stop:"+name)
	return nil
}

func (b *routedTestBackend) Start(name string) error {
	b.calls = append(b.calls, "start:"+name)
	return nil
}

func (b *routedTestBackend) Remove(names ...string) error {
	b.removed = append(b.removed, names...)
	return nil
}

type routedTestHost struct{ *routedTestBackend }

func (h *routedTestHost) Available() bool                            { return true }
func (h *routedTestHost) Version() (string, error)                   { return "test", nil }
func (h *routedTestHost) SystemRunning() bool                        { return true }
func (h *routedTestHost) SystemStatus(time.Duration) (string, error) { return "running", nil }
func (h *routedTestHost) SystemStart(bool) error                     { return nil }
func (h *routedTestHost) NetworkHasIPv6(string) (bool, error)        { return true, nil }
func (h *routedTestHost) ImagePull(string) error                     { return nil }
func (h *routedTestHost) ImageSave(string, string) error             { return nil }

func TestRoutedRuntimeDispatchesByNodeName(t *testing.T) {
	primary := &routedTestHost{&routedTestBackend{name: BackendContainer}}
	gpu := &routedTestBackend{name: BackendKrunkit}
	owned := map[string]bool{"kiac-dev-control-plane": true, "kiac-dev-worker-1": true}
	rt := NewRoutedRuntime(primary, BackendRoute{
		Name:    BackendKrunkit,
		Match:   func(name string) bool { return owned[name] },
		Backend: gpu,
	})

	if err := rt.RunDetached(RunOpts{Name: "kiac-cpu-worker-1"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunDetached(RunOpts{Backend: BackendKrunkit, Name: "kiac-dev-control-plane"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := rt.Exec("kiac-dev-worker-1", "true"); got != BackendKrunkit {
		t.Fatalf("GPU exec used %q backend", got)
	}
	if len(primary.calls) != 1 || primary.calls[0] != "run:kiac-cpu-worker-1" {
		t.Fatalf("primary calls = %v", primary.calls)
	}
	if len(gpu.calls) != 2 || gpu.calls[0] != "run:kiac-dev-control-plane" {
		t.Fatalf("GPU calls = %v", gpu.calls)
	}
}

func TestRoutedRuntimeRejectsUnknownExplicitBackend(t *testing.T) {
	primary := &routedTestHost{&routedTestBackend{name: BackendContainer}}
	rt := NewRoutedRuntime(primary)
	if err := rt.RunDetached(RunOpts{Backend: "missing", Name: "kiac-dev-control-plane"}); err == nil || !strings.Contains(err.Error(), "unknown runtime backend") {
		t.Fatalf("RunDetached error = %v", err)
	}
}

func TestRoutedRuntimeMergesInventoryAndRemoval(t *testing.T) {
	primary := &routedTestHost{&routedTestBackend{name: BackendContainer, infos: []Info{{Name: "kiac-dev-control-plane"}}}}
	gpu := &routedTestBackend{name: BackendKrunkit, infos: []Info{{Name: "kiac-dev-gpu-1"}}}
	rt := NewRoutedRuntime(primary, BackendRoute{
		Name:    BackendKrunkit,
		Match:   func(name string) bool { return strings.Contains(name, "-gpu-") },
		Backend: gpu,
	})

	infos, err := rt.List("kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Backend != BackendContainer || infos[1].Backend != BackendKrunkit {
		t.Fatalf("merged inventory = %+v", infos)
	}
	if err := rt.Remove("kiac-dev-worker-1", "kiac-dev-gpu-1", "kiac-dev-gpu-2"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(primary.removed, ","); got != "kiac-dev-worker-1" {
		t.Fatalf("primary removed %q", got)
	}
	if got := strings.Join(gpu.removed, ","); got != "kiac-dev-gpu-1,kiac-dev-gpu-2" {
		t.Fatalf("GPU removed %q", got)
	}
}

func TestRoutedRuntimeRejectsDuplicateInventory(t *testing.T) {
	primary := &routedTestHost{&routedTestBackend{infos: []Info{{Name: "kiac-dev-gpu-1"}}}}
	gpu := &routedTestBackend{infos: []Info{{Name: "kiac-dev-gpu-1"}}}
	rt := NewRoutedRuntime(primary, BackendRoute{Name: BackendKrunkit, Match: func(string) bool { return true }, Backend: gpu})

	if _, err := rt.List("kiac-dev-"); err == nil || !strings.Contains(err.Error(), "more than one runtime backend") {
		t.Fatalf("List error = %v", err)
	}
}
