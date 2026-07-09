package v1

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/teslamotors/fleet-telemetry/protos"
)

// captureSlog redirects slog.Default to a race-safe text handler for the test and
// returns the buffer (the source logs from its handler goroutine).
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// waitFor polls the buffer until it contains want or the deadline passes.
func waitFor(buf *syncBuffer, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return strings.Contains(buf.String(), want)
}

// TestStreamDecisionTrail: a real mTLS dial produces a "stream connect" decision
// line with the vehicle and peer, and a clean close produces a "stream disconnect"
// line. These are the low-volume connect/disconnect seams (never per frame).
func TestStreamDecisionTrail(t *testing.T) {
	buf := captureSlog(t)
	serverCertFile, serverKeyFile, clientCAFile, clientCertPEM, clientKeyPEM := genMTLSCerts(t)
	port := freePort(t)

	src, err := newSource(mustYAML(t, SourceSettings{
		Host: "127.0.0.1", Port: port, Path: "/",
		ServerCert: serverCertFile, ServerKey: serverKeyFile, ClientCA: clientCAFile,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := newCaptureSink(testVIN)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, sink, nil) }()
	time.Sleep(100 * time.Millisecond)

	payload := &protos.Payload{Data: []*protos.Datum{
		{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 50}}},
	}}
	car, err := mock.NewStreamVehicle("wss://127.0.0.1:"+strconv.Itoa(port), "txid-dl", payload, clientCertPEM, clientKeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := car.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	if !waitFor(buf, "msg=\"stream connect\"", 3*time.Second) {
		t.Fatalf("missing stream connect line in:\n%s", buf.String())
	}
	out := buf.String()
	for _, want := range []string{"vehicle=" + testVIN, "peer=127.0.0.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("connect line missing %q in:\n%s", want, out)
		}
	}

	car.Close()
	if !waitFor(buf, "msg=\"stream disconnect\"", 3*time.Second) {
		t.Fatalf("missing stream disconnect line in:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "vehicle="+testVIN) {
		t.Errorf("disconnect line missing vehicle id in:\n%s", buf.String())
	}
}

// TestStreamIdleTimeout: a connection that sends nothing for IdleTimeout is reaped
// and logged as "stream idle-timeout" (distinct from a peer-initiated disconnect),
// so an operator can tell a silent car apart from a clean drop.
func TestStreamIdleTimeout(t *testing.T) {
	buf := captureSlog(t)
	serverCertFile, serverKeyFile, clientCAFile, clientCertPEM, clientKeyPEM := genMTLSCerts(t)
	port := freePort(t)

	src, err := newSource(mustYAML(t, SourceSettings{
		Host: "127.0.0.1", Port: port, Path: "/",
		ServerCert: serverCertFile, ServerKey: serverKeyFile, ClientCA: clientCAFile,
		IdleTimeout: 200 * time.Millisecond,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := newCaptureSink(testVIN)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = src.Run(ctx, sink, nil) }()
	time.Sleep(100 * time.Millisecond)

	// Connect but never push a frame: the idle deadline reaps the connection.
	car, err := mock.NewStreamVehicle("wss://127.0.0.1:"+strconv.Itoa(port), "txid-idle", nil, clientCertPEM, clientKeyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := car.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer car.Close()

	if !waitFor(buf, "msg=\"stream idle-timeout\"", 3*time.Second) {
		t.Fatalf("missing stream idle-timeout line in:\n%s", buf.String())
	}
}
