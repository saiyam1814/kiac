package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	cdspec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	nativeDriverName   = "metal.kiac.dev"
	nativeDeviceName   = "metal-0"
	nativeCDIKind      = "kiac.dev/metal"
	nativeContainerDir = "/var/run/kiac-metal"
	nativeHealthTaint  = "metal.kiac.dev/unavailable"
	nativePollInterval = 5 * time.Second
)

type nativeOptions struct {
	endpoint       string
	caFile         string
	adminTokenFile string
	selectedNode   string
	leaseTTL       time.Duration
}

// Credentials grant access only to the broker-owned service lease. They do not
// grant access to a Linux GPU device, macOS itself or the broker admin API.
type nativeCredentials struct {
	Version   int       `json:"version"`
	Endpoint  string    `json:"endpoint"`
	LeaseID   string    `json:"leaseID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	CAFile    string    `json:"caFile"`
}

type nativeClaimState struct {
	Namespace     string      `json:"namespace"`
	Name          string      `json:"name"`
	UID           types.UID   `json:"uid"`
	NodeName      string      `json:"nodeName"`
	Lease         nativeLease `json:"lease"`
	PendingRevoke bool        `json:"pendingRevoke,omitempty"`
}

type nativeDriver struct {
	mu                 sync.Mutex
	nodeName           string
	endpoint           string
	ca                 []byte
	cdiRoot            string
	stateRoot          string
	ttl                time.Duration
	client             kubernetes.Interface
	broker             nativeBroker
	states             map[types.UID]*nativeClaimState
	healthy            bool
	healthCheckedAt    time.Time
	inferenceAvailable bool
	cancel             context.CancelCauseFunc
}

func runNativeDriver(ctx context.Context, opts options) error {
	if opts.nodeName == "" || opts.podUID == "" {
		return errors.New("--node-name and --pod-uid are required in native-dra mode")
	}
	if opts.native.selectedNode == "" || opts.native.selectedNode != opts.nodeName {
		return errors.New("--native-node-name must explicitly select this one node; do not deploy native-dra on every worker")
	}
	if opts.native.leaseTTL < 30*time.Second || opts.native.leaseTTL > 300*time.Second || opts.native.leaseTTL%time.Second != 0 {
		return errors.New("--native-lease-ttl must be a whole number of seconds between 30s and 300s")
	}
	broker, ca, err := newNativeBrokerClient(opts.native.endpoint, opts.native.caFile, opts.native.adminTokenFile)
	if err != nil {
		return err
	}
	defer broker.client.CloseIdleConnections()
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster Kubernetes credentials: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	if opts.pluginsDir == "" {
		opts.pluginsDir = kubeletplugin.KubeletPluginsDir
	}
	pluginDir := filepath.Join(opts.pluginsDir, nativeDriverName)
	// One owner of the checkpoint prevents an old renewing instance from
	// racing Unprepare in its replacement. The installer uses Recreate.
	unlock, err := lockNativeCheckpoint(pluginDir)
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithCancelCause(klog.NewContext(ctx, klog.Background()))
	defer cancel(nil)
	drv := &nativeDriver{nodeName: opts.nodeName, endpoint: broker.endpoint, ca: ca,
		cdiRoot: opts.cdiRoot, stateRoot: filepath.Join(pluginDir, "native-state"), ttl: opts.native.leaseTTL,
		client: client, broker: broker, states: map[types.UID]*nativeClaimState{}, cancel: cancel}
	if err := drv.loadStates(); err != nil {
		return fmt.Errorf("load native DRA checkpoint: %w", err)
	}
	helper, err := kubeletplugin.Start(ctx, drv,
		kubeletplugin.KubeClient(client), kubeletplugin.NodeName(opts.nodeName),
		kubeletplugin.DriverName(nativeDriverName), kubeletplugin.RegistrarDirectoryPath(opts.registrarDir),
		kubeletplugin.PluginDataDirectoryPath(pluginDir), kubeletplugin.RollingUpdate(types.UID(opts.podUID)),
		kubeletplugin.HealthService(true), kubeletplugin.CDIDirectory(opts.cdiRoot))
	if err != nil {
		return fmt.Errorf("start native Metal DRA plugin: %w", err)
	}
	defer helper.Stop()
	if err := helper.PublishResources(ctx, nativeResources(opts.nodeName, opts.podUID, false, false)); err != nil {
		return fmt.Errorf("publish initial native Metal resources: %w", err)
	}
	previousHealth, previousInference := false, false
	ticker := time.NewTicker(nativePollInterval)
	defer ticker.Stop()
	for {
		health, healthErr := broker.Health(ctx)
		checkedAt := time.Now()
		reconcileErr := drv.reconcile(ctx, time.Now())
		ready := healthErr == nil && reconcileErr == nil
		drv.mu.Lock()
		drv.healthy, drv.inferenceAvailable = ready, health.InferenceAvailable && ready
		drv.healthCheckedAt = checkedAt
		inference := drv.inferenceAvailable
		drv.mu.Unlock()
		if ready != previousHealth || inference != previousInference {
			if err := helper.PublishResources(ctx, nativeResources(opts.nodeName, opts.podUID, ready, inference)); err != nil {
				return fmt.Errorf("publish native Metal health: %w", err)
			}
			previousHealth, previousInference = ready, inference
		}
		if reconcileErr != nil {
			klog.FromContext(ctx).Error(reconcileErr, "native Metal lease reconciliation deferred; existing tokens expire without renewal")
		}
		select {
		case <-ctx.Done():
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return nil
		case <-ticker.C:
		}
	}
}

func nativeResources(nodeName, podUID string, healthy, inference bool) resourceslice.DriverResources {
	api, execution, allocation := "native-metal", "macos-host-service", "exclusive-service-lease"
	device := resourceapi.Device{Name: nativeDeviceName, Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"api": {StringValue: &api}, "execution": {StringValue: &execution}, "allocation": {StringValue: &allocation},
		"inferenceAvailable": {BoolValue: &inference},
		// The installer must distinguish this process's inventory from a still
		// healthy ResourceSlice left by a previous driver during rollout.
		"driverPodUID": {StringValue: &podUID},
	}}
	if !healthy {
		device.Taints = []resourceapi.DeviceTaint{{Key: nativeHealthTaint, Effect: resourceapi.DeviceTaintEffectNoSchedule}}
	}
	// No AllowMultipleAllocations or memory capacity: there is one broker lease,
	// not a per-worker copy of the Mac GPU and not isolated physical VRAM.
	return resourceslice.DriverResources{Pools: map[string]resourceslice.Pool{
		nodeName: {Slices: []resourceslice.Slice{{Devices: []resourceapi.Device{device}}}},
	}}
}

func nativeClaimAllocation(claim *resourceapi.ResourceClaim, node string) ([]resourceapi.DeviceRequestAllocationResult, error) {
	if claim == nil || claim.UID == "" || claim.Name == "" || claim.Namespace == "" || claim.DeletionTimestamp != nil || claim.Status.Allocation == nil {
		return nil, errors.New("native ResourceClaim must exist and be allocated")
	}
	var allocations []resourceapi.DeviceRequestAllocationResult
	for _, allocation := range claim.Status.Allocation.Devices.Results {
		if allocation.Driver != nativeDriverName {
			continue
		}
		if allocation.Pool != node || allocation.Device != nativeDeviceName || allocation.Request == "" {
			return nil, errors.New("native ResourceClaim has an allocation for an unknown node or device")
		}
		if allocation.ShareID != nil || len(allocation.ConsumedCapacity) != 0 || (allocation.AdminAccess != nil && *allocation.AdminAccess) {
			return nil, errors.New("native Metal alpha supports exclusive service leases only")
		}
		allocations = append(allocations, allocation)
	}
	if len(allocations) != 1 {
		return nil, errors.New("native ResourceClaim must contain exactly one metal.kiac.dev allocation")
	}
	return allocations, nil
}

func (d *nativeDriver) currentClaim(ctx context.Context, state *nativeClaimState) (*resourceapi.ResourceClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	claim, err := d.client.ResourceV1().ResourceClaims(state.Namespace).Get(ctx, state.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if claim.UID != state.UID {
		return nil, fmt.Errorf("%w: UID changed", errNativeClaimChanged)
	}
	if _, err := nativeClaimAllocation(claim, d.nodeName); err != nil {
		return nil, fmt.Errorf("%w: %v", errNativeClaimChanged, err)
	}
	return claim, nil
}

func (d *nativeDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		if claim == nil {
			return nil, errors.New("nil native ResourceClaim")
		}
		prepared := kubeletplugin.PrepareResult{}
		allocations, err := nativeClaimAllocation(claim, d.nodeName)
		if err == nil {
			prepared, err = d.prepareClaim(ctx, claim, allocations)
		}
		prepared.Err = err
		results[claim.UID] = prepared
	}
	return results, nil
}

func (d *nativeDriver) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim, allocations []resourceapi.DeviceRequestAllocationResult) (kubeletplugin.PrepareResult, error) {
	prepared := kubeletplugin.PrepareResult{}
	state := d.states[claim.UID]
	if state != nil && (state.PendingRevoke || state.Name != claim.Name || state.Namespace != claim.Namespace) {
		return prepared, errors.New("native claim has a pending revocation or changed identity")
	}
	if state == nil {
		state = &nativeClaimState{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID, NodeName: d.nodeName}
	}
	if _, err := d.currentClaim(ctx, state); err != nil {
		return prepared, fmt.Errorf("verify current native ResourceClaim: %w", err)
	}
	health, err := d.broker.Health(ctx)
	if err != nil {
		return prepared, err
	}
	lease, err := d.broker.Acquire(ctx, d.leaseRequest(state))
	if err != nil {
		return prepared, err
	}
	state.Lease = lease
	d.states[state.UID] = state
	if err := d.writeState(state); err == nil {
		err = d.writeCredentials(state)
		if err == nil {
			err = d.writeCDI(state)
		}
		if err == nil {
			api, execution := "native-metal", "macos-host-service"
			prepared.Devices = []kubeletplugin.Device{{Requests: []string{allocations[0].Request}, PoolName: d.nodeName,
				DeviceName: nativeDeviceName, CDIDeviceIDs: []string{nativeCDIKind + "=claim-" + nativeClaimKey(state.UID)},
				Metadata: &kubeletplugin.DeviceMetadata{Attributes: map[string]resourceapi.DeviceAttribute{
					"api": {StringValue: &api}, "execution": {StringValue: &execution}, "inferenceAvailable": {BoolValue: &health.InferenceAvailable},
				}}}}
			return prepared, nil
		}
	}
	state.PendingRevoke = true
	_ = d.writeState(state)
	_ = d.removeCredentials(state)
	if revokeErr := d.broker.Revoke(ctx, state.Lease.ID); revokeErr == nil {
		_ = d.removeState(state)
	}
	return prepared, errors.New("persist native lease credentials or CDI specification failed")
}

func (d *nativeDriver) leaseRequest(state *nativeClaimState) nativeLeaseRequest {
	return nativeLeaseRequest{Namespace: state.Namespace, ClaimUID: string(state.UID), NodeName: d.nodeName, TTLSeconds: int(d.ttl / time.Second)}
}

func (d *nativeDriver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	results := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		state := d.states[claim.UID]
		if state == nil {
			results[claim.UID] = nil
			continue
		}
		state.PendingRevoke = true
		persistErr := d.writeState(state)
		credentialsErr := d.removeCredentials(state)
		if err := d.broker.Revoke(ctx, state.Lease.ID); err != nil {
			results[claim.UID] = errors.Join(persistErr, credentialsErr, err)
			continue
		}
		results[claim.UID] = errors.Join(credentialsErr, d.removeState(state))
	}
	return results, nil
}

func (d *nativeDriver) reconcile(ctx context.Context, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var failures []error
	for _, state := range d.states {
		if !state.PendingRevoke {
			if _, err := d.currentClaim(ctx, state); err != nil {
				// A transient API error must not revoke a valid claim or renew an
				// unverified one. Its current token still has a bounded expiry.
				if !apierrors.IsNotFound(err) && !errors.Is(err, errNativeClaimChanged) {
					failures = append(failures, errors.New("cannot verify native ResourceClaim; lease was not renewed"))
					continue
				}
				state.PendingRevoke = true
				if err := d.writeState(state); err != nil {
					failures = append(failures, err)
					continue
				}
				_ = d.removeCredentials(state)
			}
		}
		if state.PendingRevoke {
			if err := d.broker.Revoke(ctx, state.Lease.ID); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := d.removeState(state); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if now.Before(state.Lease.ExpiresAt.Add(-d.ttl * 2 / 3)) {
			continue
		}
		lease, err := d.broker.Acquire(ctx, d.leaseRequest(state))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		state.Lease = lease
		if err := d.writeState(state); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := d.writeCredentials(state); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

var errNativeClaimChanged = errors.New("native ResourceClaim allocation changed")

func nativeClaimKey(uid types.UID) string {
	hash := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(hash[:])
}

func (d *nativeDriver) claimDir(state *nativeClaimState) string {
	return filepath.Join(d.stateRoot, "claims", nativeClaimKey(state.UID))
}

func (d *nativeDriver) writeState(state *nativeClaimState) error {
	if err := os.MkdirAll(d.claimDir(state), 0o700); err != nil {
		return err
	}
	return writeNativeJSON(filepath.Join(d.claimDir(state), "state.json"), state, 0o600)
}

func (d *nativeDriver) writeCredentials(state *nativeClaimState) error {
	dir := filepath.Join(d.claimDir(state), "credentials")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeNativeBytes(filepath.Join(dir, "ca.crt"), d.ca, 0o444); err != nil {
		return err
	}
	credentials := nativeCredentials{Version: 1, Endpoint: d.endpoint, LeaseID: state.Lease.ID, Token: state.Lease.Token,
		ExpiresAt: state.Lease.ExpiresAt, CAFile: nativeContainerDir + "/ca.crt"}
	return writeNativeJSON(filepath.Join(dir, "credentials.json"), credentials, 0o444)
}

func (d *nativeDriver) writeCDI(state *nativeClaimState) error {
	if err := os.MkdirAll(d.cdiRoot, 0o755); err != nil {
		return err
	}
	spec := cdspec.Spec{Version: "0.6.0", Kind: nativeCDIKind, Devices: []cdspec.Device{{
		Name: "claim-" + nativeClaimKey(state.UID), ContainerEdits: cdspec.ContainerEdits{
			Env: []string{"KIAC_GPU_API=native-metal", "KIAC_METAL_CREDENTIALS=" + nativeContainerDir + "/credentials.json"},
			Mounts: []*cdspec.Mount{{HostPath: filepath.Join(d.claimDir(state), "credentials"), ContainerPath: nativeContainerDir,
				Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}}},
		},
	}}}
	return writeNativeJSON(d.cdiPath(state), spec, 0o644)
}

func (d *nativeDriver) cdiPath(state *nativeClaimState) string {
	return filepath.Join(d.cdiRoot, "kiac-metal-"+nativeClaimKey(state.UID)+".json")
}

func (d *nativeDriver) removeCredentials(state *nativeClaimState) error {
	err := os.Remove(d.cdiPath(state))
	if os.IsNotExist(err) {
		err = nil
	}
	return errors.Join(err, os.RemoveAll(filepath.Join(d.claimDir(state), "credentials")))
}

func (d *nativeDriver) removeState(state *nativeClaimState) error {
	if err := d.removeCredentials(state); err != nil {
		return err
	}
	if err := os.RemoveAll(d.claimDir(state)); err != nil {
		return err
	}
	delete(d.states, state.UID)
	return nil
}

func (d *nativeDriver) loadStates() error {
	if err := ensureNativePrivateDirectory(d.stateRoot); err != nil {
		return err
	}
	if err := ensureNativePrivateDirectory(filepath.Join(d.stateRoot, "claims")); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(d.stateRoot, "claims"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return errors.New("unexpected file in native claim checkpoint directory")
		}
		dir := filepath.Join(d.stateRoot, "claims", entry.Name())
		if err := ensureNativePrivateDirectory(dir); err != nil {
			return err
		}
		statePath := filepath.Join(dir, "state.json")
		info, err := os.Lstat(statePath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !nativeOwnedByCurrentUser(info) {
			return errors.New("native claim checkpoint must be a private owned regular file")
		}
		raw, err := readNativeFile(statePath, 16<<10)
		if err != nil {
			return err
		}
		state := &nativeClaimState{}
		if err := json.Unmarshal(raw, state); err != nil || state.UID == "" || state.Namespace == "" || state.Name == "" ||
			state.NodeName != d.nodeName || nativeClaimKey(state.UID) != entry.Name() {
			return errors.New("invalid native claim checkpoint")
		}
		if state.Lease.ID == "" || !validNativeToken(state.Lease.Token) || state.Lease.ExpiresAt.IsZero() {
			return errors.New("invalid native lease checkpoint")
		}
		d.states[state.UID] = state
	}
	return nil
}

func writeNativeJSON(path string, value any, mode os.FileMode) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeNativeBytes(path, append(raw, '\n'), mode)
}

func writeNativeBytes(path string, raw []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".native-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (d *nativeDriver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) {
		d.cancel(fmt.Errorf("%s: %w", msg, err))
	}
}

func (d *nativeDriver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	ticker := time.NewTicker(nativePollInterval)
	defer ticker.Stop()
	for {
		d.mu.Lock()
		checkedAt := d.healthCheckedAt
		healthy := d.healthy && !checkedAt.IsZero() && time.Since(checkedAt) <= 3*nativePollInterval
		d.mu.Unlock()
		status := kubeletplugin.HealthStatusUnhealthy
		if healthy {
			status = kubeletplugin.HealthStatusHealthy
		}
		report := kubeletplugin.DeviceHealthReport{Devices: []kubeletplugin.DeviceHealth{{PoolName: d.nodeName, DeviceName: nativeDeviceName,
			Health: status, LastUpdated: checkedAt, HealthCheckTimeout: 3 * nativePollInterval, Message: "native host service readiness; not GPU utilization or physical isolation"}}}
		select {
		case reports <- report:
		case <-ctx.Done():
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}
