package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	cdspec "tags.cncf.io/container-device-interface/specs-go"
)

type fakeNativeBroker struct {
	lease       nativeLease
	identity    nativeLeaseRequest
	acquires    int
	revokes     int
	sequence    int
	unavailable bool
}

func (b *fakeNativeBroker) Health(context.Context) (nativeBrokerHealth, error) {
	if b.unavailable {
		return nativeBrokerHealth{}, errors.New("broker unavailable")
	}
	return nativeBrokerHealth{Ready: true, Backend: "native-metal", InferenceAvailable: true}, nil
}

func (b *fakeNativeBroker) Acquire(_ context.Context, request nativeLeaseRequest) (nativeLease, error) {
	b.acquires++
	if b.unavailable {
		return nativeLease{}, errors.New("broker unavailable")
	}
	if b.lease.ID != "" && (b.identity.ClaimUID != request.ClaimUID || b.identity.Namespace != request.Namespace || b.identity.NodeName != request.NodeName) {
		return nativeLease{}, errors.New("exclusive lease conflict")
	}
	if b.lease.ID == "" {
		b.sequence++
		b.lease = nativeLease{ID: strings.Repeat("x", b.sequence), Token: strings.Repeat("a", 32+b.sequence)}
	}
	b.identity = request
	b.lease.ExpiresAt = time.Now().Add(time.Duration(request.TTLSeconds) * time.Second)
	return b.lease, nil
}

func (b *fakeNativeBroker) Revoke(_ context.Context, id string) error {
	b.revokes++
	if b.unavailable {
		return errors.New("broker unavailable")
	}
	if id == b.lease.ID {
		b.lease = nativeLease{}
	}
	return nil
}

func nativeTestClaim(uid types.UID) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "work", Name: string(uid), UID: uid},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{
			Results: []resourceapi.DeviceRequestAllocationResult{{Request: "gpu", Driver: nativeDriverName, Pool: "node-one", Device: nativeDeviceName}},
		}}}}
}

func newNativeTestDriver(t *testing.T, claims ...*resourceapi.ResourceClaim) (*nativeDriver, *fakeNativeBroker, *fake.Clientset) {
	t.Helper()
	root := t.TempDir()
	objects := make([]runtime.Object, len(claims))
	for i := range claims {
		objects[i] = claims[i]
	}
	client := fake.NewClientset(objects...)
	broker := &fakeNativeBroker{}
	driver := &nativeDriver{nodeName: "node-one", endpoint: "https://192.0.2.1:9443", ca: []byte("test-ca-not-a-real-certificate"),
		cdiRoot: filepath.Join(root, "cdi"), stateRoot: filepath.Join(root, "private", "state"), ttl: 120 * time.Second,
		client: client, broker: broker, states: map[types.UID]*nativeClaimState{}}
	if err := driver.loadStates(); err != nil {
		t.Fatal(err)
	}
	return driver, broker, client
}

func prepareNativeTestClaim(t *testing.T, driver *nativeDriver, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	t.Helper()
	results, err := driver.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	result := results[claim.UID]
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	return result
}

func restartedNativeTestDriver(driver *nativeDriver) *nativeDriver {
	return &nativeDriver{nodeName: driver.nodeName, endpoint: driver.endpoint, ca: driver.ca,
		cdiRoot: driver.cdiRoot, stateRoot: driver.stateRoot, ttl: driver.ttl,
		client: driver.client, broker: driver.broker, states: map[types.UID]*nativeClaimState{}}
}

func TestNativeResourcesAreOneExclusiveHostService(t *testing.T) {
	resources := nativeResources("node-one", "driver-pod-one", false, true)
	pool := resources.Pools["node-one"]
	if len(resources.Pools) != 1 || len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 1 {
		t.Fatalf("expected one exclusive device on one node: %+v", resources)
	}
	device := pool.Slices[0].Devices[0]
	if device.Name != nativeDeviceName || len(device.Capacity) != 0 || (device.AllowMultipleAllocations != nil && *device.AllowMultipleAllocations) {
		t.Fatalf("native service must not advertise per-VM memory or shares: %+v", device)
	}
	if len(device.Taints) != 1 || device.Taints[0].Key != nativeHealthTaint || *device.Attributes["execution"].StringValue != "macos-host-service" {
		t.Fatalf("unready native service must be tainted and described accurately: %+v", device)
	}
	if len(nativeResources("node-one", "driver-pod-one", true, true).Pools["node-one"].Slices[0].Devices[0].Taints) != 0 {
		t.Fatal("ready native service remained tainted")
	}
}

