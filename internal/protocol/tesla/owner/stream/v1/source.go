package v1

// source.go is the tesla-owner-stream-v1 STREAMING SOURCE plugin: the outbound
// WSS dialer. For each served vehicle, it watches the cache's derived Power
// state (the poll loop's no-wake summary) and dials Tesla's Owner streaming
// endpoint ONLY while the car is online, sending data:subscribe_oauth and
// decoding pushed data:update frames into canonical snapshots. It disconnects
// when the car sleeps (so an open connection cannot hold the car awake) and
// reconnects with escalating backoff, never a tight retry (the bug that made
// TeslaMate hammer the endpoint every 10s).
//
// This is the source shape the data:update sink imitates: same wire shape, same
// column order, so the encode (sink) and decode (source) are inverses. The
// decode lives in source_mapping.go. The reconnect backoff lives in
// internal/transport/wssutil/backoff.go.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/streamsource"
	teslaauth "github.com/bttnns/red-switchboard/internal/protocol/tesla/auth"
	"github.com/bttnns/red-switchboard/internal/transport/wssutil"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/lxzan/gws"
	"gopkg.in/yaml.v3"
)

// SourcePluginName is the streamsource registry key.
const SourcePluginName = "tesla-owner-stream-v1"

func init() { streamsource.Register(SourcePluginName, newSource) }

// SourceSettings is the stream.sources.tesla-owner-stream-v1 config sub-block.
type SourceSettings struct {
	// CredsFile is the Owner API creds file (same shape as the Owner/Fleet poll
	// source): its access_token is the data:subscribe_oauth token. Required.
	CredsFile string `yaml:"creds_file"`
	// StreamURL is the Owner streaming endpoint. Default Tesla's public host.
	StreamURL string `yaml:"stream_url"`
	// HandshakeTimeout caps the WS dial + upgrade.
	HandshakeTimeout time.Duration `yaml:"handshake_timeout"`
	// ReconnectInitial is the first reconnect delay after a failed/zero-frame
	// session. Default 1s.
	ReconnectInitial time.Duration `yaml:"reconnect_initial"`
	// ReconnectMax caps the escalating reconnect backoff. Default 5m.
	ReconnectMax time.Duration `yaml:"reconnect_max"`
}

const (
	defaultStreamURL = "wss://streaming.vn.teslamotors.com"
	defaultHandshake = 10 * time.Second
)

type ownerStreamSource struct {
	wssutil.SourceCounters // connected/frames/last-frame stats + Stats()

	settings SourceSettings
	mgr      *teslaauth.TokenManager
	logger   *log.Logger
}

func newSource(node *yaml.Node, logger *log.Logger) (streamsource.StreamSource, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SourceSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("tesla-owner-stream: decode settings: %w", err)
		}
	}
	if s.StreamURL == "" {
		s.StreamURL = defaultStreamURL
	}
	if s.HandshakeTimeout == 0 {
		s.HandshakeTimeout = defaultHandshake
	}
	mgr, err := teslaauth.Shared(s.CredsFile, logger)
	if err != nil {
		return nil, fmt.Errorf("tesla-owner-stream: %w", err)
	}
	return &ownerStreamSource{settings: s, mgr: mgr, logger: logger}, nil
}

func (s *ownerStreamSource) Name() string { return SourcePluginName }

// Vehicles returns nil: identity enumeration is the poll source's job (the
// active cfg.Source). This source reads the served identities from the cache
// watcher in Run.
func (s *ownerStreamSource) Vehicles(context.Context) ([]vehicle.Identity, error) {
	return nil, nil
}

// Run starts one supervisor per served vehicle. Each supervisor follows the
// cache's Power state (read through watch) and dials/disconnects the Owner stream
// accordingly. Blocks until ctx is cancelled; on cancellation it tears down every
// active dialer.
func (s *ownerStreamSource) Run(ctx context.Context, sink streamsource.RecordSink, watch streamsource.CacheWatcher) error {
	if watch == nil {
		return errors.New("tesla-owner-stream: cache watcher required to drive connect/disconnect state")
	}
	w := watch
	ids := w.Vehicles()
	if len(ids) == 0 {
		s.logger.Printf("tesla-owner-stream: no served vehicles; dialer idle")
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		if id.VIN == "" {
			continue // cannot dial without a VIN
		}
		wg.Add(1)
		go func(id vehicle.Identity) {
			defer wg.Done()
			s.supervise(ctx, w, sink, id)
		}(id)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// supervise follows one vehicle's Power state, starting a dialer while the car
// is online and stopping it when it sleeps/goes offline. The dialer is the only
// connection held per vehicle; cancelling its context closes the WS so ReadLoop
// returns and the dialer exits.
func (s *ownerStreamSource) supervise(ctx context.Context, w streamsource.CacheWatcher, sink streamsource.RecordSink, id vehicle.Identity) {
	events := w.Subscribe(ctx, id.ID)
	var (
		dctx    context.Context
		dcancel context.CancelFunc
		running bool
	)
	stop := func() {
		if running {
			dcancel()
			s.Disconnected()
			running = false
		}
	}
	start := func() {
		dctx, dcancel = context.WithCancel(ctx)
		running = true
		s.Connected()
		d := &dialer{src: s, id: id.ID, vin: id.VIN, backoff: wssutil.NewReconnectBackoff(s.settings.ReconnectInitial, s.settings.ReconnectMax)}
		go d.run(dctx, sink)
	}
	// Evaluate the current state immediately (the poll loop may already have it).
	s.eval(w, id.ID, &running, start, stop)
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case _, ok := <-events:
			if !ok {
				stop()
				return
			}
			s.eval(w, id.ID, &running, start, stop)
		}
	}
}

