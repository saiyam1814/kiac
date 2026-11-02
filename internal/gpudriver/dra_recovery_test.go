package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	cdspec "tags.cncf.io/container-device-interface/specs-go"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	metadatav1beta1 "k8s.io/dynamic-resource-allocation/api/metadata/v1beta1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	draapi "k8s.io/kubelet/pkg/apis/dra/v1"
)

func TestDRAClaimMetadataSurvivesDriverRestartAndUnprepare(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "kiac-dra-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	data, cdi := filepath.Join(root, "data"), filepath.Join(root, "cdi")
	os.MkdirAll(data, 0750)
	os.MkdirAll(cdi, 0755)
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "work", UID: "claim-one"}, Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{{Driver: driverName, Pool: "node", Device: deviceName, Request: "gpu"}}}}}}
	client := fake.NewClientset(claim)
	if err := writeCDISpec(cdi, "/dev/null", ""); err != nil {
		t.Fatal(err)
	}
	start := func(pod string) (*kubeletplugin.Helper, draapi.DRAPluginClient, func()) {
		t.Helper()
		ctx, cancel := context.WithCancelCause(t.Context())
		drv := &driver{nodeName: "node", memoryMiB: 32768, health: testHealthyMonitor(), renderDevice: "/dev/null", cancel: cancel}
		helper, err := kubeletplugin.Start(ctx, drv, kubeletplugin.DriverName(driverName), kubeletplugin.NodeName("node"), kubeletplugin.KubeClient(client), kubeletplugin.RegistrationService(false), kubeletplugin.PluginDataDirectoryPath(data), kubeletplugin.RollingUpdate(types.UID(pod)), kubeletplugin.CDIDirectory(cdi), kubeletplugin.EnableDeviceMetadata(true, []schema.GroupVersion{metadatav1beta1.SchemeGroupVersion}))
		if err != nil {
			cancel(nil)
			t.Fatal(err)
		}
		dialCtx, stopDial := context.WithTimeout(t.Context(), time.Second)
		defer stopDial()
		conn, err := dialUnix(dialCtx, filepath.Join(data, "dra-"+pod+".sock"))
		if err != nil {
			helper.Stop()
			cancel(nil)
			t.Fatal(err)
		}
		var once sync.Once
		stop := func() { once.Do(func() { conn.Close(); helper.Stop(); cancel(nil) }) }
		t.Cleanup(stop)
		return helper, draapi.NewDRAPluginClient(conn), stop
	}
	request := &draapi.Claim{Namespace: claim.Namespace, Name: claim.Name, Uid: string(claim.UID)}
	_, first, stopFirst := start("pod-one")
	prepare := func(api draapi.DRAPluginClient) []string {
		t.Helper()
		result, err := api.NodePrepareResources(t.Context(), &draapi.NodePrepareResourcesRequest{Claims: []*draapi.Claim{request}})
		if err != nil {
			t.Fatal(err)
		}
		prepared := result.Claims[string(claim.UID)]
		if prepared == nil || prepared.Error != "" || len(prepared.Devices) != 1 {
			t.Fatalf("prepare: %+v", prepared)
		}
		return prepared.Devices[0].CdiDeviceIds
	}
	ids := prepare(first)
	if len(ids) != 2 || ids[0] != cdiDeviceID {
		t.Fatalf("CDI IDs=%v", ids)
	}
	metadata := filepath.Join(data, "dra-device-metadata", "work_claim", "gpu", "metadata.json")
	raw, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(cdi, "gpu.kiac.dev_metadata_claim-one_*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("metadata CDI files=%v err=%v", files, err)
	}
	specRaw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var spec cdspec.Spec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Devices) != 1 || len(spec.Devices[0].ContainerEdits.Mounts) != 1 || !strings.Contains(strings.Join(spec.Devices[0].ContainerEdits.Mounts[0].Options, ","), "ro") {
		t.Fatalf("metadata CDI must mount read-only: %+v", spec)
	}
	if st, err := os.Stat(metadata); err != nil || st.Mode().Perm() != 0644 {
		t.Fatalf("metadata permissions: %v %v", st, err)
	}
	stopFirst()
	// A new stateless driver can reproduce Prepare and remove the prior
	// instance's helper-owned metadata without a custom checkpoint format.
	_, second, stopSecond := start("pod-two")
	defer stopSecond()
	repeated := prepare(second)
	if len(repeated) != len(ids) || repeated[1] != ids[1] {
		t.Fatalf("restart changed metadata CDI identity: %v -> %v", ids, repeated)
	}
	result, err := second.NodeUnprepareResources(t.Context(), &draapi.NodeUnprepareResourcesRequest{Claims: []*draapi.Claim{request}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Claims[string(claim.UID)].Error != "" {
		t.Fatalf("unprepare=%+v", result)
	}
	if _, err := os.Stat(metadata); !os.IsNotExist(err) {
		t.Fatalf("claim metadata survived unprepare: %v", err)
	}
	files, _ = filepath.Glob(filepath.Join(cdi, "gpu.kiac.dev_metadata_claim-one_*.json"))
	if len(files) != 0 {
		t.Fatalf("metadata CDI not cleaned: %v", files)
	}
	if _, err := os.Stat(filepath.Join(cdi, "kiac-gpu.json")); err != nil {
		t.Fatalf("base GPU CDI was removed: %v", err)
	}
	// Duplicate unprepare after restart is idempotent.
	if _, err := second.NodeUnprepareResources(t.Context(), &draapi.NodeUnprepareResourcesRequest{Claims: []*draapi.Claim{request}}); err != nil {
		t.Fatal(err)
	}
}