func TestNativeResourceInventoryIdentifiesTheCurrentDriverAcrossRestartAndHealthChanges(t *testing.T) {
	previous := nativeResources("node-one", "previous-driver-pod", true, true).Pools["node-one"].Slices[0].Devices[0]
	for _, healthy := range []bool{false, true} {
		current := nativeResources("node-one", "replacement-driver-pod", healthy, healthy).Pools["node-one"].Slices[0].Devices[0]
		before, after := previous.Attributes["driverPodUID"].StringValue, current.Attributes["driverPodUID"].StringValue
		if before == nil || after == nil || *before != "previous-driver-pod" || *after != "replacement-driver-pod" || *before == *after {
			t.Fatal("installer cannot distinguish stale healthy inventory from the replacement driver")
		}
		if current.Name != previous.Name || (healthy && len(current.Taints) != 0) || (!healthy && len(current.Taints) != 1) {
			t.Fatal("driver restart changed physical-service identity or dropped its current health")
		}
	}
}

func TestNativePrepareIsIdempotentAndMountsNoHostAdminOrDRMDevice(t *testing.T) {
	claim := nativeTestClaim("claim-one")
	driver, broker, _ := newNativeTestDriver(t, claim)
	first := prepareNativeTestClaim(t, driver, claim)
	second := prepareNativeTestClaim(t, driver, claim)
	if len(first.Devices) != 1 || first.Devices[0].CDIDeviceIDs[0] != second.Devices[0].CDIDeviceIDs[0] || broker.sequence != 1 {
		t.Fatalf("repeated prepare changed lease or CDI identity: %+v %+v", first, second)
	}
	state := driver.states[claim.UID]
	raw, err := os.ReadFile(driver.cdiPath(state))
	if err != nil {
		t.Fatal(err)
	}
	var spec cdspec.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	edits := spec.Devices[0].ContainerEdits
	if spec.Kind != nativeCDIKind || len(edits.DeviceNodes) != 0 || len(edits.Mounts) != 1 {
		t.Fatalf("native client must receive a service directory, not a Linux GPU: %+v", spec)
	}
	mount := edits.Mounts[0]
	if mount.ContainerPath != nativeContainerDir || !strings.Contains(strings.Join(mount.Options, ","), "ro") || strings.Contains(mount.HostPath, "admin") {
		t.Fatalf("invalid native credentials mount: %+v", mount)
	}
	entries, _ := os.ReadDir(mount.HostPath)
	if len(entries) != 2 || entries[0].Name() != "ca.crt" || entries[1].Name() != "credentials.json" {
		t.Fatalf("only lease credentials and CA may be mounted: %v", entries)
	}
	var credentials nativeCredentials
	raw, _ = os.ReadFile(filepath.Join(mount.HostPath, "credentials.json"))
	if err := json.Unmarshal(raw, &credentials); err != nil || credentials.Token != broker.lease.Token || credentials.CAFile != nativeContainerDir+"/ca.crt" {
		t.Fatalf("invalid leased credentials: %v", err)
	}
	if stat, _ := os.Stat(driver.claimDir(state)); stat.Mode().Perm() != 0o700 {
		t.Fatal("host checkpoint must be private")
	}
	if stat, _ := os.Stat(filepath.Join(driver.claimDir(state), "state.json")); stat.Mode().Perm() != 0o600 {
		t.Fatal("host state file must be private")
	}
}

