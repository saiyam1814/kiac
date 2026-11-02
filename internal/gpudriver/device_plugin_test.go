package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	deviceapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type testRegistration struct {
	deviceapi.UnimplementedRegistrationServer
	registered chan string
}

func (r *testRegistration) Register(_ context.Context, req *deviceapi.RegisterRequest) (*deviceapi.Empty, error) {
	r.registered <- req.Endpoint
	return &deviceapi.Empty{}, nil
}
func startFakeKubelet(t *testing.T, path string, registered chan string) *grpc.Server {
	t.Helper()
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	deviceapi.RegisterRegistrationServer(server, &testRegistration{registered: registered})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return server
}
func waitRegistered(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case endpoint := <-ch:
		return endpoint
	case <-time.After(5 * time.Second):
		t.Fatal("plugin did not register")
		return ""
	}
}
func TestDevicePluginKubeletSocketRestartAndHealth(t *testing.T) {
	// Short paths work with the Unix-domain socket limit on macOS as well.
	dir, err := os.MkdirTemp("/tmp", "kiac-plugin-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	registry := filepath.Join(dir, "kubelet.sock")
	registered := make(chan string, 8)
	kubelet := startFakeKubelet(t, registry, registered)
	m := testHealthyMonitor()
	plugin := &devicePlugin{health: m, renderDevice: "/dev/null"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveDevicePlugin(ctx, dir, "pod", plugin, 20*time.Millisecond) }()
	endpoint := waitRegistered(t, registered)
	readHealth := func(want string) {
		t.Helper()
		dialCtx, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		conn, err := dialUnix(dialCtx, filepath.Join(dir, endpoint))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		stream, err := deviceapi.NewDevicePluginClient(conn).ListAndWatch(dialCtx, &deviceapi.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		report, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Devices) != 1 || report.Devices[0].Health != want {
			t.Fatalf("devices=%+v want %s", report.Devices, want)
		}
	}
	readHealth(deviceapi.Healthy)
	// A real kubelet restart deletes both its registration socket and plugin
	// endpoints. A new gRPC server must be reachable and registered afterward.
	kubelet.Stop()
	os.Remove(registry)
	os.Remove(filepath.Join(dir, endpoint))
	startFakeKubelet(t, registry, registered)
	if next := waitRegistered(t, registered); next != endpoint {
		t.Fatalf("endpoint changed: %s", next)
	}
	readHealth(deviceapi.Healthy)

	watchCtx, stopWatch := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopWatch()
	conn, err := dialUnix(watchCtx, filepath.Join(dir, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := deviceapi.NewDevicePluginClient(conn).ListAndWatch(watchCtx, &deviceapi.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	setHealth := func(healthy bool) {
		m.mu.Lock()
		m.state = healthState{healthy: healthy, checkedAt: time.Now(), message: "transfer result"}
		close(m.changed)
		m.changed = make(chan struct{})
		m.mu.Unlock()
	}
	setHealth(false)
	if report, err := stream.Recv(); err != nil || report.Devices[0].Health != deviceapi.Unhealthy {
		t.Fatalf("live failure update=%v %v", report, err)
	}
	setHealth(true)
	if report, err := stream.Recv(); err != nil || report.Devices[0].Health != deviceapi.Healthy {
		t.Fatalf("live recovery update=%v %v", report, err)
	}
	stopWatch()

	// Registry-only replacement must also trigger registration when the plugin
	// endpoint is retained (for example a kubelet crash/restart race).
	os.Remove(registry)
	startFakeKubelet(t, registry, registered)
	waitRegistered(t, registered)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plugin did not stop")
	}
}
func TestDevicePluginAllocateHealthAndIdentity(t *testing.T) {
	m := testHealthyMonitor()
	plugin := &devicePlugin{health: m, renderDevice: "/dev/null", optionalCard: "/does-not-exist"}
	req := &deviceapi.AllocateRequest{ContainerRequests: []*deviceapi.ContainerAllocateRequest{{DevicesIds: []string{deviceName}}}}
	result, err := plugin.Allocate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ContainerResponses) != 1 || len(result.ContainerResponses[0].Devices) != 1 || result.ContainerResponses[0].Devices[0].Permissions != "rw" {
		t.Fatalf("allocation=%+v", result)
	}
	req.ContainerRequests[0].DevicesIds = []string{"foreign"}
	if _, err := plugin.Allocate(t.Context(), req); err == nil {
		t.Fatal("foreign device allocated")
	}
	req.ContainerRequests[0].DevicesIds = []string{deviceName}
	m.mu.Lock()
	m.state.healthy = false
	m.mu.Unlock()
	if _, err := plugin.Allocate(t.Context(), req); err == nil {
		t.Fatal("unhealthy device allocated")
	}
	m.mu.Lock()
	m.state = healthState{healthy: true, checkedAt: time.Now()}
	m.mu.Unlock()
	if _, err := plugin.Allocate(t.Context(), req); err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	plugin.renderDevice = filepath.Join(t.TempDir(), "regular")
	os.WriteFile(plugin.renderDevice, nil, 0600)
	if _, err := plugin.Allocate(t.Context(), req); err == nil {
		t.Fatal("regular file allocated as GPU")
	}
}
