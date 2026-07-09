// Package v1 is the tesla-fleet-stream-v1 STREAMING SOURCE plugin: it serves an
// mTLS WebSocket listener in-process (gws) and feeds decoded Fleet Telemetry
// records into the cache via streamsource.RecordSink. No broker, no sidecar, no
// CGO: it imports only Tesla's broker-free telemetry + protos + messages packages
// for the binary deserialize, protobuf decode, ack frames, and cert identity, and
// owns the accept/read/ack loop itself. The decode (protos.Payload ->
// vehicle.Snapshot) lives in source_mapping.go and reuses internal/units.
//
// This is the one public-facing listener red-switchboard binds: vehicles dial in.
// It is locked down by mTLS (a client cert is always required) plus deny-by-default
// on the VIN. By default the cert is authenticated by IDENTITY (Tesla's issuer
// allow-list + the CN/VIN), since Tesla publishes no downloadable vehicle CA; an
// optional client_ca upgrades this to full RequireAndVerifyClientCert. Either way an
// unpaired or forged cert never reaches the read loop, and an unknown VIN (outside
// known_vins, or the cache's known-id set) never fills the cache.
package v1

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/streamsource"
	"github.com/bttnns/red-switchboard/internal/transport/wssutil"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/lxzan/gws"
	logrus "github.com/teslamotors/fleet-telemetry/logger"
	"github.com/teslamotors/fleet-telemetry/messages"
	"github.com/teslamotors/fleet-telemetry/telemetry"
	"gopkg.in/yaml.v3"
)

// SourcePluginName is the streamsource registry key.
const SourcePluginName = "tesla-fleet-stream-v1"

func init() { streamsource.Register(SourcePluginName, newSource) }

// SourceSettings is the stream.sources.tesla-fleet-stream-v1 config sub-block.
type SourceSettings struct {
	Host string `yaml:"host"` // public FQDN vehicles dial in to (default 0.0.0.0)
	Port int    `yaml:"port"` // default 443
	Path string `yaml:"path"` // WS path vehicles connect to (default "/")

	// mTLS: the server presents ServerCert/ServerKey. A client cert is always
	// required; ClientCA is OPTIONAL. Set it to chain-verify the vehicle cert
	// (RequireAndVerifyClientCert). Left empty (the common case, since Tesla
	// publishes no downloadable vehicle CA), the cert is authenticated by IDENTITY
	// only -- Tesla's issuer allow-list + the CN (VIN) via CreateIdentityFromCert,
	// matching Tesla's reference fleet-telemetry server -- with deny-by-default on
	// unknown VINs as the trust boundary.
	ServerCert string `yaml:"server_cert"`
	ServerKey  string `yaml:"server_key"`
	ClientCA   string `yaml:"client_ca"` // optional; empty = identity-only client auth

	// KnownVINs optionally restricts which VINs may fill the cache (deny-by-
	// default). Empty = rely on the cache's own known-id set (also deny-by-
	// default via cache.Service.Put dropping unknown ids).
	KnownVINs []string `yaml:"known_vins"`

	// IdleTimeout reaps a connection that has sent no frame for this long (a
	// healthy vehicle pushes well inside it). Default 60s.
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// MaxConns bounds total concurrent accepted connections. The listener is
	// public-facing and raw TLS reaches it (no L7 proxy can front it without
	// stripping the client cert), so a pre-auth TCP/TLS handshake flood is
	// otherwise unbounded; over the cap a fresh accept is closed immediately,
	// before the TLS handshake. Default 32 (one paired vehicle in production).
	MaxConns int `yaml:"max_conns"`
	// MaxConnsPerIP bounds concurrent connections from a single remote IP, so one
	// peer cannot consume the whole MaxConns budget. Default 4. <=0 disables it.
	MaxConnsPerIP int `yaml:"max_conns_per_ip"`
}

const (
	defaultPort          = 443
	defaultIdleTimeout   = 60 * time.Second
	defaultMaxConns      = 32
	defaultMaxConnsPerIP = 4
)

