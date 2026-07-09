package v1

// sink.go is the data:update streaming sink: it owns its own HTTP listener
// (consumer-facing wss://, separate from the internal REST sink), upgrades
// /streaming/{token} to a WebSocket, runs the TeslaMate data:subscribe_oauth
// handshake, and per connected consumer broadcasts data:update frames read from
// the cache on change. It registers under BOTH stream-family names so it pairs
// with whichever streaming source is configured. The encode lives in
// stream_encode.go; the gws transport glue in internal/transport/wssutil.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink/idmap"
	"github.com/bttnns/red-switchboard/internal/plugin/streamsink"
	"github.com/bttnns/red-switchboard/internal/transport/wssutil"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/go-chi/chi/v5"
	"github.com/lxzan/gws"
	"gopkg.in/yaml.v3"
)

// SinkPluginName is the registry key shared by both stream-family names: the
// data:update sink is one implementation serving either source family.
const SinkPluginName = "tesla-fleet-stream-v1"

// init registers the one data:update sink under BOTH stream-family keys, so it
// pairs with whichever streaming source is configured (Fleet Telemetry or Owner).
func init() {
	streamsink.Register(SinkPluginName, newSink)
	streamsink.Register("tesla-owner-stream-v1", newSink)
}

// Settings is the stream.sinks.<name> config sub-block. Defaults are applied by
// applySinkDefaults (plain yaml.v3 decode does not honor `default:` tags), so the
// zero value of each field below is its "unset" sentinel.
type Settings struct {
	// ListenAddr is the consumer-facing streaming sink address (its own listener,
	// separate from the internal REST sink). TeslaMate dials wss:// here.
	ListenAddr string `yaml:"listen_addr"`
	// TLS configures direct wss://. Omit to serve ws:// behind a TLS-terminating
	// reverse proxy.
	TLS *TLSConfig `yaml:"tls"`
	// MaxPushHz caps the per-consumer push rate (cache changes are coalesced to
	// at most this many frames/sec). 0 disables throttling.
	MaxPushHz float64 `yaml:"max_push_hz"`
	// MaxConsumers bounds concurrent consumer connections; beyond it a new
	// handshake gets data:error + close. A guard against a runaway consumer.
	MaxConsumers int `yaml:"max_consumers"`
	// IDMapFile persists the canonical-id -> synthetic int64 id mapping so an
	// integer vehicle_id tag resolves to the same canonical id the REST sink
	// serves. Empty = in-memory (still deterministic via FNV hash).
	IDMapFile string `yaml:"idmap_file"`
	// SendBuf is the per-connection send-buffer size; a slow consumer that fills
	// it is dropped (drop-or-close).
	SendBuf int `yaml:"send_buf"`
}

// TLSConfig for the streaming sink's own listener.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// streamSink implements streamsink.StreamSink.
type streamSink struct {
	name     string
	settings Settings
	watcher  streamsink.CacheWatcher
	logger   *log.Logger
	resolve  tagResolver
	upgrader *gws.Upgrader

	// ctx is the sink's run context; per-connection broadcasters derive from it
	// so they stop when the sink stops. Set by Run; Background for Handler-only
	// (test) use before Run.
	ctx           context.Context
	consumerCount atomic.Int64
	framesPushed  atomic.Int64
	framesDropped atomic.Int64
}

// tagResolver maps a consumer-supplied tag (integer vehicle_id or VIN) to the
// canonical id. Integer tags resolve through the idmap (deterministic FNV hash,
// so an in-memory idmap matches the REST sink's); VIN tags resolve directly.
type tagResolver struct {
	byInt64 map[int64]string
	byVIN   map[string]string
}

func (r tagResolver) Resolve(tag string) (string, bool) {
	if n, err := strconv.ParseInt(tag, 10, 64); err == nil {
		id, ok := r.byInt64[n]
		return id, ok
	}
	id, ok := r.byVIN[tag]
	return id, ok
}

// buildResolver populates the int64 and VIN lookups from the served identities
// and the idmap. The idmap is reused (not the REST sink's instance) because the
// FNV hash is deterministic: an in-memory idmap here produces the same int64<->
// GUID mapping the REST sink's idmap does, so an integer tag resolves correctly
// without sharing state.
func buildResolver(identities []vehicle.Identity, idmapFile string) (tagResolver, error) {
	m, err := idmap.New(idmapFile)
	if err != nil {
		return tagResolver{}, err
	}
	r := tagResolver{
		byInt64: make(map[int64]string, len(identities)),
		byVIN:   make(map[string]string, len(identities)),
	}
	for _, id := range identities {
		n, err := m.ID(id.ID)
		if err != nil {
			return tagResolver{}, err
		}
		r.byInt64[n] = id.ID
		if id.VIN != "" {
			r.byVIN[id.VIN] = id.ID
		}
	}
	return r, nil
}

