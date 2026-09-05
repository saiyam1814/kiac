package cluster

import _ "embed"

// kubeadmSystemdStorageManifest is Rancher local-path-provisioner v0.0.37,
// rendered for Kiac's systemd-backed Fedora nodes with immutable image pins.
//
//go:embed assets/kubeadm/local-path-storage.yaml
var kubeadmSystemdStorageManifest string
