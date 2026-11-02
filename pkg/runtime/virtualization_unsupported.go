//go:build !darwin || !arm64

package runtime

func hostNestedVirtualizationSupported() (bool, error) {
	return false, nil
}
