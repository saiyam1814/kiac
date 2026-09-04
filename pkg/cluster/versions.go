package cluster

import (
	"fmt"
	"sort"
	"strings"
)

// nodeImages pins the newest kindest/node build of each Kubernetes minor
// to its exact digest; node images are only tested at exact digests.
// Refresh with: hub.docker.com/v2/repositories/kindest/node/tags
var nodeImages = map[string]string{
	"1.32": "docker.io/kindest/node:v1.32.11@sha256:5fc52d52a7b9574015299724bd68f183702956aa4a2116ae75a63cb574b35af8",
	"1.33": "docker.io/kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4",
	"1.34": "docker.io/kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d",
	"1.35": "docker.io/kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0",
	"1.36": "docker.io/kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed",
	"1.37": "docker.io/kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5",
}

// Defaults are distro-specific because upstream Kubernetes and k3s do
// not publish new minors at the same time.
const (
	DefaultK8sVersion = "1.37"
	DefaultK3sVersion = "1.36"
)

// ResolveImage maps a Kubernetes version like "1.37", "v1.37" or
// "1.37.0" to a pinned node image. Full patch versions outside the pin
// table fall back to the matching kindest/node tag, unpinned.
func ResolveImage(version string) (string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid Kubernetes version %q (want e.g. %s)", version, DefaultK8sVersion)
	}
	minor := parts[0] + "." + parts[1]
	if img, ok := nodeImages[minor]; ok && len(parts) == 2 {
		return img, nil
	}
	if len(parts) == 3 {
		if img, ok := nodeImages[minor]; ok && strings.Contains(img, ":v"+v+"@") {
			return img, nil
		}
		return "docker.io/kindest/node:v" + v, nil
	}
	return "", fmt.Errorf("unsupported Kubernetes version %q (supported minors: %s)", version, strings.Join(SupportedVersions(), ", "))
}

// SupportedVersions lists pinned minors, newest first.
func SupportedVersions() []string {
	out := make([]string, 0, len(nodeImages))
	for v := range nodeImages {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// k3sImages pins the newest rancher/k3s build of each Kubernetes minor
// to its multi-arch manifest digest (linux/arm64 included); like
// nodeImages, k3s images are only tested at exact digests.
// Refresh with: hub.docker.com/v2/repositories/rancher/k3s/tags
var k3sImages = map[string]string{
	"1.32": "docker.io/rancher/k3s:v1.32.13-k3s1@sha256:7534b63e02277917f77c584ed5532b31562c760d6bb8fe88059002e9bdeee033",
	"1.33": "docker.io/rancher/k3s:v1.33.13-k3s1@sha256:523cfdf26aaef2c3164eefa30a61f5f1dca86d1cf3f1d38beae62ac65905a3ab",
	"1.34": "docker.io/rancher/k3s:v1.34.11-k3s1@sha256:5d52389a0f4fd7ebdb5a1fb2d7c67c35da966230782c4abb0667d86bcccea9c2",
	"1.35": "docker.io/rancher/k3s:v1.35.8-k3s1@sha256:59fe491fd3b73204e499e40b325240d85c42c7189c3ae50150d37b78243f3b32",
	"1.36": "docker.io/rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657",
}

// ResolveK3sImage maps a Kubernetes version to a pinned rancher/k3s
// image, mirroring ResolveImage. Full patch versions outside the pin
// table fall back to the matching -k3s1 tag, unpinned.
func ResolveK3sImage(version string) (string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid Kubernetes version %q (want e.g. %s)", version, DefaultK3sVersion)
	}
	minor := parts[0] + "." + parts[1]
	if img, ok := k3sImages[minor]; ok && len(parts) == 2 {
		return img, nil
	}
	if len(parts) == 3 {
		if img, ok := k3sImages[minor]; ok && strings.Contains(img, ":v"+v+"-k3s") {
			return img, nil
		}
		return "docker.io/rancher/k3s:v" + v + "-k3s1", nil
	}
	return "", fmt.Errorf("no pinned k3s image for Kubernetes %q (k3s-supported minors: %s)", version, strings.Join(SupportedK3sVersions(), ", "))
}

// SupportedK3sVersions lists minors with a pinned k3s image, newest first.
func SupportedK3sVersions() []string {
	out := make([]string, 0, len(k3sImages))
	for v := range k3sImages {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
