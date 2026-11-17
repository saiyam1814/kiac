//go:build !darwin && !linux

package nativegpu

import "errors"

func lockState(string) (func(), error) {
	return nil, errors.New("native GPU host service requires macOS on Apple silicon")
}
