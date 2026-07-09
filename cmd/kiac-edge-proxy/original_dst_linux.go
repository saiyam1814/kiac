//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"unsafe"
)

const soOriginalDst = 80

type rawSockaddrInet4 struct {
	Family uint16
	Port   [2]byte
	Addr   [4]byte
	Zero   [8]byte
}

func originalDst(conn *net.TCPConn) (string, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return "", err
	}

	var dst rawSockaddrInet4
	size := uint32(unsafe.Sizeof(dst))
	var sockErr syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		_, _, sockErr = syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(syscall.SOL_IP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&dst)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
	}); err != nil {
		return "", err
	}
	if sockErr != 0 {
		return "", sockErr
	}
	if dst.Family != syscall.AF_INET {
		return "", fmt.Errorf("unexpected original destination family %d", dst.Family)
	}

	ip := net.IPv4(dst.Addr[0], dst.Addr[1], dst.Addr[2], dst.Addr[3])
	port := int(binary.BigEndian.Uint16(dst.Port[:]))
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}
