package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testGPUInventory() []resourcev1.ResourceSlice {
	node := "kiac-test-gpu-1"
	return []resourcev1.ResourceSlice{{Spec: resourcev1.ResourceSliceSpec{Driver: kiacGPUDriver, NodeName: &node, Pool: resourcev1.ResourcePool{Name: node, Generation: 1, ResourceSliceCount: 1}, Devices: []resourcev1.Device{{Name: "venus-0", Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{"memory": {Value: resource.MustParse("58Gi")}}}}}}}
}

func testGPUClaim(name, memory string) resourcev1.ResourceClaim {
	claim := resourcev1.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"}, Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{Name: "gpu", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: kiacGPUDriver}}}}}}
	if memory != "" {
		id := types.UID("share-" + name)
		claim.Status.Allocation = &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{{Request: "gpu", Driver: kiacGPUDriver, Pool: "kiac-test-gpu-1", Device: "venus-0", ShareID: &id, ConsumedCapacity: map[resourcev1.QualifiedName]resource.Quantity{"memory": resource.MustParse(memory)}}}}}
	}
	return claim
}

func inspectTestData(slices []resourcev1.ResourceSlice, claims []resourcev1.ResourceClaim, pods []corev1.Pod) GPUInspection {
	return buildGPUInspection(GPUStatus{Driver: "dra"}, slices, claims, pods)
}

func TestGPUInspectionFullWindowAndPendingWorkload(t *testing.T) {
	claims := []resourcev1.ResourceClaim{testGPUClaim("a", "8Gi"), testGPUClaim("b", "50Gi"), testGPUClaim("overflow", "")}
	overflow := "overflow"
	pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: "test"}, Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: &overflow}}}, Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "cannot allocate all claims"}}}}}
	report := inspectTestData(testGPUInventory(), claims, pods)
	if len(report.Devices) != 1 || report.Devices[0].Reserved == nil || *report.Devices[0].Reserved != "58Gi" || *report.Devices[0].Unreserved != "0" {
		t.Fatalf("accounting: %+v", report.Devices)
	}
	if len(report.Claims) != 3 || report.Claims[2].State != "pending" || len(report.Claims[2].Consumers) != 1 {
		t.Fatalf("claims: %+v", report.Claims)
	}
	if len(report.Workloads) != 1 || report.Workloads[0].Reason != "Unschedulable" || report.Workloads[0].Message != "cannot allocate all claims" {
		t.Fatalf("workloads: %+v", report.Workloads)
	}
	// Releasing a claim changes only scheduler accounting, never physical usage.
	report = inspectTestData(testGPUInventory(), []resourcev1.ResourceClaim{claims[1], testGPUClaim("overflow", "1Gi")}, pods)
	if *report.Devices[0].Reserved != "51Gi" || *report.Devices[0].Unreserved != "7Gi" {
		t.Fatalf("released accounting: %+v", report.Devices)
	}
}

func TestGPUInspectionSharedClaimCountedOnce(t *testing.T) {
	claim := testGPUClaim("shared", "8Gi")
	claim.Status.Allocation.Devices.Results = append(claim.Status.Allocation.Devices.Results, claim.Status.Allocation.Devices.Results[0])
	claim.Status.Allocation.Devices.Results[1].Request = "alias"
	claim.Status.ReservedFor = []resourcev1.ResourceClaimConsumerReference{{Resource: "pods", Name: "one"}}
	name := claim.Name
	var pods []corev1.Pod
	for _, podName := range []string{"one", "two"} {
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "test"}, Spec: corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: &name}}}})
	}
	report := inspectTestData(testGPUInventory(), []resourcev1.ResourceClaim{claim}, pods)
	if *report.Devices[0].Reserved != "8Gi" {
		t.Fatalf("double counted share: %+v", report.Devices)
	}
	if got := strings.Join(report.Claims[0].Consumers, ","); got != "one,two" {
		t.Fatalf("consumers=%s", got)
	}
}