func newSink(node *yaml.Node, w streamsink.CacheWatcher, logger *log.Logger) (streamsink.StreamSink, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s Settings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, errors.New("tesla stream: decode settings: " + err.Error())
		}
	}
	applySinkDefaults(&s)
	resolve, err := buildResolver(w.Vehicles(), s.IDMapFile)
	if err != nil {
		return nil, err
	}
	sink := &streamSink{
		name:     SinkPluginName,
		settings: s,
		watcher:  w,
		logger:   logger,
		resolve:  resolve,
		ctx:      context.Background(),
	}
	sink.upgrader = wssutil.NewUpgrader(sink)
	return sink, nil
}

func applySinkDefaults(s *Settings) {
	if s.ListenAddr == "" {
		s.ListenAddr = ":4001"
	}
	if s.MaxPushHz == 0 {
		s.MaxPushHz = 1.0
	}
	if s.MaxConsumers == 0 {
		s.MaxConsumers = 16
	}
	if s.SendBuf == 0 {
		s.SendBuf = 64
	}
}

func (s *streamSink) Name() string { return s.name }

// minPushInterval is the per-consumer coalesce window from MaxPushHz.
func (s *streamSink) minPushInterval() time.Duration {
	if s.settings.MaxPushHz <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / s.settings.MaxPushHz)
}

// connState is the per-connection broadcaster state, stored in the gws session.
type connState struct {
	sender   *wssutil.Sender
	cancel   context.CancelFunc
	lastPush time.Time
}

const stateKey = "state"

func loadState(socket *gws.Conn) *connState {
	v, _ := socket.Session().Load(stateKey)
	st, _ := v.(*connState)
	return st
}

// --- gws Event handlers (the per-connection lifecycle) ---

func (s *streamSink) OnOpen(socket *gws.Conn) {
	// Reserve a slot atomically (increment-then-check) so concurrent opens cannot
	// both slip past the bound. OnClose only decrements when a connState was stored,
	// so the reject path releases its own reservation here.
	if s.consumerCount.Add(1) > int64(s.settings.MaxConsumers) {
		s.consumerCount.Add(-1)
		_ = socket.WriteMessage(gws.OpcodeText, []byte(EncodeError("", "server_error", "too many consumers")))
		_ = socket.WriteClose(1013, nil)
		return
	}
	st := &connState{
		sender: wssutil.NewSender(socket, s.settings.SendBuf),
	}
	socket.Session().Store(stateKey, st)
}

func (s *streamSink) OnClose(socket *gws.Conn, _ error) {
	st := loadState(socket)
	if st == nil {
		return
	}
	if st.cancel != nil {
		st.cancel()
	}
	st.sender.Close()
	s.consumerCount.Add(-1)
}

func (s *streamSink) OnMessage(socket *gws.Conn, msg *gws.Message) {
	st := loadState(socket)
	if st == nil {
		return
	}
	tag, ok := parseSubscribe(msg.Data.String())
	if !ok {
		_ = socket.WriteMessage(gws.OpcodeText, []byte(EncodeError("", "client_error", "expected data:subscribe_oauth or data:subscribe_all")))
		_ = socket.WriteClose(1008, nil)
		return
	}
	canonID, ok := s.resolve.Resolve(tag)
	if !ok {
		_ = socket.WriteMessage(gws.OpcodeText, []byte(EncodeError(tag, "vehicle_error", "unknown vehicle")))
		_ = socket.WriteClose(1008, nil)
		return
	}
	// Acknowledge the subscribe; the keepalive ticker also sends hello periodically.
	_ = st.sender.Send([]byte(EncodeHello(int(helloInterval.Milliseconds()))))
	// A re-subscribe on the same connection replaces the broadcaster: cancel the
	// previous one so it does not leak (and double-push).
	if st.cancel != nil {
		st.cancel()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	st.cancel = cancel
	go s.broadcast(ctx, st, canonID, tag)
}

// OnPing / OnPong: gws auto-pongs; nothing to do.
func (s *streamSink) OnPing(socket *gws.Conn, payload []byte) {}
func (s *streamSink) OnPong(socket *gws.Conn, payload []byte) {}

// broadcast is the per-consumer push loop. It subscribes to cache changes for
// canonID and sends a full data:update frame on change, coalesced to MaxPushHz.
// control:hello keepalives fire every 10s. Returns when ctx is cancelled (sink
// stop or connection close).
func (s *streamSink) broadcast(ctx context.Context, st *connState, canonID, tag string) {
	// Replay first (J2): re-emit the merged snapshots buffered during a TRS or
	// consumer restart, oldest first, so the reconnecting consumer's drive history is
	// not lost to the gap before the first live frame.
	if r, ok := s.watcher.(streamsink.CacheReplayer); ok {
		for _, snap := range r.Replay(canonID) {
			if !s.pushSnap(st, tag, snap) {
				return
			}
		}
	}
	events := s.watcher.Subscribe(ctx, canonID)
	helloTick := time.NewTicker(helloInterval)
	defer helloTick.Stop()
	minInt := s.minPushInterval()
	var coalesce *time.Ticker
	var coalesceC <-chan time.Time
	if minInt > 0 {
		coalesce = time.NewTicker(minInt)
		coalesceC = coalesce.C
		defer coalesce.Stop()
	}
	var pending bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-helloTick.C:
			st.sender.Send([]byte(EncodeHello(int(helloInterval.Milliseconds()))))
		case _, ok := <-events:
			if !ok {
				return
			}
			if minInt > 0 && time.Since(st.lastPush) < minInt {
				pending = true // coalesce: the next tick sends the latest
				continue
			}
			if !s.push(st, canonID, tag) {
				return
			}
			pending = false
		case <-coalesceC:
			if pending && time.Since(st.lastPush) >= minInt {
				if !s.push(st, canonID, tag) {
					return
				}
				pending = false
			}
		}
	}
}

