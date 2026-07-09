//go:build !linux

package main

import "syscall"

func setTCPMaxSeg(_ syscall.RawConn, _ int) error {
	return nil
}