// dispatchTopics is the set of record TxTypes accepted off the wire. A topic
// present here makes telemetry.Deserialize skip its sender-id check (mTLS already
// authenticated the vehicle); the Producer slices are unused by the in-tree loop,
// which maps and acks directly. Only "V" (vehicle_data) carries a protos.Payload
// we map; "errors"/"alerts" are acked so the vehicle stops retransmitting.
var dispatchTopics = map[string][]telemetry.Producer{"V": nil, "errors": nil, "alerts": nil}

type fleetTelemetrySource struct {
	wssutil.SourceCounters // connected/frames/last-frame stats + Stats()

	settings SourceSettings
	logger   *log.Logger
	known    map[string]bool
	noop     *logrus.Logger

	upgrader *gws.Upgrader
}

func newSource(node *yaml.Node, logger *log.Logger) (streamsource.StreamSource, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SourceSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("fleet-telemetry: decode settings: %w", err)
		}
	}
	if s.Port == 0 {
		s.Port = defaultPort
	}
	if s.Host == "" {
		s.Host = "0.0.0.0"
	}
	if s.Path == "" {
		s.Path = "/"
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = defaultIdleTimeout
	}
	if s.MaxConns == 0 {
		s.MaxConns = defaultMaxConns
	}
	if s.MaxConnsPerIP == 0 {
		s.MaxConnsPerIP = defaultMaxConnsPerIP
	}
	known := make(map[string]bool, len(s.KnownVINs))
	for _, v := range s.KnownVINs {
		known[v] = true
	}
	noop, _ := logrus.NoOpLogger()
	src := &fleetTelemetrySource{settings: s, logger: logger, known: known, noop: noop}
	src.upgrader = wssutil.NewUpgrader(src)
	return src, nil
}

func (s *fleetTelemetrySource) Name() string { return SourcePluginName }

// Vehicles returns nil: Fleet Telemetry learns VINs as vehicles connect (the cert
// CN is the VIN). The poll loop / a co-configured REST source enumerates the
// account's vehicles for identity purposes; this source's job is to receive.
func (s *fleetTelemetrySource) Vehicles(context.Context) ([]vehicle.Identity, error) {
	return nil, nil
}

// Run binds the mTLS listener and serves the vehicle WS until ctx is cancelled.
// A client cert is always required. By default we authenticate the vehicle by
// IDENTITY (Tesla's issuer allow-list + the CN/VIN via CreateIdentityFromCert)
// rather than chain-verifying it: Tesla publishes no downloadable vehicle CA, and
// its reference fleet-telemetry server ships no client-CA either. Deny-by-default
// on unknown VINs (the cache's known-id set) is the trust boundary. If an operator
// does supply ClientCA, we upgrade to full RequireAndVerifyClientCert. watch is
// unused: Fleet Telemetry is inbound-push (the car dials in), never reads the cache.
func (s *fleetTelemetrySource) Run(ctx context.Context, sink streamsource.RecordSink, _ streamsource.CacheWatcher) error {
	tlsCfg := &tls.Config{
		ClientAuth: tls.RequireAnyClientCert,
		MinVersion: tls.VersionTLS12,
	}
	if s.settings.ClientCA != "" {
		clientCAs, err := loadCAs(s.settings.ClientCA)
		if err != nil {
			return fmt.Errorf("fleet-telemetry: client CA: %w", err)
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = clientCAs
	}
	mux := http.NewServeMux()
	mux.HandleFunc(s.settings.Path, s.handleVehicle(ctx, sink))
	httpSrv := &http.Server{
		Handler:   mux,
		TLSConfig: tlsCfg,
	}
	// Bound concurrent connections at the raw listener (before TLS) so a pre-auth
	// handshake flood cannot exhaust resources. The wrapper rejects an accept over
	// the global or per-IP cap by closing it immediately.
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.settings.Host, s.settings.Port))
	if err != nil {
		return fmt.Errorf("fleet-telemetry: listen: %w", err)
	}
	ln = newCappedListener(ln, s.settings.MaxConns, s.settings.MaxConnsPerIP, s.logger)
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	s.logger.Printf("fleet-telemetry: listening on %s:%d (mTLS, max_conns=%d per_ip=%d)",
		s.settings.Host, s.settings.Port, s.settings.MaxConns, s.settings.MaxConnsPerIP)
	if err := httpSrv.ServeTLS(ln, s.settings.ServerCert, s.settings.ServerKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("fleet-telemetry: %w", err)
	}
	return nil
}

