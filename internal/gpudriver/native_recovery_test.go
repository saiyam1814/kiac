package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	draapi "k8s.io/kubelet/pkg/apis/dra/v1"
)

// Exercise the real kubelet DRA gRPC adapter, not only direct method calls.
func TestNativeDRAKubeletPrepareRestartAndRevocation(t *testing.T) {
	claim := nativeTestClaim("claim-one")
	initial, broker, _ := newNativeTestDriver(t, claim)
	root, err := os.MkdirTemp("/tmp", "kiac-native-dra-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	pluginDir := filepath.Join(root, "plugin")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	start := func(driver *nativeDriver, pod string) (draapi.DRAPluginClient, func()) {
		t.Helper()
		ctx, cancel := context.WithCancelCause(t.Context())
		driver.cancel = cancel
		helper, err := kubeletplugin.Start(ctx, driver,
			kubeletplugin.DriverName(nativeDriverName), kubeletplugin.NodeName(driver.nodeName),
			kubeletplugin.KubeClient(driver.client), kubeletplugin.RegistrationService(false),
			kubeletplugin.PluginDataDirectoryPath(pluginDir), kubeletplugin.RollingUpdate(types.UID(pod)),
			kubeletplugin.CDIDirectory(driver.cdiRoot))
		if err != nil {
			cancel(nil)
			t.Fatal(err)
		}
		dialCtx, stopDial := context.WithTimeout(t.Context(), time.Second)
		defer stopDial()
		conn, err := dialUnix(dialCtx, filepath.Join(pluginDir, "dra-"+pod+".sock"))
		if err != nil {
			helper.Stop()
			cancel(nil)
			t.Fatal(err)
		}
		var once sync.Once
		stop := func() { once.Do(func() { conn.Close(); helper.Stop(); cancel(nil) }) }
		t.Cleanup(stop)
		return draapi.NewDRAPluginClient(conn), stop
	}
	request := &draapi.Claim{Namespace: claim.Namespace, Name: claim.Name, Uid: string(claim.UID)}
	prepare := func(api draapi.DRAPluginClient) string {
		t.Helper()
		response, err := api.NodePrepareResources(t.Context(), &draapi.NodePrepareResourcesRequest{Claims: []*draapi.Claim{request}})
		if err != nil {
			t.Fatal(err)
		}
		result := response.Claims[string(claim.UID)]
		if result == nil || result.Error != "" || len(result.Devices) != 1 || len(result.Devices[0].CdiDeviceIds) != 1 {
			t.Fatalf("native kubelet prepare failed: %+v", result)
		}
		return result.Devices[0].CdiDeviceIds[0]
	}
	first, stopFirst := start(initial, "pod-one")
	cdiID := prepare(first)
	oldLeaseID := broker.lease.ID
	stopFirst()
	replacement := restartedNativeTestDriver(initial)
	if err := replacement.loadStates(); err != nil {
		t.Fatal(err)
	}
	second, stopSecond := start(replacement, "pod-two")
	defer stopSecond()
	if got := prepare(second); got != cdiID || broker.lease.ID != oldLeaseID {
		t.Fatal("native DRA restart changed the still-live lease or CDI identity")
	}
	response, err := second.NodeUnprepareResources(t.Context(), &draapi.NodeUnprepareResourcesRequest{Claims: []*draapi.Claim{request}})
	if err != nil || response.Claims[string(claim.UID)].Error != "" || broker.lease.ID != "" {
		t.Fatalf("native kubelet Unprepare did not revoke the host lease: response=%+v error=%v", response, err)
	}
	files, _ := filepath.Glob(filepath.Join(replacement.cdiRoot, "kiac-metal-*.json"))
	if len(files) != 0 || len(replacement.states) != 0 {
		t.Fatal("native claim credentials or CDI survived Unprepare")
	}
}
