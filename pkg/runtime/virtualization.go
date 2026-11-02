package runtime

import "fmt"

// CheckNestedVirtualization verifies the host's native nested virtualization
// capability without creating a VM. Call it before downloading create assets;
// the backends also check at boot so direct callers cannot silently lose it.
func CheckNestedVirtualization() error {
	return requireNestedVirtualization(nil)
}

func requireNestedVirtualization(probe func() (bool, error)) error {
	if probe == nil {
		probe = hostNestedVirtualizationSupported
	}
	supported, err := probe()
	if err != nil {
		return fmt.Errorf("cannot verify host support for --virtualization (requires an M3 or newer Mac on macOS 15+): %w", err)
	}
	if !supported {
		return fmt.Errorf("this host does not support --virtualization; nested virtualization requires an M3 or newer Mac on macOS 15+")
	}
	return nil
}
