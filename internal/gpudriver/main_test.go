package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestDriverResourcesDescribeMeasuredVenusDevice(t *testing.T) {
	resources, err := driverResources("kiac-test-gpu-1", 32768)
	if err != nil {
		t.Fatal(err)
	}
	pool := resources.Pools["kiac-test-gpu-1"]
	if len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 1 {
		t.Fatalf("unexpected resources: %#v", resources)
	}
	device := pool.Slices[0].Devices[0]
	if device.Name != deviceName {
		t.Fatalf("device name = %q", device.Name)
	}
	if device.AllowMultipleAllocations == nil || !*device.AllowMultipleAllocations {
		t.Fatal("DRA device must allow capacity-accounted sharing")
	}
	memory := device.Capacity["memory"]
	if got := memory.Value.String(); got != "32Gi" {
		t.Fatalf("memory = %s, want 32Gi", got)
	}
	if memory.RequestPolicy == nil || memory.RequestPolicy.ValidRange == nil {
		t.Fatal("memory capacity has no request policy")
	}
	if got := memory.RequestPolicy.ValidRange.Step.String(); got != "1Gi" {
		t.Fatalf("memory step = %s, want 1Gi", got)
	}
}

func TestDriverResourcesRejectTinyWindow(t *testing.T) {
	if _, err := driverResources("node", 1023); err == nil {
		t.Fatal("expected a sub-GiB GPU window to be rejected")
	}
}

func TestPrepareUsesOnlyRealPublishedDevice(t *testing.T) {
	drv := &driver{nodeName: "kiac-test-gpu-1"}
	share := types.UID("share-one")
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", UID: types.UID("claim-one")},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Request: "gpu", Driver: driverName, Pool: "kiac-test-gpu-1", Device: deviceName, ShareID: &share,
					}},
				},
			},
		},
	}
	results, err := drv.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	prepared := results[claim.UID]
	if prepared.Err != nil || len(prepared.Devices) != 1 {
		t.Fatalf("prepare result: %#v", prepared)
	}
	if got := prepared.Devices[0].CDIDeviceIDs; len(got) != 1 || got[0] != cdiDeviceID {
		t.Fatalf("CDI IDs = %v", got)
	}
}

func TestPrepareRejectsForeignDevice(t *testing.T) {
	drv := &driver{nodeName: "kiac-test-gpu-1"}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-one")},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Request: "gpu", Driver: driverName, Pool: "other-node", Device: deviceName,
					}},
				},
			},
		},
	}
	results, err := drv.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	if results[claim.UID].Err == nil {
		t.Fatal("foreign pool was accepted")
	}
}

func TestWriteCDISpec(t *testing.T) {
	root := t.TempDir()
	render := filepath.Join(root, "renderD128")
	if err := os.WriteFile(render, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The writer itself is kept independent from device discovery so its JSON
	// contract can be tested on hosts without a Linux DRM character device.
	if err := writeCDISpec(root, render, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "kiac-gpu.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec["kind"] != resourceName || spec["cdiVersion"] != "0.6.0" {
		t.Fatalf("unexpected CDI spec: %s", raw)
	}
}

func TestNVIDIACompatibilityPatches(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "inference",
				Resources: corev1.ResourceRequirements{
					Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
					Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
				},
			}},
		},
	}
	patches, rewritten, err := nvidiaCompatibilityPatches(pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewritten) != 1 || rewritten[0] != "nvidia.com/gpu" {
		t.Fatalf("rewritten = %v", rewritten)
	}
	raw, err := json.Marshal(patches)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"kiac.dev~1gpu", "nvidia.com~1gpu", compatAnnotation, "/spec/tolerations"} {
		if !strings.Contains(text, want) {
			t.Errorf("patch is missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, `"path":"/spec/containers/0/resources/limits/nvidia.com/gpu"`) {
		t.Fatalf("resource JSON pointer path was not escaped: %s", text)
	}
	if !strings.Contains(text, `"kiac.dev/rewrote-gpu-resource":"nvidia.com/gpu"`) {
		t.Fatalf("compatibility annotation does not match the public contract: %s", text)
	}
}

func TestNVIDIACompatibilityIsOptInByAdmissionScope(t *testing.T) {
	pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ordinary"}}}}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	response := mutateAdmission(&admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		Object:    runtime.RawExtension{Raw: raw},
	})
	if !response.Allowed || response.Patch != nil {
		t.Fatalf("ordinary pod was mutated: %#v", response)
	}
}

func TestNVIDIACompatibilityRejectsConflictingResources(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "conflict",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("1"),
			"kiac.dev/gpu":   resource.MustParse("2"),
		}},
	}}}}
	if _, _, err := nvidiaCompatibilityPatches(pod); err == nil {
		t.Fatal("conflicting resource requests were accepted")
	}
}

func TestNVIDIACompatibilityRejectsMultipleAliases(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "ambiguous",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"nvidia.com/gpu":        resource.MustParse("1"),
			"nvidia.com/mig-1g.5gb": resource.MustParse("1"),
		}},
	}}}}
	if _, _, err := nvidiaCompatibilityPatches(pod); err == nil || !strings.Contains(err.Error(), "multiple NVIDIA") {
		t.Fatalf("multiple aliases error = %v", err)
	}
}

func TestNVIDIACompatibilityRejectsAliasesSplitAcrossRequestsAndLimits(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "ambiguous",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			Limits:   corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("1")},
		},
	}}}}
	if _, _, err := nvidiaCompatibilityPatches(pod); err == nil || !strings.Contains(err.Error(), "multiple NVIDIA") {
		t.Fatalf("split aliases error = %v", err)
	}
}
