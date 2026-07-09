package v1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// fakeSink is the test cache: it implements streamsource.RecordSink (Put/
// KnownIDs) AND streamsource.CacheWatcher (Latest/Subscribe/Vehicles) so the
// source can drive connect/disconnect off a Power state the test mutates. It
// stands in for internal/cache.Service (passed as both Run args).
type fakeSink struct {
	mu   sync.Mutex
	ids  map[string]bool
	snap map[string]vehicle.Snapshot
	subs map[string][]chan struct{}
	id   string
	vin  string

	puts    int
	lastPut vehicle.Snapshot
	lastID  string
}

func newFakeSink(id, vin string) *fakeSink {
	return &fakeSink{
		ids:  map[string]bool{id: true},
		snap: map[string]vehicle.Snapshot{id: {State: &vehicle.State{Power: vehicle.PowerOnline}}},
		subs: map[string][]chan struct{}{},
		id:   id,
		vin:  vin,
	}
}

func (f *fakeSink) Put(_ context.Context, id string, snap vehicle.Snapshot) error {
	f.mu.Lock()
	f.puts++
	f.lastPut = snap
	f.lastID = id
	f.mu.Unlock()
	return nil
}

func (f *fakeSink) KnownIDs() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]bool, len(f.ids))
	for k := range f.ids {
		out[k] = true
	}
	return out
}

func (f *fakeSink) Latest(id string) vehicle.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap[id]
}

func (f *fakeSink) Subscribe(ctx context.Context, id string) <-chan struct{} {
	ch := make(chan struct{}, 8)
	f.mu.Lock()
	f.subs[id] = append(f.subs[id], ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		for i, c := range f.subs[id] {
			if c == ch {
				f.subs[id] = append(f.subs[id][:i], f.subs[id][i+1:]...)
				break
			}
		}
		f.mu.Unlock()
	}()
	return ch
}

func (f *fakeSink) Vehicles() []vehicle.Identity {
	return []vehicle.Identity{{ID: f.id, VIN: f.vin}}
}

// setPower updates the cached Power state for id and notifies subscribers, the
// test's analogue of the poll loop writing a new summary.
func (f *fakeSink) setPower(id string, p vehicle.Power) {
	f.mu.Lock()
	s := f.snap[id]
	if s.State == nil {
		s.State = &vehicle.State{}
	}
	s.State.Power = p
	f.snap[id] = s
	subs := append([]chan struct{}(nil), f.subs[id]...)
	f.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// writeCreds writes a minimal Owner API creds file and returns its path.
func writeCreds(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(p, []byte(`{"access_token":"test-owner-token","refresh_token":"r","expires_in":3600}`), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return p
}

// waitFor polls cond until it returns true or the deadline elapses.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSourceDialsAndDecodes is the integration test: the source dials the mock
// Owner streaming server, sends data:subscribe_oauth, and a pushed data:update
// frame is decoded into the cache. It then asserts state-driven disconnect on
// sleep and reconnect on wake.
func TestSourceDialsAndDecodes(t *testing.T) {
	srv := mock.NewStreamServer()
	defer srv.Close()

	src := newOwnerSource(t, srv.URL(), writeCreds(t))
	sink := newFakeSink("123", "5YJSA11111111111")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx, sink, sink) }()

	// The supervisor starts a dialer while online; wait for the handshake.
	waitFor(t, "subscribe handshake", 3*time.Second, func() bool {
		return len(srv.Handshakes()) > 0
	})
	hs := srv.Handshakes()[0]
	if want := `"tag":"5YJSA11111111111"`; !strings.Contains(hs, want) {
		t.Errorf("handshake missing %q: %s", want, hs)
	}
	if want := `"token":"test-owner-token"`; !strings.Contains(hs, want) {
		t.Errorf("handshake missing access token: %s", hs)
	}

	if connected := src.Stats().Connected; connected != 1 {
		t.Errorf("connected = %d, want 1 after dial", connected)
	}

	// Push a driving data:update frame; the cache receives the decoded snapshot.
	srv.Push(`{"msg_type":"data:update","tag":"5YJSA11111111111","value":"1700000000000,60,12345.6,78,120,90,37.7749,-122.4194,-30,D,210,205,90"}`)
	waitFor(t, "first frame", 3*time.Second, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.puts > 0 && sink.lastID == "123"
	})
	sink.mu.Lock()
	got := sink.lastPut
	sink.mu.Unlock()
	if got.State == nil || got.State.Gear != vehicle.GearDrive {
		t.Errorf("decoded frame: gear = %v, want GearDrive", gearOf(got))
	}
	if got.StreamPresent&vehicle.StreamSpeed == 0 {
		t.Errorf("decoded frame missing speed presence: %b", got.StreamPresent)
	}

	// State-driven disconnect: the car sleeps -> the supervisor cancels the
	// dialer -> the WS closes -> connected drops to 0.
	sink.setPower("123", vehicle.PowerSleep)
	waitFor(t, "disconnect on sleep", 3*time.Second, func() bool {
		return src.Stats().Connected == 0
	})

	// Reconnect on wake: online again -> new dialer -> handshake -> frame.
	sink.setPower("123", vehicle.PowerOnline)
	waitFor(t, "reconnect handshake", 3*time.Second, func() bool {
		return len(srv.Handshakes()) >= 2
	})
	sink.mu.Lock()
	before := sink.puts
	sink.mu.Unlock()
	// The reconnect handshake is recorded before the source's read loop on the
	// new connection is fully running, so a single push can race ahead of it and
	// be dropped. Push until one lands (each frame decodes idempotently).
	waitFor(t, "second frame", 3*time.Second, func() bool {
		srv.Push(`{"msg_type":"data:update","tag":"5YJSA11111111111","value":"1700000000001,65,12346.0,77,121,91,37.7750,-122.4195,-28,D,209,204,91"}`)
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.puts > before
	})
}

// TestSourceDoesNotDialWhileAsleep asserts the source holds no connection while
// the cache reports the car asleep (the load-bearing property: an open Owner
// stream must not be held on a sleeping car).
func TestSourceDoesNotDialWhileAsleep(t *testing.T) {
	srv := mock.NewStreamServer()
	defer srv.Close()

	src := newOwnerSource(t, srv.URL(), writeCreds(t))
	sink := newFakeSink("123", "5YJSA11111111111")
	sink.setPower("123", vehicle.PowerSleep) // asleep from the start

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx, sink, sink) }()

	// Give the supervisor time to evaluate (and NOT dial).
	time.Sleep(300 * time.Millisecond)
	if c := src.Stats().Connected; c != 0 {
		t.Errorf("connected = %d while asleep, want 0", c)
	}
	if len(srv.Handshakes()) != 0 {
		t.Errorf("server received handshake while asleep: %v", srv.Handshakes())
	}
}

// newOwnerSource builds a source pointing at the mock server.
func newOwnerSource(t *testing.T, url, credsPath string) *ownerStreamSource {
	settings := SourceSettings{
		CredsFile:        credsPath,
		StreamURL:        url,
		HandshakeTimeout: time.Second,
		ReconnectInitial: 50 * time.Millisecond,
		ReconnectMax:     2 * time.Second,
	}
	node, err := yaml.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	var n yaml.Node
	_ = yaml.Unmarshal(node, &n)
	s, err := newSource(&n, nil)
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	return s.(*ownerStreamSource)
}

func gearOf(s vehicle.Snapshot) vehicle.Gear {
	if s.State == nil {
		return vehicle.GearUnknown
	}
	return s.State.Gear
}
