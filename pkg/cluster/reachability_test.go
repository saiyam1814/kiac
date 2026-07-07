package cluster

import (
	"net"
	"testing"
	"time"
)

func TestHostCanDialOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// Accept and immediately close so dials complete.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	if !hostCanDial(ln.Addr().String(), time.Second) {
		t.Fatalf("expected open port %s to be dialable", ln.Addr())
	}
}

func TestHostCanDialClosedPort(t *testing.T) {
	// Bind then close to obtain a port that is (almost certainly) free,
	// so the dial fails fast the way an unreachable node VM would.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	start := time.Now()
	if hostCanDial(addr, 300*time.Millisecond) {
		t.Fatalf("expected closed port %s to be undialable", addr)
	}
	// It must give up around the timeout, not hang.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("hostCanDial took %s, expected it to bail near the 300ms budget", elapsed)
	}
}
