package cluster

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// ClusterCapability describes the version choices for one node backend and
// distribution. Ordinary clusters expose their pinned minors; GPU clusters
// expose exact releases because their guest binaries must have verified pins.
type ClusterCapability struct {
	Backend        string              `json:"backend"`
	Distro         string              `json:"distro"`
	Versions       []string            `json:"versions"`
	DefaultVersion string              `json:"defaultVersion"`
	GPUDrivers     map[string][]string `json:"gpuDrivers,omitempty"`
}

// SupportedClusterCapabilities derives UI choices and GPU validation from the
// artifacts each backend actually installs. Adding an OCI version does not
// implicitly advertise GPU binaries that have not been pinned yet.
func SupportedClusterCapabilities() []ClusterCapability {
	capabilities := []ClusterCapability{
		{Backend: runtime.BackendContainer, Distro: "kubeadm", Versions: SupportedVersions(), DefaultVersion: DefaultK8sVersion},
		{Backend: runtime.BackendContainer, Distro: "k3s", Versions: SupportedK3sVersions(), DefaultVersion: DefaultK3sVersion},
	}
	kubeadm := make([]string, 0, len(kubeadmGPUReleases))
	for version := range kubeadmGPUReleases {
		kubeadm = append(kubeadm, version)
	}
	k3s := make([]string, 0, len(k3sARM64Assets))
	for version := range k3sARM64Assets {
		k3s = append(k3s, version)
	}
	for _, capability := range []ClusterCapability{
		{Backend: runtime.BackendKrunkit, Distro: "kubeadm", Versions: kubeadm},
		{Backend: runtime.BackendKrunkit, Distro: "k3s", Versions: k3s},
	} {
		slices.SortFunc(capability.Versions, compareCapabilityVersions)
		capability.GPUDrivers = make(map[string][]string)
		for _, driver := range SupportedGPUResourceDrivers() {
			capability.GPUDrivers[driver] = []string{}
			for _, version := range capability.Versions {
				if driver != "dra" || requireDRAKubernetesVersion(version) == nil {
					capability.GPUDrivers[driver] = append(capability.GPUDrivers[driver], version)
				}
			}
		}
		if len(capability.Versions) > 0 {
			capability.DefaultVersion = capability.Versions[0]
		}
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

// ResolveGPUVersion accepts a pinned minor or exact release and returns the
// exact guest binary release. It performs no host checks or downloads, so all
// callers can reject unsupported choices before starting expensive work.
func ResolveGPUVersion(distro, version, driver string) (string, error) {
	if distro == "" {
		distro = "kubeadm"
	}
	if driver == "" {
		driver = "device-plugin"
	}
	if !slices.Contains(SupportedGPUResourceDrivers(), driver) {
		return "", fmt.Errorf("unknown --gpu-resource-driver %q (supported: %s)", driver, strings.Join(SupportedGPUResourceDrivers(), ", "))
	}
	for _, capability := range SupportedClusterCapabilities() {
		if capability.Backend != runtime.BackendKrunkit || capability.Distro != distro {
			continue
		}
		if version == "" {
			version = capability.DefaultVersion
		}
		requested := strings.TrimPrefix(strings.TrimSpace(version), "v")
		if distro == "k3s" && strings.Count(requested, ".") == 2 {
			requested = strings.Replace(requested, "+k3s", "-k3s", 1)
			if !strings.Contains(requested, "-k3s") {
				requested += "-k3s1"
			}
		}
		for _, release := range capability.Versions {
			canonical := strings.TrimPrefix(release, "v")
			minor := canonical[:strings.LastIndex(canonical, ".")]
			if requested != canonical && requested != minor {
				continue
			}
			if !slices.Contains(capability.GPUDrivers[driver], release) {
				return "", requireDRAKubernetesVersion(release)
			}
			return release, nil
		}
		return "", fmt.Errorf("Kubernetes %q has no pinned %s GPU-node binaries; supported releases: %s", version, distro, strings.Join(capability.Versions, ", "))
	}
	return "", fmt.Errorf("unknown GPU cluster distro %q (supported: kubeadm, k3s)", distro)
}

// compareCapabilityVersions orders exact releases newest first, including
// K3s build revisions. All inputs come from the checked-in artifact tables.
func compareCapabilityVersions(a, b string) int {
	parts := func(version string) []int {
		version = strings.TrimPrefix(version, "v")
		version = strings.ReplaceAll(version, "-k3s", ".")
		result := make([]int, 0, 4)
		for _, value := range strings.Split(version, ".") {
			n, _ := strconv.Atoi(value)
			result = append(result, n)
		}
		return result
	}
	return slices.CompareFunc(parts(b), parts(a), cmp.Compare[int])
}
