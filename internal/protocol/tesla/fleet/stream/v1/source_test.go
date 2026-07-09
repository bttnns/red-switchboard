package v1

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/lxzan/gws"
	"github.com/teslamotors/fleet-telemetry/protos"
	"gopkg.in/yaml.v3"
)

// syncBuffer is a race-safe io.Writer for capturing a *log.Logger's output: the
// source logs from its HTTP handler goroutine while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSink is a streamsource.RecordSink that records the last snapshot per id
// and signals its arrival, so the integration test can assert the cache received
// a merged frame from a pushed protobuf Payload.
type captureSink struct {
	got  map[string]vehicle.Snapshot
	done chan string
}

func newCaptureSink(known ...string) *captureSink {
	return &captureSink{got: map[string]vehicle.Snapshot{}, done: make(chan string, 16)}
}
func (c *captureSink) Put(_ context.Context, id string, snap vehicle.Snapshot) error {
	c.got[id] = snap
	select {
	case c.done <- id:
	default:
	}
	return nil
}
func (c *captureSink) KnownIDs() map[string]bool { return map[string]bool{testVIN: true} }

const testVIN = "5YJSA11111111111"

// genMTLSCerts builds a CA (CN "TeslaMotors" so messages.CreateIdentityFromCert
// accepts it), a server cert signed by it, and a client cert (CN = the VIN)
// signed by it. Returns PEM bytes and writes server cert/key + client CA to temp
// files the source can load.
func genMTLSCerts(t *testing.T) (serverCertFile, serverKeyFile, clientCAFile string, clientCertPEM, clientKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "TeslaMotors"}, // in fleet-telemetry's knownIssuers map
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	mkLeaf := func(cn string, isClient bool) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if isClient {
			tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tpl.DNSNames = []string{"localhost", "127.0.0.1"}
			tpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}

	serverCert, serverKey := mkLeaf("localhost", false)
	clientCert, clientKey := mkLeaf(testVIN, true)

	dir := t.TempDir()
	writePEM := func(name string, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		_ = pem.Encode(f, &pem.Block{Type: typ, Bytes: der})
		return p
	}
	serverCertFile = writePEM("server.crt", "CERTIFICATE", serverCert.Raw)
	serverKeyDER, _ := x509.MarshalECPrivateKey(serverKey)
	serverKeyFile = writePEM("server.key", "EC PRIVATE KEY", serverKeyDER)
	clientCAFile = writePEM("ca.crt", "CERTIFICATE", caDER)

	clientCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCert.Raw})
	clientKeyDER, _ := x509.MarshalECPrivateKey(clientKey)
	clientKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER})
	return
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// TestSourceIntegration_PushesFrameToCache: a fake vehicle dials the source's mTLS
// listener, pushes one protobuf Payload, and the source decodes + delivers it to
// the RecordSink. This exercises the real mTLS accept, gws upgrade, telemetry
// deserialize, protobuf decode, and canonical map end to end.
func TestSourceIntegration_PushesFrameToCache(t *testing.T) {
	serverCertFile, serverKeyFile, clientCAFile, clientCertPEM, clientKeyPEM := genMTLSCerts(t)
	port := freePort(t)

	settings := SourceSettings{
		Host:       "127.0.0.1",
		Port:       port,
		Path:       "/",
		ServerCert: serverCertFile,
		ServerKey:  serverKeyFile,
		ClientCA:   clientCAFile,
	}
	node := mustYAML(t, settings)
	logs := &syncBuffer{}
	src, err := newSource(node, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	sink := newCaptureSink(testVIN)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, sink, nil) }()

	// Give the listener a moment to bind.
	time.Sleep(100 * time.Millisecond)

	payload := &protos.Payload{
		Data: []*protos.Datum{
			{Key: protos.Field_VehicleSpeed, Value: &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: 60}}},
			{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 72}}},
			{Key: protos.Field_Gear, Value: &protos.Value{Value: &protos.Value_ShiftStateValue{ShiftStateValue: protos.ShiftState_ShiftStateD}}},
			{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: 37.4, Longitude: -122.1}}}},
		},
	}
	car, err := mock.NewStreamVehicle("wss://127.0.0.1:"+strconv.Itoa(port), "txid-1", payload, clientCertPEM, clientKeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := car.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer car.Close()

	select {
	case <-sink.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for the cache to receive the pushed snapshot")
	}

	snap, ok := sink.got[testVIN]
	if !ok {
		t.Fatalf("no snapshot delivered for %s", testVIN)
	}
	st := snap.State
	if st == nil {
		t.Fatal("nil state")
	}
	if st.BatteryLevelPct != 72 {
		t.Errorf("BatteryLevelPct: want 72, got %g", st.BatteryLevelPct)
	}
	if st.Gear != vehicle.GearDrive {
		t.Errorf("Gear: want Drive, got %v", st.Gear)
	}
	if st.Location == nil || st.Location.Latitude != 37.4 {
		t.Errorf("Location: want 37.4, got %+v", st.Location)
	}
	// The source's stats reflect the decoded frame.
	if s := src.(*fleetTelemetrySource).Stats(); s.Connected != 1 || s.Frames != 1 {
		t.Errorf("Stats: want connected=1 frames=1, got connected=%d frames=%d", s.Connected, s.Frames)
	}
	// The accepted connection logs the peer RemoteAddr and VIN so connects are
	// attributable on the security surface.
	if s := logs.String(); !strings.Contains(s, "vehicle connected from 127.0.0.1") || !strings.Contains(s, "vin="+testVIN) {
		t.Errorf("expected a connect log with RemoteAddr and VIN, got:\n%s", s)
	}
}

