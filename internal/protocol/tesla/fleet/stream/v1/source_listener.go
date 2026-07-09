package v1

import (
	"log"
	"net"
	"sync"
)

// cappedListener wraps a net.Listener and bounds concurrent connections by a
// global count and a per-remote-IP count. An accept over either cap is closed
// immediately (before the TLS handshake), so a pre-auth handshake flood on the
// public-facing listener cannot exhaust resources. A wrapped conn decrements its
// reservation on Close. perIP <= 0 disables the per-IP cap.
type cappedListener struct {
	net.Listener
	global int
	perIP  int
	logger *log.Logger

	mu     sync.Mutex
	active int
	byIP   map[string]int
}

func newCappedListener(ln net.Listener, global, perIP int, logger *log.Logger) net.Listener {
	return &cappedListener{
		Listener: ln,
		global:   global,
		perIP:    perIP,
		logger:   logger,
		byIP:     make(map[string]int),
	}
}

func (l *cappedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := remoteIP(c.RemoteAddr())
		if l.reserve(ip) {
			return &cappedConn{Conn: c, ln: l, ip: ip}, nil
		}
		// Over cap: close and keep accepting (a rejected flood must not spin or
		// stall the listener). Closing here frees the fd before the TLS handshake.
		l.logger.Printf("fleet-telemetry: connection from %s rejected (over cap)", ip)
		_ = c.Close()
	}
}

// reserve admits a connection if it is under both the global and per-IP caps.
func (l *cappedListener) reserve(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= l.global {
		return false
	}
	if l.perIP > 0 && l.byIP[ip] >= l.perIP {
		return false
	}
	l.active++
	l.byIP[ip]++
	return true
}

func (l *cappedListener) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.byIP[ip]--; l.byIP[ip] <= 0 {
		delete(l.byIP, ip)
	}
}

// cappedConn releases its listener reservation exactly once, on first Close.
type cappedConn struct {
	net.Conn
	ln   *cappedListener
	ip   string
	once sync.Once
}

func (c *cappedConn) Close() error {
	c.once.Do(func() { c.ln.release(c.ip) })
	return c.Conn.Close()
}

func remoteIP(addr net.Addr) string {
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}