// handleVehicle upgrades one verified mTLS connection to a WebSocket and runs the
// read/decode/ack loop (conn.ReadLoop drives the gws event handlers). The client
// cert is extracted BEFORE the upgrade so a bad cert is rejected with a 401
// rather than a WS close.
func (s *fleetTelemetrySource) handleVehicle(ctx context.Context, sink streamsource.RecordSink) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, err := identityFromClientCert(r)
		if err != nil {
			s.Reject("identity")
			s.logger.Printf("fleet-telemetry: rejected connection from %s (identity: %v)", r.RemoteAddr, err)
			http.Error(w, "client cert required", http.StatusUnauthorized)
			return
		}
		s.logger.Printf("fleet-telemetry: vehicle connected from %s vin=%s", r.RemoteAddr, identity.DeviceID)
		slog.Info("stream connect", "source", SourcePluginName, "vehicle", identity.DeviceID, "peer", r.RemoteAddr)
		conn, err := s.upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		st := &connState{
			source:     s,
			sink:       sink,
			ctx:        ctx,
			identity:   identity,
			serializer: telemetry.NewBinarySerializer(identity, dispatchTopics, s.noop),
		}
		conn.Session().Store(stateKey, st)
		conn.ReadLoop() // returns when the connection closes
	}
}

// connState is the per-connection state, stored in the gws session.
type connState struct {
	source       *fleetTelemetrySource
	sink         streamsource.RecordSink
	ctx          context.Context
	identity     *telemetry.RequestIdentity
	serializer   *telemetry.BinarySerializer
	rejectLogged bool // deny-by-default reject is logged once per connection, not per frame
}

const stateKey = "ftstate"

func loadConnState(socket *gws.Conn) *connState {
	v, _ := socket.Session().Load(stateKey)
	st, _ := v.(*connState)
	return st
}

// --- gws Event handlers (the per-connection lifecycle) ---

func (s *fleetTelemetrySource) OnOpen(socket *gws.Conn) {
	s.Connected()
	s.resetDeadline(socket)
}

func (s *fleetTelemetrySource) OnClose(socket *gws.Conn, err error) {
	s.Disconnected()
	st := loadConnState(socket)
	// A read-deadline timeout means resetDeadline reaped an idle connection (no frame
	// for IdleTimeout); anything else is the peer/transport closing.
	idle := isIdleTimeout(err)
	if idle {
		s.IdleTimeout()
	}
	event := "stream disconnect"
	if idle {
		event = "stream idle-timeout"
	}
	vin := ""
	if st != nil && st.identity != nil {
		vin = st.identity.DeviceID
	}
	slog.Info(event, "source", SourcePluginName, "vehicle", vin, "peer", socket.RemoteAddr().String())
	// Fire an immediate poll so the cache reflects the car's post-disconnect state
	// (parked, asleep) before TeslaMate's next query. Without this, a mid-drive
	// disconnect without a GearPark frame leaves stale driving state in the cache.
	if st == nil || st.identity == nil {
		return
	}
	if n, ok := st.sink.(streamsource.StreamDisconnectNotifier); ok {
		// DeviceID is the cert CN, which for Tesla is the VIN: the same id the cache
		// is keyed by (Put uses rec.Vin). An unknown id is a safe no-op downstream.
		n.OnStreamDisconnect(st.ctx, st.identity.DeviceID)
	}
}

