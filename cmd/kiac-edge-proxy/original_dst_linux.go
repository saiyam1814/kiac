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

// soOriginalDst is the getsockopt option that returns a REDIRECT'd
// connection's pre-DNAT destination. The value is the same (80) for both
// the IPv4 (SOL_IP) and IPv6 (SOL_IPV6) levels.
const (
	soOriginalDst = 80
	solIPv6       = 41 // syscall.SOL_IPV6 is not exported on all arches
)

type rawSockaddrInet4 struct {
	Family uint16
	Port   [2]byte
	Addr   [4]byte
	Zero   [8]byte
}

type rawSockaddrInet6 struct {
	Family   uint16
	Port     [2]byte
	Flowinfo [4]byte
	Addr     [16]byte
	ScopeID  uint32
}

// originalDst reads the pre-REDIRECT destination of an intercepted
// connection. v6 selects the IPv6 socket option and sockaddr layout; the
// caller knows the family from the listener that accepted the connection.
func originalDst(conn *net.TCPConn, v6 bool) (string, error) {
	if v6 {
		return originalDst6(conn)
	}
	return originalDst4(conn)
}

func originalDst4(conn *net.TCPConn) (string, error) {
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

func originalDst6(conn *net.TCPConn) (string, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return "", err
	}
	var dst rawSockaddrInet6
	size := uint32(unsafe.Sizeof(dst))
	var sockErr syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		_, _, sockErr = syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(solIPv6),
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
	if dst.Family != syscall.AF_INET6 {
		return "", fmt.Errorf("unexpected original destination family %d", dst.Family)
	}
	ip := make(net.IP, 16)
	copy(ip, dst.Addr[:])
	port := int(binary.BigEndian.Uint16(dst.Port[:]))
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}
