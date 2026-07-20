//go:build !linux

package main

import (
	"fmt"
	"net"
)

func originalDst(conn *net.TCPConn, v6 bool) (string, error) {
	return "", fmt.Errorf("SO_ORIGINAL_DST is only available on Linux")
}
