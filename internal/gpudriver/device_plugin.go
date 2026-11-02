package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	deviceapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type devicePlugin struct {
	deviceapi.UnimplementedDevicePluginServer
	health                     *healthMonitor
	renderDevice, optionalCard string
}

func (*devicePlugin) GetDevicePluginOptions(context.Context, *deviceapi.Empty) (*deviceapi.DevicePluginOptions, error) {
	return &deviceapi.DevicePluginOptions{}, nil
}
func (p *devicePlugin) ListAndWatch(_ *deviceapi.Empty, stream deviceapi.DevicePlugin_ListAndWatchServer) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		health, changed := p.health.snapshot()
		value := deviceapi.Unhealthy
		if health.healthy {
			value = deviceapi.Healthy
		}
		if err := stream.Send(&deviceapi.ListAndWatchResponse{Devices: []*deviceapi.Device{{ID: deviceName, Health: value}}}); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-changed:
		case <-tick.C:
		}
	}
}
func (p *devicePlugin) Allocate(_ context.Context, req *deviceapi.AllocateRequest) (*deviceapi.AllocateResponse, error) {
	if err := p.health.requireHealthy(); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err := requireCharacterDevice(p.renderDevice); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	response := &deviceapi.AllocateResponse{}
	for _, container := range req.ContainerRequests {
		if len(container.DevicesIds) != 1 || container.DevicesIds[0] != deviceName {
			return nil, status.Error(codes.InvalidArgument, "only the published Venus device may be allocated")
		}
		devices := []*deviceapi.DeviceSpec{{HostPath: p.renderDevice, ContainerPath: p.renderDevice, Permissions: "rw"}}
		if p.optionalCard != "" && requireCharacterDevice(p.optionalCard) == nil {
			devices = append(devices, &deviceapi.DeviceSpec{HostPath: p.optionalCard, ContainerPath: p.optionalCard, Permissions: "rw"})
		}
		response.ContainerResponses = append(response.ContainerResponses, &deviceapi.ContainerAllocateResponse{Devices: devices, Envs: map[string]string{"KIAC_GPU_API": "venus", "KIAC_GPU_PRODUCT": "apple-silicon"}})
	}
	return response, nil
}
func dialUnix(ctx context.Context, path string) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, "passthrough:///"+path, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}))
}
func registerDevicePlugin(ctx context.Context, registry, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := dialUnix(ctx, registry)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = deviceapi.NewRegistrationClient(conn).Register(ctx, &deviceapi.RegisterRequest{Version: deviceapi.Version, Endpoint: endpoint, ResourceName: resourceName, Options: &deviceapi.DevicePluginOptions{}})
	return err
}
func runDevicePlugin(ctx context.Context, opts options, health *healthMonitor) error {
	return serveDevicePlugin(ctx, opts.pluginsDir, opts.podUID, &devicePlugin{health: health, renderDevice: opts.renderDevice, optionalCard: opts.optionalCard}, time.Second)
}

// Kubelet removes plugin sockets during restart. Recreate our server and
// register against the new kubelet socket; never rely on a stale inode or
// repeatedly re-register an already live endpoint (which disrupts watches).
func serveDevicePlugin(ctx context.Context, dir, podUID string, plugin *devicePlugin, retry time.Duration) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(podUID))
	endpoint := fmt.Sprintf("kiac-gpu-%x.sock", sum[:6])
	path := filepath.Join(dir, endpoint)
	registry := filepath.Join(dir, "kubelet.sock")
	var server *grpc.Server
	var endpointInfo, registeredRegistry os.FileInfo
	var serveErrors chan error
	stop := func() {
		if server != nil {
			server.Stop()
			server = nil
		}
		_ = os.Remove(path)
		endpointInfo = nil
		registeredRegistry = nil
	}
	defer stop()
	tick := time.NewTicker(retry)
	defer tick.Stop()
	for {
		if server == nil {
			_ = os.Remove(path)
			listener, err := net.Listen("unix", path)
			if err != nil {
				return fmt.Errorf("listen on device plugin socket: %w", err)
			}
			endpointInfo, err = os.Stat(path)
			if err != nil {
				listener.Close()
				return err
			}
			server = grpc.NewServer()
			deviceapi.RegisterDevicePluginServer(server, plugin)
			serveErrors = make(chan error, 1)
			current, result := server, serveErrors
			go func() { result <- current.Serve(listener) }()
		}
		currentEndpoint, err := os.Stat(path)
		if err != nil || !os.SameFile(endpointInfo, currentEndpoint) {
			stop()
			continue
		}
		currentRegistry, err := os.Stat(registry)
		if err != nil {
			registeredRegistry = nil
		} else if registeredRegistry == nil || !os.SameFile(registeredRegistry, currentRegistry) {
			if err := registerDevicePlugin(ctx, registry, endpoint); err == nil {
				registeredRegistry = currentRegistry
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErrors:
			if err != nil {
				return fmt.Errorf("device plugin server: %w", err)
			}
			stop()
		case <-tick.C:
		}
	}
}