func TestNativePrepareRejectsForeignSharedAndUnverifiedClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*resourceapi.ResourceClaim)
	}{
		{"foreign-node", func(claim *resourceapi.ResourceClaim) { claim.Status.Allocation.Devices.Results[0].Pool = "node-two" }},
		{"foreign-device", func(claim *resourceapi.ResourceClaim) { claim.Status.Allocation.Devices.Results[0].Device = "venus-0" }},
		{"shared", func(claim *resourceapi.ResourceClaim) {
			uid := types.UID("share")
			claim.Status.Allocation.Devices.Results[0].ShareID = &uid
		}},
		{"admin", func(claim *resourceapi.ResourceClaim) {
			yes := true
			claim.Status.Allocation.Devices.Results[0].AdminAccess = &yes
		}},
		{"duplicate", func(claim *resourceapi.ResourceClaim) {
			claim.Status.Allocation.Devices.Results = append(claim.Status.Allocation.Devices.Results, claim.Status.Allocation.Devices.Results[0])
		}},
		{"not-allocated", func(claim *resourceapi.ResourceClaim) { claim.Status.Allocation = nil }},
		{"deleting", func(claim *resourceapi.ResourceClaim) { now := metav1.Now(); claim.DeletionTimestamp = &now }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := nativeTestClaim("claim-one")
			test.mutate(claim)
			driver, broker, _ := newNativeTestDriver(t, claim)
			results, err := driver.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
			if err != nil || results[claim.UID].Err == nil || broker.acquires != 0 {
				t.Fatalf("invalid claim acquired a host lease: result=%+v error=%v acquires=%d", results[claim.UID], err, broker.acquires)
			}
		})
	}
	claim := nativeTestClaim("claim-one")
	driver, broker, _ := newNativeTestDriver(t)
	results, err := driver.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
	if err != nil || results[claim.UID].Err == nil || broker.acquires != 0 {
		t.Fatal("claim absent from Kubernetes acquired a host lease")
	}
}

func TestNativeBrokerEnforcesExclusivityAndDriverFailsClosed(t *testing.T) {
	one, two := nativeTestClaim("claim-one"), nativeTestClaim("claim-two")
	driver, broker, _ := newNativeTestDriver(t, one, two)
	prepareNativeTestClaim(t, driver, one)
	results, err := driver.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{two})
	if err != nil || results[two.UID].Err == nil || len(driver.states) != 1 {
		t.Fatal("a second claim bypassed broker exclusivity")
	}
	broker.unavailable = true
	results, err = driver.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{one})
	if err != nil || results[one.UID].Err == nil {
		t.Fatal("prepare succeeded with an unavailable broker")
	}
}

