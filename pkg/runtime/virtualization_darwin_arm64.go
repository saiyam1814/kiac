package runtime

import (
	"fmt"

	"github.com/ebitengine/purego"
)

// Query Hypervisor.framework rather than inferring support from a CPU name or
// the presence of a CLI flag. This is the native capability used by libkrun:
// https://developer.apple.com/documentation/hypervisor/hv_vm_config_get_el2_supported(_:)
// Load it dynamically so CGO_ENABLED=0 releases work and older macOS versions
// fail with an error if the macOS 15+ symbol is unavailable.
func hostNestedVirtualizationSupported() (bool, error) {
	framework, err := purego.Dlopen("/System/Library/Frameworks/Hypervisor.framework/Hypervisor", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return false, fmt.Errorf("loading Hypervisor.framework: %w", err)
	}
	defer purego.Dlclose(framework)
	symbol, err := purego.Dlsym(framework, "hv_vm_config_get_el2_supported")
	if err != nil {
		return false, fmt.Errorf("nested virtualization capability API is unavailable; update macOS to 15 or newer: %w", err)
	}
	var getEL2Supported func(*bool) int32
	purego.RegisterFunc(&getEL2Supported, symbol)
	var supported bool
	if status := getEL2Supported(&supported); status != 0 {
		return false, fmt.Errorf("hv_vm_config_get_el2_supported failed with status %#x", uint32(status))
	}
	return supported, nil
}
