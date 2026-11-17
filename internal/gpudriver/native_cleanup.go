package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// runNativeCleanup is an explicit offline maintenance mode. The installer first
// stops the renewing driver and verifies that no native claims are allocated.
// This process does not load Kubernetes credentials or renew any service lease.
func runNativeCleanup(ctx context.Context, opts options) error {
	if opts.nodeName == "" || opts.native.selectedNode == "" || opts.native.selectedNode != opts.nodeName {
		return errors.New("native cleanup requires --node-name and an identical explicit --native-node-name")
	}
	broker, ca, err := newNativeBrokerClient(opts.native.endpoint, opts.native.caFile, opts.native.adminTokenFile)
	if err != nil {
		return err
	}
	defer broker.client.CloseIdleConnections()
	if opts.pluginsDir == "" {
		opts.pluginsDir = kubeletplugin.KubeletPluginsDir
	}
	pluginDir := filepath.Join(opts.pluginsDir, nativeDriverName)
	unlock, err := lockNativeCheckpoint(pluginDir)
	if err != nil {
		return err
	}
	defer unlock()
	driver := &nativeDriver{nodeName: opts.nodeName, endpoint: broker.endpoint, ca: ca,
		cdiRoot: opts.cdiRoot, stateRoot: filepath.Join(pluginDir, "native-state"),
		broker: broker, states: map[types.UID]*nativeClaimState{}}
	if err := driver.loadStates(); err != nil {
		return fmt.Errorf("load native cleanup checkpoint: %w", err)
	}
	return driver.cleanupClaims(ctx)
}

func (d *nativeDriver) cleanupClaims(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, 0, len(d.states))
	for uid := range d.states {
		keys = append(keys, string(uid))
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := d.states[types.UID(key)]
		state.PendingRevoke = true
		if err := d.writeState(state); err != nil {
			return fmt.Errorf("persist native cleanup revocation: %w", err)
		}
		// Keep checkpoint and mount artifacts when the broker cannot confirm
		// revocation. The installer must not report a completed uninstall.
		if err := d.broker.Revoke(ctx, state.Lease.ID); err != nil {
			return fmt.Errorf("native lease cleanup requires broker-confirmed revocation: %w", err)
		}
		if err := d.removeState(state); err != nil {
			return fmt.Errorf("remove revoked native claim artifacts: %w", err)
		}
	}
	// Remove only empty owned checkpoint directories. Never recursively remove
	// the plugin root, shared agent binary, other drivers or unknown artifacts.
	for _, dir := range []string{filepath.Join(d.stateRoot, "claims"), d.stateRoot} {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("native checkpoint directory contains unexpected remaining artifacts: %w", err)
		}
	}
	return nil
}

func nativeOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func ensureNativePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !nativeOwnedByCurrentUser(info) {
		return errors.New("native checkpoint directory must be a real, owned, private directory")
	}
	return nil
}

func lockNativeCheckpoint(pluginDir string) (func(), error) {
	if err := ensureNativePrivateDirectory(pluginDir); err != nil {
		return nil, err
	}
	path := filepath.Join(pluginDir, "native.lock")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !nativeOwnedByCurrentUser(info) {
			return nil, errors.New("native checkpoint lock must be a private owned regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another native DRA driver or cleanup job owns this node checkpoint")
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
