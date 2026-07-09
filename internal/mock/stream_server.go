package mock

// stream_server.go is a mock Tesla Owner streaming SERVER stand-in: the inverse
// of stream_consumer.go. The Owner streaming source (an outbound dialer) dials
// this server in its integration test; the server accepts the
// data:subscribe_oauth handshake and lets the test push data:update frames to
// the connected source. It uses gws (the same WS library the source uses) so the
// test exercises the real wire path end to end. httptest gives it a real TCP
// listener on 127.0.0.1, so the source's dialer connects for real.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/lxzan/gws"
)

// StreamServer is a mock Owner streaming endpoint for tests.
type StreamServer struct {
	srv *httptest.Server

	mu         sync.Mutex
	handshakes []string // received data:subscribe_oauth frames
	conns      []*gws.Conn
}

// serverHandler is the gws Event handler for the mock server.
type serverHandler struct {
	s    *StreamServer
	conn *gws.Conn
}

func (h *serverHandler) OnOpen(socket *gws.Conn) { h.conn = socket; h.s.addConn(socket) }
func (h *serverHandler) OnMessage(socket *gws.Conn, msg *gws.Message) {
	h.s.addHandshake(msg.Data.String())
}
func (h *serverHandler) OnClose(socket *gws.Conn, _ error) { h.s.removeConn(socket) }
func (h *serverHandler) OnPing(*gws.Conn, []byte)          {}
func (h *serverHandler) OnPong(*gws.Conn, []byte)          {}

// NewStreamServer starts a mock Owner streaming server on a random local port.
// Close it with Close.
func NewStreamServer() *StreamServer {
	s := &StreamServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/streaming/", s.handle)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *StreamServer) handle(w http.ResponseWriter, r *http.Request) {
	upgrader := gws.NewUpgrader(&serverHandler{s: s}, &gws.ServerOption{
		ReadMaxPayloadSize: 1 << 20,
		HandshakeTimeout:   5 * time.Second,
	})
	conn, err := upgrader.Upgrade(w, r)
	if err != nil {
		return
	}
	conn.ReadLoop()
}

func (s *StreamServer) addConn(c *gws.Conn) {
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
}
func (s *StreamServer) removeConn(c *gws.Conn) {
	s.mu.Lock()
	for i, cc := range s.conns {
		if cc == c {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}
func (s *StreamServer) addHandshake(frame string) {
	s.mu.Lock()
	s.handshakes = append(s.handshakes, frame)
	s.mu.Unlock()
}

// URL returns the ws:// URL the source should dial (with a trailing slash so
// /streaming/<vin> appends cleanly).
func (s *StreamServer) URL() string {
	return "ws://" + s.srv.Listener.Addr().String() + "/"
}

// Handshakes returns the data:subscribe_oauth frames received from connected
// sources.
func (s *StreamServer) Handshakes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.handshakes))
	copy(out, s.handshakes)
	return out
}

// Push sends one frame to every connected source (a data:update frame the source
// will decode).
func (s *StreamServer) Push(frame string) {
	s.mu.Lock()
	conns := append([]*gws.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(gws.OpcodeText, []byte(frame))
	}
}

// CloseAll closes every connected source's WS (simulates the car sleeping /
// Tesla dropping the connection) so the source's read loop returns.
func (s *StreamServer) CloseAll() {
	s.mu.Lock()
	conns := append([]*gws.Conn(nil), s.conns...)
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteClose(1000, nil)
	}
}

// Close stops the mock server.
func (s *StreamServer) Close() { s.srv.Close() }

// SubscribeValue is the column list the real Owner endpoint expects, mirrored
// from the sink/source column order so tests build realistic frames.
var SubscribeValue = strings.Join([]string{
	"speed", "odometer", "soc", "elevation", "est_heading", "est_lat", "est_lng",
	"power", "shift_state", "range", "est_range", "heading",
}, ",")
