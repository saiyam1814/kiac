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
	"1.34": "docker.io/kindest/node:v1.34.8@sha256:02722c2dedddcfc00febf5d27fbeb9b7b2c14294c82109ff4a85d89ac9ba3256",
	"1.35": "docker.io/kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95",
	"1.36": "docker.io/kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5",
}

// DefaultK8sVersion is the minor used when neither --k8s-version nor
// --image is given.
const DefaultK8sVersion = "1.36"

// ResolveImage maps a Kubernetes version like "1.36", "v1.36" or
// "1.36.1" to a pinned node image. Full patch versions outside the pin
// table fall back to the matching kindest/node tag, unpinned.
func ResolveImage(version string) (string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid Kubernetes version %q (want e.g. 1.36)", version)
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