func TestNativeDriverRestartRenewalAndBrokerRestartRotateMountedCredentials(t *testing.T) {
	claim := nativeTestClaim("claim-one")
	driver, broker, _ := newNativeTestDriver(t, claim)
	prepareNativeTestClaim(t, driver, claim)
	oldLease := driver.states[claim.UID].Lease
	replacement := restartedNativeTestDriver(driver)
	if err := replacement.loadStates(); err != nil {
		t.Fatal(err)
	}
	prepareNativeTestClaim(t, replacement, claim)
	if replacement.states[claim.UID].Lease.ID != oldLease.ID || broker.sequence != 1 {
		t.Fatal("driver restart did not preserve the live lease")
	}
	// The broker deliberately invalidates leases on restart. Reconciliation
	// rechecks the live claim before replacing the mounted credentials file.
	broker.lease = nativeLease{}
	state := replacement.states[claim.UID]
	if err := replacement.reconcile(t.Context(), state.Lease.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if state.Lease.Token == oldLease.Token || state.Lease.ID == oldLease.ID {
		t.Fatal("broker restart did not rotate credentials")
	}
	raw, err := os.ReadFile(filepath.Join(replacement.claimDir(state), "credentials", "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var credentials nativeCredentials
	if err := json.Unmarshal(raw, &credentials); err != nil || credentials.Token != state.Lease.Token {
		t.Fatal("restarted broker credentials were not published to the mounted directory")
	}
}

func TestNativeRenewalNeverExtendsAnUnverifiedOrDeletedClaim(t *testing.T) {
	claim := nativeTestClaim("claim-one")
	driver, broker, client := newNativeTestDriver(t, claim)
	prepareNativeTestClaim(t, driver, claim)
	state := driver.states[claim.UID]
	before := broker.acquires
	client.PrependReactor("get", "resourceclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("API temporarily unavailable")
	})
	if err := driver.reconcile(t.Context(), state.Lease.ExpiresAt); err == nil || broker.acquires != before || broker.revokes != 0 {
		t.Fatal("an unverified claim was renewed or unnecessarily revoked")
	}
	client.ReactionChain = client.ReactionChain[1:]
	if err := client.ResourceV1().ResourceClaims(claim.Namespace).Delete(t.Context(), claim.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := driver.reconcile(t.Context(), time.Now()); err != nil || broker.acquires != before || broker.revokes != 1 || len(driver.states) != 0 {
		t.Fatalf("deleted claim was not revoked: err=%v acquires=%d revokes=%d states=%d", err, broker.acquires, broker.revokes, len(driver.states))
	}
}

func TestNativeUnpreparePersistsRevocationAcrossDriverAndBrokerOutage(t *testing.T) {
	claim := nativeTestClaim("claim-one")
	driver, broker, _ := newNativeTestDriver(t, claim)
	prepareNativeTestClaim(t, driver, claim)
	state := driver.states[claim.UID]
	request := []kubeletplugin.NamespacedObject{{NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, UID: claim.UID}}
	broker.unavailable = true
	results, err := driver.UnprepareResourceClaims(t.Context(), request)
	if err != nil || results[claim.UID] == nil || !state.PendingRevoke {
		t.Fatal("failed revocation was not persisted for retry")
	}
	if _, err := os.Stat(filepath.Join(driver.claimDir(state), "credentials")); !os.IsNotExist(err) {
		t.Fatal("credentials remained readable after Unprepare")
	}
	replacement := restartedNativeTestDriver(driver)
	if err := replacement.loadStates(); err != nil {
		t.Fatal(err)
	}
	broker.unavailable = false
	if err := replacement.reconcile(t.Context(), time.Now()); err != nil || len(replacement.states) != 0 || broker.lease.ID != "" {
		t.Fatal("restarted driver did not finish pending revocation")
	}
	results, err = replacement.UnprepareResourceClaims(t.Context(), request)
	if err != nil || results[claim.UID] != nil {
		t.Fatal("duplicate Unprepare must be idempotent")
	}
}

func TestNativeReallocatedClaimRevokesOldIdentity(t *testing.T) {
	claim := nativeTestClaim("claim-one")
	driver, broker, client := newNativeTestDriver(t, claim)
	prepareNativeTestClaim(t, driver, claim)
	changed := claim.DeepCopy()
	changed.Status.Allocation.Devices.Results[0].Pool = "node-two"
	if _, err := client.ResourceV1().ResourceClaims(claim.Namespace).Update(t.Context(), changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := driver.reconcile(t.Context(), time.Now()); err != nil || broker.revokes != 1 || len(driver.states) != 0 {
		t.Fatal("changed allocation retained a host lease")
	}
}

func TestNativeHealthReportExpiresInsteadOfInventingFreshChecks(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "stale"}[stale], func(t *testing.T) {
			driver, _, _ := newNativeTestDriver(t)
			driver.healthy, driver.healthCheckedAt = true, time.Now()
			if stale {
				driver.healthCheckedAt = driver.healthCheckedAt.Add(-time.Minute)
			}
			ctx, cancel := context.WithCancel(t.Context())
			reports := make(chan kubeletplugin.DeviceHealthReport)
			done := make(chan error, 1)
			go func() { done <- driver.WatchHealthStatus(ctx, reports) }()
			report := <-reports
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			expected := kubeletplugin.HealthStatusHealthy
			if stale {
				expected = kubeletplugin.HealthStatusUnhealthy
			}
			if len(report.Devices) != 1 || report.Devices[0].Health != expected || report.Devices[0].LastUpdated != driver.healthCheckedAt {
				t.Fatalf("stale health was refreshed without checking the broker: %+v", report)
			}
		})
	}
}