func TestGPUInspectionDoesNotInventCapacity(t *testing.T) {
	for _, scenario := range []string{"missing accounting", "overcommitted", "incomplete inventory", "duplicate device", "stale generation"} {
		t.Run(scenario, func(t *testing.T) {
			slices := testGPUInventory()
			claim := testGPUClaim("a", "8Gi")
			switch scenario {
			case "missing accounting":
				claim.Status.Allocation.Devices.Results[0].ConsumedCapacity = nil
			case "overcommitted":
				claim = testGPUClaim("a", "59Gi")
			case "incomplete inventory":
				slices[0].Spec.Pool.ResourceSliceCount = 2
			case "duplicate device":
				slices[0].Spec.Devices = append(slices[0].Spec.Devices, slices[0].Spec.Devices[0])
			case "stale generation":
				newer := *slices[0].DeepCopy()
				newer.Spec.Pool.Generation = 2
				newer.Spec.Pool.ResourceSliceCount = 2
				slices = append(slices, newer)
			}
			report := inspectTestData(slices, []resourcev1.ResourceClaim{claim}, nil)
			for _, device := range report.Devices {
				if device.Reserved != nil || device.Unreserved != nil {
					t.Fatalf("invented accounting: %+v", device)
				}
			}
			if len(report.Notes) < 4 {
				t.Fatal("missing incomplete-accounting note")
			}
		})
	}
}

func TestGPUInspectionLatestGenerationAdminAndDeletingClaims(t *testing.T) {
	slices := testGPUInventory()
	newer := *slices[0].DeepCopy()
	newer.Spec.Pool.Generation = 2
	newer.Spec.Devices[0].Capacity["memory"] = resourcev1.DeviceCapacity{Value: resource.MustParse("60Gi")}
	slices = append(slices, newer)
	admin := testGPUClaim("admin", "58Gi")
	yes := true
	admin.Status.Allocation.Devices.Results[0].AdminAccess = &yes
	deleting := testGPUClaim("deleting", "8Gi")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	report := inspectTestData(slices, []resourcev1.ResourceClaim{admin, deleting}, nil)
	if len(report.Devices) != 1 || report.Devices[0].Capacity != "60Gi" || *report.Devices[0].Reserved != "8Gi" || *report.Devices[0].Unreserved != "52Gi" {
		t.Fatalf("accounting: %+v", report.Devices)
	}
	if !report.Claims[1].Deleting {
		t.Fatal("lost deletion state")
	}
}

func TestGPUInspectionPodClaimSourcesAndPluginRequests(t *testing.T) {
	var pods corev1.PodList
	err := json.Unmarshal([]byte(`{"items":[
 {"metadata":{"namespace":"test","name":"extended"},"spec":{"containers":[{"name":"gpu","env":[{"name":"SECRET","value":"must-never-appear"}],"resources":{"limits":{"kiac.dev/gpu":"1"}}}]},"status":{"extendedResourceClaimStatus":{"resourceClaimName":"auto"}}},
 {"metadata":{"namespace":"test","name":"template"},"status":{"resourceClaimStatuses":[{"name":"gpu","resourceClaimName":"generated"}]}},
 {"metadata":{"namespace":"test","name":"init"},"spec":{"initContainers":[{"name":"gpu","resources":{"requests":{"kiac.dev/gpu":"1"}}}]}},
 {"metadata":{"namespace":"test","name":"cpu"},"spec":{"containers":[{"name":"cpu","resources":{"requests":{"cpu":"1"}}}]}}
 ]}`), &pods)
	if err != nil {
		t.Fatal(err)
	}
	report := inspectTestData(testGPUInventory(), []resourcev1.ResourceClaim{testGPUClaim("auto", "8Gi"), testGPUClaim("generated", "8Gi")}, pods.Items)
	if len(report.Workloads) != 3 || report.Claims[0].Consumers[0] != "extended" || report.Claims[1].Consumers[0] != "template" {
		t.Fatalf("joins: %+v %+v", report.Claims, report.Workloads)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "must-never-appear") {
		t.Fatal("inspection copied pod environment")
	}
	plugin := buildGPUInspection(GPUStatus{Driver: "device-plugin"}, nil, nil, pods.Items)
	if len(plugin.Workloads) != 2 || len(plugin.Devices) != 0 || len(plugin.Claims) != 0 {
		t.Fatalf("plugin inspection: %+v", plugin)
	}
}
