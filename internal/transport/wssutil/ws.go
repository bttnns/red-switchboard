// Package wssutil holds the shared WebSocket transport helpers used by every
// streaming component (the data:update sink in Phase 1, the Fleet Telemetry
// listener in Phase 2, the Owner dialer in Phase 3), so the per-protocol plugins
// stay focused on their wire shape and not on gws plumbing. It wraps
// github.com/lxzan/gws.
//
// This package is transport-only: it imports gws and the stdlib, never any
// internal/protocol/* package. It owns no business logic.
package wssutil

import (
	"net/http"
	"sync"
	"time"

	"github.com/lxzan/gws"
)

// Defaults for the WS transport. writeDeadline bounds a blocked send so a
// half-open TCP connection is reaped rather than pinning a goroutine;
// readMaxPayload caps a hostile/buggy frame.
const (
	writeDeadline  = 10 * time.Second
	readMaxPayload = 1 << 20 // 1 MiB: a data:update frame is tiny; a telemetry frame is bounded
	defaultSendBuf = 64
)

// NewUpgrader builds a gws upgrader for a server-side WS with sane transport
// defaults. The caller supplies the Event handler (the per-protocol behavior);
// this helper only sets the cross-cutting transport options.
func NewUpgrader(handler gws.Event) *gws.Upgrader {
	return gws.NewUpgrader(handler, &gws.ServerOption{
		ReadMaxPayloadSize: readMaxPayload,
		HandshakeTimeout:   10 * time.Second,
	})
}

// Sender wraps a *gws.Conn with a bounded send buffer and a dedicated write
// goroutine. Callers write with Send (non-blocking): if the buffer is full the
// connection is closed (drop-or-close), so a slow consumer never blocks the cache
// writer or the broadcaster. Each write applies a deadline so a half-open peer is
// reaped. It is safe for concurrent use.
type Sender struct {
	conn      *gws.Conn
	send      chan []byte
	closeOnce sync.Once
	done      chan struct{}
}

// NewSender starts the write goroutine for conn. buf is the send-buffer size
// (<=0 falls back to defaultSendBuf). The caller must NOT call conn.WriteMessage
// directly after this; all writes go through Send.
func NewSender(conn *gws.Conn, buf int) *Sender {
	if buf <= 0 {
		buf = defaultSendBuf
	}
	s := &Sender{
		conn: conn,
		send: make(chan []byte, buf),
		done: make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

// Send enqueues a message. It returns false (and closes the connection) when the
// buffer is full, signalling the caller to tear down the subscriber. It also returns
// false once the write loop has exited (a prior write error closed done): without
// this check Send would keep enqueuing into a buffer no reader drains, silently
// dropping frames while telling the caller the send succeeded.
func (s *Sender) Send(msg []byte) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.send <- msg:
		return true
	default:
		s.Close()
		return false
	}
}

// Close shuts down the write goroutine and closes the underlying WS once.
func (s *Sender) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.WriteClose(1000, nil)
	})
}

func (s *Sender) writeLoop() {
	for {
		select {
		case <-s.done:
			return
		case msg := <-s.send:
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := s.conn.WriteMessage(gws.OpcodeText, msg); err != nil {
				s.Close()
				return
			}
		}
	}
}

// Upgrade is a convenience wrapping NewUpgrader + Upgrade for the common
// server-side case. It returns the *gws.Conn (whose ReadLoop the caller must run
// in a goroutine) and the Sender.
func Upgrade(handler gws.Event, w http.ResponseWriter, r *http.Request, buf int) (*gws.Conn, *Sender, error) {
	conn, err := NewUpgrader(handler).Upgrade(w, r)
	if err != nil {
		return nil, nil, err
	}
	return conn, NewSender(conn, buf), nil
}
