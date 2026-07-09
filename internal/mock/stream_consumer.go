package mock

// stream_consumer.go is a faithful TeslaMate TeslaApi.Stream test double: it
// dials the streaming sink's WebSocket, sends the data:subscribe_oauth handshake
// for a tag (VIN or integer vehicle_id, matching TESLA_WSS_USE_VIN), and reads
// data:update / data:error / control:hello frames. The sink integration test
// uses it to assert a connected consumer receives correctly-shaped frames when
// the cache changes. It uses gws (the same WS library the sink uses) so the test
// exercises the real wire path.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/lxzan/gws"
)

// StreamConsumer is a TeslaMate-style streaming consumer for tests.
type StreamConsumer struct {
	url    string
	tag    string
	mu     sync.Mutex
	frames []string
	done   chan struct{}
	conn   *gws.Conn
}

// consumerHandler adapts received frames into the StreamConsumer's buffer.
type consumerHandler struct {
	sc *StreamConsumer
}

func (h *consumerHandler) OnOpen(socket *gws.Conn) { h.sc.conn = socket }
func (h *consumerHandler) OnMessage(socket *gws.Conn, msg *gws.Message) {
	h.sc.mu.Lock()
	h.sc.frames = append(h.sc.frames, msg.Data.String())
	h.sc.mu.Unlock()
}
func (h *consumerHandler) OnClose(*gws.Conn, error) {
	select {
	case <-h.sc.done:
	default:
		close(h.sc.done)
	}
}
func (h *consumerHandler) OnPing(*gws.Conn, []byte) {}
func (h *consumerHandler) OnPong(*gws.Conn, []byte) {}

// NewStreamConsumer builds a consumer that will dial url and subscribe to tag.
func NewStreamConsumer(url, tag string) *StreamConsumer {
	return &StreamConsumer{url: url, tag: tag, done: make(chan struct{})}
}

// Connect dials the sink and sends the data:subscribe_oauth handshake. It starts
// a background read loop; call Close to stop. The handshake matches TeslaMate's
// stream.ex shape.
func (c *StreamConsumer) Connect(ctx context.Context) error {
	h := &consumerHandler{sc: c}
	conn, _, err := gws.NewClient(h, &gws.ClientOption{
		Addr: c.url,
	})
	if err != nil {
		return err
	}
	c.conn = conn
	go conn.ReadLoop()
	// Wait for OnOpen, then send the handshake.
	handshake := `{"msg_type":"data:subscribe_oauth","token":"test","value":"speed,odometer,soc,elevation,est_heading,est_lat,est_lng,power,shift_state,range,est_range,heading","tag":"` + c.tag + `"}`
	if err := conn.WriteMessage(gws.OpcodeText, []byte(handshake)); err != nil {
		_ = conn.WriteClose(1000, nil)
		return err
	}
	return nil
}

// Frames returns a copy of the frames received so far.
func (c *StreamConsumer) Frames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.frames))
	copy(out, c.frames)
	return out
}

// WaitForFrame waits until at least one frame containing substr arrives or the
// timeout elapses. Returns the matching frame and whether it was found.
func (c *StreamConsumer) WaitForFrame(substr string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, f := range c.Frames() {
			if strings.Contains(f, substr) {
				return f, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", false
}

// Close tears down the connection.
func (c *StreamConsumer) Close() {
	if c.conn != nil {
		_ = c.conn.WriteClose(1000, nil)
	}
}