// eval starts/stops the dialer to match the cache's current Power state for id.
func (s *ownerStreamSource) eval(w streamsource.CacheWatcher, id string, running *bool, start, stop func()) {
	snap := w.Latest(id)
	online := snap.State != nil && snap.State.Power == vehicle.PowerOnline
	switch {
	case online && !*running:
		start()
	case !online && *running:
		stop()
	}
}

// dialer owns one outbound Owner streaming connection for one vehicle and its
// reconnect loop. It runs only while its ctx is live (the supervisor cancels it
// when the car sleeps).
type dialer struct {
	src     *ownerStreamSource
	id      string
	vin     string
	backoff *wssutil.ReconnectBackoff
}

// run dials, reads, and reconnects with escalating backoff until ctx is
// cancelled. A session that yielded at least one frame resets the backoff (a
// brief blip after a live stream reconnects promptly); a zero-frame session
// (dial failure or immediate close) escalates.
func (d *dialer) run(ctx context.Context, sink streamsource.RecordSink) {
	for {
		if ctx.Err() != nil {
			return
		}
		gotFrame := d.connect(ctx, sink)
		if ctx.Err() != nil {
			return
		}
		if gotFrame {
			d.backoff.Reset()
		}
		delay := d.backoff.NextDelay()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// connect dials the Owner streaming endpoint, sends data:subscribe_oauth, and
// runs the read loop until the connection closes. Returns whether at least one
// data:update frame was decoded (used to reset the backoff).
func (d *dialer) connect(ctx context.Context, sink streamsource.RecordSink) bool {
	h := &dialerHandler{d: d, sink: sink}
	url := strings.TrimSuffix(d.src.settings.StreamURL, "/") + "/streaming/" + d.vin
	conn, _, err := gws.NewClient(h, &gws.ClientOption{
		Addr:             url,
		HandshakeTimeout: d.src.settings.HandshakeTimeout,
	})
	if err != nil {
		return false
	}
	h.conn = conn
	// Cancellation closes the WS so ReadLoop returns promptly. The watcher also exits
	// when ReadLoop returns on its own (peer close), so it does not leak one goroutine
	// (and one captured closed conn) per reconnect for the whole online session.
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.WriteClose(1000, nil)
		case <-closed:
		}
	}()
	conn.ReadLoop() // blocks until close; drives OnOpen/OnMessage/OnClose
	close(closed)
	return h.gotFrame
}

// dialerHandler is the gws Event handler for one Owner streaming connection.
type dialerHandler struct {
	d        *dialer
	sink     streamsource.RecordSink
	conn     *gws.Conn
	gotFrame bool
}

func (h *dialerHandler) OnOpen(socket *gws.Conn) {
	// Send data:subscribe_oauth immediately. The token is read from the central
	// TokenManager at connect time, so an expiry-driven reconnect picks up a token
	// the manager has refreshed. The tag is the VIN (TESLA_WSS_USE_VIN shape); the
	// value is the column list the decode mirrors.
	token, _ := h.d.src.mgr.Token(context.Background())
	cols := strings.Join(ownerColumns, ",")
	hs := fmt.Sprintf(`{"msg_type":"data:subscribe_oauth","token":%q,"value":%q,"tag":%q}`, token, cols, h.d.vin)
	_ = socket.WriteMessage(gws.OpcodeText, []byte(hs))
}

func (h *dialerHandler) OnMessage(socket *gws.Conn, msg *gws.Message) {
	frame := msg.Data.String()
	_, snap, ok := DecodeDataUpdate(frame)
	if !ok {
		return // control:hello / data:error / non-data:update: ignore
	}
	if snap.StreamPresent == 0 {
		return // keepalive: no streamed field
	}
	h.gotFrame = true
	h.d.src.Frame()
	_ = h.sink.Put(context.Background(), h.d.id, snap)
}

// OnClose/OnPing/OnPong: the dialer reacts to closure via ReadLoop returning and
// gws auto-pongs, so nothing to do here.
func (h *dialerHandler) OnClose(*gws.Conn, error) {}
func (h *dialerHandler) OnPing(*gws.Conn, []byte) {}
func (h *dialerHandler) OnPong(*gws.Conn, []byte) {}