// TestSourceAcceptsClientCertWithoutClientCA: with no client_ca configured (the
// default, since Tesla publishes no vehicle CA), the source still requires a client
// cert but authenticates the vehicle by IDENTITY (issuer allow-list + VIN), matching
// Tesla's reference server. The same mock vehicle pushes a frame and it lands.
func TestSourceAcceptsClientCertWithoutClientCA(t *testing.T) {
	serverCertFile, serverKeyFile, _, clientCertPEM, clientKeyPEM := genMTLSCerts(t)
	port := freePort(t)

	settings := SourceSettings{
		Host:       "127.0.0.1",
		Port:       port,
		Path:       "/",
		ServerCert: serverCertFile,
		ServerKey:  serverKeyFile,
		// ClientCA intentionally empty: identity-only client auth.
	}
	src, err := newSource(mustYAML(t, settings), nil)
	if err != nil {
		t.Fatal(err)
	}

	sink := newCaptureSink(testVIN)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, sink, nil) }()
	time.Sleep(100 * time.Millisecond)

	payload := &protos.Payload{Data: []*protos.Datum{
		{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 64}}},
	}}
	car, err := mock.NewStreamVehicle("wss://127.0.0.1:"+strconv.Itoa(port), "txid-noca", payload, clientCertPEM, clientKeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := car.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer car.Close()

	select {
	case <-sink.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: frame not delivered on the identity-only (no client_ca) path")
	}
	if snap, ok := sink.got[testVIN]; !ok || snap.State == nil || snap.State.BatteryLevelPct != 64 {
		t.Fatalf("expected SOC 64 from identity-only path, got %+v", sink.got[testVIN].State)
	}
}

// TestSourceRejectsUnknownVIN: a vehicle whose VIN is not in the cache's known set
// is denied (deny-by-default): the source decodes and acks but never calls Put.
func TestSourceRejectsUnknownVIN(t *testing.T) {
	serverCertFile, serverKeyFile, clientCAFile, clientCertPEM, clientKeyPEM := genMTLSCerts(t)
	port := freePort(t)

	settings := SourceSettings{
		Host: "127.0.0.1", Port: port, Path: "/",
		ServerCert: serverCertFile, ServerKey: serverKeyFile, ClientCA: clientCAFile,
	}
	node := mustYAML(t, settings)
	logs := &syncBuffer{}
	src, err := newSource(node, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	// known set does NOT include the vehicle's VIN.
	unknownSink := &denySink{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, unknownSink, nil) }()
	time.Sleep(100 * time.Millisecond)

	payload := &protos.Payload{Data: []*protos.Datum{
		{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 50}}},
	}}
	car, err := mock.NewStreamVehicle("wss://127.0.0.1:"+strconv.Itoa(port), "txid-2", payload, clientCertPEM, clientKeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := car.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer car.Close()

	time.Sleep(500 * time.Millisecond)
	if unknownSink.putCalled {
		t.Error("Put must NOT be called for a VIN not in the known set (deny-by-default)")
	}
	// The deny-by-default path increments the unknown_vin reject counter and logs
	// once (with RemoteAddr) so the rejection is attributable.
	if got := src.(*fleetTelemetrySource).Stats().Rejects["unknown_vin"]; got < 1 {
		t.Errorf("unknown_vin reject counter: want >=1, got %d", got)
	}
	if s := logs.String(); !strings.Contains(s, "unknown VIN") || !strings.Contains(s, "127.0.0.1") {
		t.Errorf("expected an unknown-VIN reject log with RemoteAddr, got:\n%s", s)
	}
}