func (s *fleetTelemetrySource) OnMessage(socket *gws.Conn, msg *gws.Message) {
	defer s.resetDeadline(socket)
	st := loadConnState(socket)
	if st == nil {
		return
	}
	data := msg.Data.Bytes()
	// transmitDecodedRecords=false keeps PayloadBytes as protobuf (the proto
	// decode + Vin/transform steps still run inside applyProtoRecordTransforms);
	// true would overwrite them with JSON and break proto.Unmarshal downstream.
	rec, err := telemetry.NewRecord(st.serializer, data, st.identity.DeviceID, false)
	if err != nil {
		if rec != nil {
			_ = socket.WriteMessage(gws.OpcodeBinary, rec.Error(err))
		}
		return
	}
	if rec.TxType == "V" {
		s.handleVehicleData(socket, st, rec)
	}
	// Reliable delivery: the vehicle retransmits until acked. Ack every accepted
	// record (mapped or not) so it stops retransmitting.
	_ = socket.WriteMessage(gws.OpcodeBinary, rec.Ack())
}

func (s *fleetTelemetrySource) OnPing(socket *gws.Conn, _ []byte) { s.resetDeadline(socket) }
func (s *fleetTelemetrySource) OnPong(socket *gws.Conn, _ []byte) { s.resetDeadline(socket) }

// handleVehicleData decodes a vehicle_data record into a canonical snapshot and
// delivers it to the cache. Deny-by-default: a VIN not in the known set (when a
// known set is configured) is skipped; the cache enforces the same again.
func (s *fleetTelemetrySource) handleVehicleData(socket *gws.Conn, st *connState, rec *telemetry.Record) {
	snap := DecodePayload(payloadOf(rec.Payload()))
	if snap.StreamPresent == 0 {
		return // keepalive: no streamed field, do not trip a spurious subscriber notify
	}
	if known := st.sink.KnownIDs(); len(known) > 0 && !known[rec.Vin] {
		s.Reject("unknown_vin")
		if !st.rejectLogged {
			st.rejectLogged = true // log once per connection, not per frame
			s.logger.Printf("fleet-telemetry: rejected frame from %s vin=%s (unknown VIN, deny-by-default)", socket.RemoteAddr(), rec.Vin)
		}
		return
	}
	s.Frame()
	s.RecordFields(snap.StreamPresent)
	_ = st.sink.Put(st.ctx, rec.Vin, snap)
}

// resetDeadline (re)arms the per-connection idle timeout. A connection that sends
// no frame (data, ping, or pong) for IdleTimeout is reaped: the next read returns
// a timeout and gws dispatches OnClose.
func (s *fleetTelemetrySource) resetDeadline(socket *gws.Conn) {
	_ = socket.SetReadDeadline(time.Now().Add(s.settings.IdleTimeout))
}

// isIdleTimeout reports whether a close error is the idle-reap read-deadline
// timeout (vs a peer/transport close), so OnClose can label the two apart.
func isIdleTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// identityFromClientCert reuses Tesla's messages.CreateIdentityFromCert (which
// validates the issuer against Tesla's known CA allow-list and normalizes the CN
// to the device id) and assembles the telemetry.RequestIdentity the serializer
// associates with the socket. It reads the leaf from PeerCertificates so it works
// in both modes: RequireAnyClientCert (identity-only, no client_ca) leaves
// VerifiedChains empty, but the issuer allow-list above is the auth check either way.
func identityFromClientCert(r *http.Request) (*telemetry.RequestIdentity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, errors.New("no client certificate")
	}
	clientType, deviceID, err := messages.CreateIdentityFromCert(r.TLS.PeerCertificates[0])
	if err != nil {
		return nil, err
	}
	return &telemetry.RequestIdentity{
		DeviceID:            deviceID,
		SenderID:            clientType + "." + deviceID,
		DeviceClientVersion: r.Header.Get("Version"),
	}, nil
}

// loadCAs reads the operator's client-CA bundle into a cert pool (the mTLS trust
// root for vehicle certs).
func loadCAs(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certs in %s", path)
	}
	return pool, nil
}
