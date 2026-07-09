package wssutil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxzan/gws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSenderRoundTrip: a message sent through the bounded Sender arrives at a
// connected gws client, proving the upgrade + write-loop path. The sink test
// exercises the full handshake; this isolates the transport helper.
func TestSenderRoundTrip(t *testing.T) {
	got := make(chan string, 4)
	srvHandler := &loopbackHandler{} // records its socket so we can Send from it
	clientHandler := &clientHandler{onMsg: func(m string) { got <- m }}

	upgrader := NewUpgrader(srvHandler)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		c.ReadLoop()
	}))
	defer srv.Close()

	conn, _, err := gws.NewClient(clientHandler, &gws.ClientOption{
		Addr: "ws" + strings.TrimPrefix(srv.URL, "http") + "/",
	})
	require.NoError(t, err)
	go conn.ReadLoop()

	// Wait for the server OnOpen to record the socket, then send through a Sender.
	require.Eventually(t, func() bool {
		srvHandler.mu.Lock()
		defer srvHandler.mu.Unlock()
		return srvHandler.socket != nil
	}, time.Second, 5*time.Millisecond)

	sender := NewSender(srvHandler.socket, 4)
	require.True(t, sender.Send([]byte("hello")))

	select {
	case m := <-got:
		assert.Equal(t, "hello", m)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the message to arrive")
	}
	sender.Close()
}

type loopbackHandler struct {
	gws.BuiltinEventHandler
	mu     sync.Mutex
	socket *gws.Conn
}

func (h *loopbackHandler) OnOpen(socket *gws.Conn) {
	h.mu.Lock()
	h.socket = socket
	h.mu.Unlock()
}

type clientHandler struct {
	gws.BuiltinEventHandler
	onMsg func(string)
}

func (h *clientHandler) OnMessage(_ *gws.Conn, msg *gws.Message) {
	if h.onMsg != nil {
		h.onMsg(msg.Data.String())
	}
}
