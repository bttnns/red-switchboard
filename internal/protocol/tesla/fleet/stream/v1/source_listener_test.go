package v1

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"testing"
	"time"
)

// dialFrom opens a TCP connection to addr from a specific loopback source IP so
// the per-IP cap can be exercised with distinct peers (127.0.0.1 vs 127.0.0.2,
// both in the loopback /8). It returns the client conn; closing it frees the
// listener reservation on the server side.
func dialFrom(t *testing.T, srcIP, addr string) net.Conn {
	t.Helper()
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(srcIP)}, Timeout: time.Second}
	c, err := d.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial from %s: %v", srcIP, err)
	}
	return c
}

// accepted reports whether the server kept the connection. A rejected conn is
// closed by the listener, so the client read returns EOF; an accepted conn has
// no data and the read blocks to the deadline (a timeout, treated as accepted).
func accepted(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var buf [1]byte
	_, err := c.Read(buf[:])
	_ = c.SetReadDeadline(time.Time{})
	return errors.Is(err, os.ErrDeadlineExceeded)
}

func TestCappedListener_GlobalAndPerIP(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newCappedListener(base, 5, 3, log.New(io.Discard, "", 0))
	cl := ln.(*cappedListener)
	defer func() { _ = ln.Close() }()

	// Drain accepts on the server side so reservations are taken and tracked.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the accepted conn open; a real server would serve it. Closing on
			// listener shutdown happens via the deferred ln.Close on the base.
			go func(c net.Conn) {
				var b [1]byte
				_, _ = c.Read(b[:]) // blocks until peer closes, then releases
				_ = c.Close()
			}(c)
		}
	}()

	addr := base.Addr().String()

	// Per-IP cap is 3: from 127.0.0.1, three connect, the fourth is rejected.
	var ip1 []net.Conn
	for i := 0; i < 3; i++ {
		ip1 = append(ip1, dialFrom(t, "127.0.0.1", addr))
	}
	waitActive(t, cl, 3)
	if got := perIPCount(cl, "127.0.0.1"); got != 3 {
		t.Fatalf("per-IP active for 127.0.0.1: want 3, got %d", got)
	}

	// A 4th from the same IP must be rejected (server closes it): active stays 3.
	c4 := dialFrom(t, "127.0.0.1", addr)
	if accepted(c4) {
		t.Error("4th connection from 127.0.0.1 should be rejected by the per-IP cap")
	}
	_ = c4.Close()
	waitActive(t, cl, 3)

	// A different IP is still accepted (per-IP is per peer, not global).
	other := dialFrom(t, "127.0.0.2", addr)
	waitActive(t, cl, 4)
	if perIPCount(cl, "127.0.0.2") != 1 {
		t.Errorf("per-IP active for 127.0.0.2: want 1, got %d", perIPCount(cl, "127.0.0.2"))
	}

	// Global cap is 5: one more (a third IP) reaches it; the next is rejected.
	fifth := dialFrom(t, "127.0.0.3", addr)
	waitActive(t, cl, 5)
	sixth := dialFrom(t, "127.0.0.4", addr)
	if accepted(sixth) {
		t.Error("6th connection should be rejected by the global cap")
	}
	_ = sixth.Close()
	waitActive(t, cl, 5)

	// Close everything: active must return to zero (no leak).
	for _, c := range ip1 {
		_ = c.Close()
	}
	_ = other.Close()
	_ = fifth.Close()
	waitActive(t, cl, 0)
	if got := perIPCount(cl, "127.0.0.1"); got != 0 {
		t.Errorf("per-IP map not cleaned up: 127.0.0.1 still has %d", got)
	}
}

func waitActive(t *testing.T, cl *cappedListener, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cl.mu.Lock()
		got := cl.active
		cl.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cl.mu.Lock()
	got := cl.active
	cl.mu.Unlock()
	t.Fatalf("active connections: want %d, got %d", want, got)
}

func perIPCount(cl *cappedListener, ip string) int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.byIP[ip]
}