// push encodes the latest snapshot into a full frame and sends it. Returns false
// if the consumer was dropped (send buffer full).
func (s *streamSink) push(st *connState, canonID, tag string) bool {
	return s.pushSnap(st, tag, s.watcher.Latest(canonID))
}

// pushSnap encodes one given snapshot into a full data:update frame and sends it.
// Every known column is emitted on every frame (the cache holds last-known values),
// so a drive boundary never lands on a frame missing odometer or location. The live
// push and the replay path share it so a replayed frame is encoded and counted
// identically to a live one. Returns false if the consumer was dropped.
func (s *streamSink) pushSnap(st *connState, tag string, snap vehicle.Snapshot) bool {
	ts, cols := dataColumnsFor(snap)
	if cols == nil {
		return true // no data yet; nothing to send, connection stays open
	}
	st.lastPush = time.Now()
	if st.sender.Send([]byte(encodeFrame(tag, ts, cols))) {
		s.framesPushed.Add(1)
		return true
	}
	s.framesDropped.Add(1)
	return false
}

// Stats returns the sink-side counters for the /metrics surface.
func (s *streamSink) Stats() (consumers, pushed, dropped int64) {
	return s.consumerCount.Load(), s.framesPushed.Load(), s.framesDropped.Load()
}

// subscribeMsg is the subset of the data:subscribe_oauth frame the sink reads.
type subscribeMsg struct {
	MsgType string `json:"msg_type"`
	Tag     string `json:"tag"`
}

// parseSubscribe extracts the tag from a data:subscribe_oauth or
// data:subscribe_all frame. Returns ("", false) for an unrecognized msg_type.
func parseSubscribe(frame string) (string, bool) {
	var m subscribeMsg
	if err := json.Unmarshal([]byte(frame), &m); err != nil {
		return "", false
	}
	switch m.MsgType {
	case "data:subscribe_oauth", "data:subscribe_all":
		return m.Tag, true
	}
	return "", false
}

// Run binds the sink's own listener, mounts the chi router, and serves until ctx
// is cancelled, then shuts the listener down. TLS is used when configured, else
// plain ws:// for a TLS-terminating reverse proxy.
func (s *streamSink) Run(ctx context.Context) error {
	s.ctx = ctx
	r := chi.NewRouter()
	s.mountStreaming(r)

	ln, err := net.Listen("tcp", s.settings.ListenAddr)
	if err != nil {
		return errors.New("tesla stream: listen: " + err.Error())
	}
	srv := &http.Server{Handler: r}
	errCh := make(chan error, 1)
	go func() {
		if s.settings.TLS != nil {
			errCh <- srv.ServeTLS(ln, s.settings.TLS.CertFile, s.settings.TLS.KeyFile)
		} else {
			errCh <- srv.Serve(ln)
		}
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleStreaming upgrades /streaming/{token} to a WebSocket. The {token} path
// segment is cosmetic (the real Tesla endpoint uses it for routing; this sink is
// a cache, not Tesla, so the token is not validated, mirroring the REST sink's
// qts- token handshake).
func (s *streamSink) handleStreaming(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r)
	if err != nil {
		return
	}
	// ReadLoop blocks until the connection closes; it drives OnMessage/OnClose.
	// Run it on the request goroutine; gws dispatches events from it.
	conn.ReadLoop()
}

// Handler returns the chi router for tests (httptest).
func (s *streamSink) Handler() (http.Handler, error) {
	r := chi.NewRouter()
	s.mountStreaming(r)
	return r, nil
}

// mountStreaming registers the /streaming routes. The {token} path segment is
// cosmetic (the real Tesla endpoint uses it for routing; this sink resolves a
// consumer by the VIN tag in the data:subscribe_oauth handshake). TeslaMate dials
// /streaming/ (empty token) when the TOKEN env is unset, and /streaming/<token>
// otherwise, so all three shapes route to the same upgrade handler.
func (s *streamSink) mountStreaming(r chi.Router) {
	r.Get("/streaming/{token}", s.handleStreaming)
	r.Get("/streaming/", s.handleStreaming)
	r.Get("/streaming", s.handleStreaming)
}