// TestSourceRejectsMissingClientCert: a plain (no client cert) TLS dial is
// rejected at the handshake; the source never reaches the read loop.
func TestSourceRejectsMissingClientCert(t *testing.T) {
	serverCertFile, serverKeyFile, clientCAFile, _, _ := genMTLSCerts(t)
	port := freePort(t)
	settings := SourceSettings{
		Host: "127.0.0.1", Port: port, Path: "/",
		ServerCert: serverCertFile, ServerKey: serverKeyFile, ClientCA: clientCAFile,
	}
	node := mustYAML(t, settings)
	src, err := newSource(node, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, newCaptureSink(testVIN), nil) }()
	time.Sleep(100 * time.Millisecond)

	// Dial without a client cert: the TLS handshake must fail.
	h := &noOpHandler{}
	_, _, err = gws.NewClient(h, &gws.ClientOption{
		Addr:             "wss://127.0.0.1:" + strconv.Itoa(port),
		TlsConfig:        &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		HandshakeTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Error("dial without a client cert should fail the mTLS handshake")
	}
}

// TestSourceRejectsUntrustedIssuer: a client cert that completes the TLS handshake
// (identity-only mode, no client_ca) but is signed by an issuer NOT in Tesla's
// allow-list is rejected by identityFromClientCert; the "identity" reject counter
// increments and the rejection is logged with the peer RemoteAddr.
func TestSourceRejectsUntrustedIssuer(t *testing.T) {
	serverCertFile, serverKeyFile, _, _, _ := genMTLSCerts(t)
	clientCertPEM, clientKeyPEM := genUntrustedClientCert(t)
	port := freePort(t)

	settings := SourceSettings{
		Host: "127.0.0.1", Port: port, Path: "/",
		ServerCert: serverCertFile, ServerKey: serverKeyFile,
		// ClientCA empty: identity-only mode, so the untrusted cert reaches the
		// issuer allow-list check rather than failing chain verification at TLS.
	}
	logs := &syncBuffer{}
	src, err := newSource(mustYAML(t, settings), log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, newCaptureSink(testVIN), nil) }()
	time.Sleep(100 * time.Millisecond)

	cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = gws.NewClient(&noOpHandler{}, &gws.ClientOption{
		Addr: "wss://127.0.0.1:" + strconv.Itoa(port),
		TlsConfig: &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			Certificates:       []tls.Certificate{cert},
		},
		HandshakeTimeout: 2 * time.Second,
	})

	time.Sleep(300 * time.Millisecond)
	if got := src.(*fleetTelemetrySource).Stats().Rejects["identity"]; got < 1 {
		t.Errorf("identity reject counter: want >=1, got %d", got)
	}
	if s := logs.String(); !strings.Contains(s, "rejected connection from 127.0.0.1") {
		t.Errorf("expected an identity-reject log with RemoteAddr, got:\n%s", s)
	}
}

// genUntrustedClientCert builds a client cert signed by a CA whose CN is NOT in
// fleet-telemetry's known-issuers allow-list, so identityFromClientCert rejects it.
func genUntrustedClientCert(t *testing.T) (clientCertPEM, clientKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "NotTesla"}, // not in knownIssuers
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: testVIN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	clientKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

// denySink admits no VINs; it records whether Put was ever called.
type denySink struct{ putCalled bool }

func (d *denySink) Put(context.Context, string, vehicle.Snapshot) error {
	d.putCalled = true
	return nil
}
func (d *denySink) KnownIDs() map[string]bool { return map[string]bool{"SOMEOTHERVIN": true} }

// noOpHandler is a gws.Event that ignores everything, for the no-cert dial test.
type noOpHandler struct{}

func (noOpHandler) OnOpen(*gws.Conn)                  {}
func (noOpHandler) OnClose(*gws.Conn, error)          {}
func (noOpHandler) OnMessage(*gws.Conn, *gws.Message) {}
func (noOpHandler) OnPing(*gws.Conn, []byte)          {}
func (noOpHandler) OnPong(*gws.Conn, []byte)          {}

func mustYAML(t *testing.T, s SourceSettings) *yaml.Node {
	t.Helper()
	out, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var n yaml.Node
	if err := yaml.Unmarshal(out, &n); err != nil {
		t.Fatal(err)
	}
	return &n
}
