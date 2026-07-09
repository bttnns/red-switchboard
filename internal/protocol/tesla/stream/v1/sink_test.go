package v1

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fakeWatcher is a controllable streamsink.CacheWatcher for the sink test. It
// holds one snapshot per id and notifies subscribers on change.
type fakeWatcher struct {
	mu     sync.Mutex
	snaps  map[string]vehicle.Snapshot
	subs   map[string][]chan struct{}
	idents []vehicle.Identity
}

func newFakeWatcher(ids ...vehicle.Identity) *fakeWatcher {
	return &fakeWatcher{
		snaps:  make(map[string]vehicle.Snapshot),
		subs:   make(map[string][]chan struct{}),
		idents: ids,
	}
}

func (w *fakeWatcher) Vehicles() []vehicle.Identity { return w.idents }
func (w *fakeWatcher) Latest(id string) vehicle.Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snaps[id]
}
func (w *fakeWatcher) Subscribe(ctx context.Context, id string) <-chan struct{} {
	ch := make(chan struct{}, 4)
	w.mu.Lock()
	w.subs[id] = append(w.subs[id], ch)
	w.mu.Unlock()
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

// set mutates the cached snapshot for id and notifies subscribers (non-blocking).
func (w *fakeWatcher) set(id string, snap vehicle.Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snaps[id] = snap
	for _, ch := range w.subs[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// replayWatcher is a fakeWatcher that also satisfies streamsink.CacheReplayer, so
// the sink replays its buffered frames to a freshly-subscribed consumer.
type replayWatcher struct {
	*fakeWatcher
	buffered map[string][]vehicle.Snapshot
}

func (w *replayWatcher) Replay(id string) []vehicle.Snapshot { return w.buffered[id] }

// TestSinkReplaysBufferedFramesOnSubscribe is the J2 reconnect path: a consumer
// that subscribes after a restart re-receives the buffered drive frames, in order,
// BEFORE any live change, so the mid-drive gap is closed.
func TestSinkReplaysBufferedFramesOnSubscribe(t *testing.T) {
	vin := "7FATESTVIN000002"
	id := vehicle.Identity{ID: "guid2", VIN: vin, Make: "Tesla"}
	base := newFakeWatcher(id)
	w := &replayWatcher{
		fakeWatcher: base,
		buffered: map[string][]vehicle.Snapshot{
			"guid2": {
				{State: &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 10, Location: &vehicle.Location{Latitude: 1, Longitude: 1}}, FetchedAt: time.UnixMilli(1700000001000)},
				{State: &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 20, Location: &vehicle.Location{Latitude: 2, Longitude: 2}}, FetchedAt: time.UnixMilli(1700000002000)},
			},
		},
	}

	settings := map[string]any{"listen_addr": "127.0.0.1:0", "max_push_hz": 100.0, "max_consumers": 4}
	raw, _ := yaml.Marshal(settings)
	var node yaml.Node
	_ = yaml.Unmarshal(raw, &node)

	sk, err := newSink(&node, w, nil)
	require.NoError(t, err)
	handler, err := sk.Handler()
	require.NoError(t, err)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/streaming/test-token"
	consumer := mock.NewStreamConsumer(wsURL, vin)
	require.NoError(t, consumer.Connect(context.Background()))
	defer consumer.Close()

	_, ok := consumer.WaitForFrame("control:hello", time.Second)
	require.True(t, ok, "expected control:hello after subscribe")

	// Both buffered frames replay before any live change. Collect the data:update
	// frames in arrival order and assert the two speeds appear in sequence.
	_, ok = consumer.WaitForFrame("45,", time.Second) // wait until the 2nd replay lands
	require.True(t, ok, "expected the second replayed frame (20 m/s -> 45 mph)")

	var updates []string
	for _, f := range consumer.Frames() {
		if strings.Contains(f, "data:update") {
			updates = append(updates, f)
		}
	}
	require.Len(t, updates, 2, "both buffered frames replayed, none coalesced away")
	assert.Contains(t, updates[0], "22,", "first replay carries 10 m/s -> 22 mph")
	assert.Contains(t, updates[1], "45,", "second replay carries 20 m/s -> 45 mph")
}

// TestSinkPushesDataUpdateOnCacheChange: a connected consumer that sends
// data:subscribe_oauth for a known VIN receives a correctly-shaped data:update
// frame when the cache changes, and the connection stays open (keepalive). This
// is the cost-win path: TeslaMate pulls drive data from the cache over WSS.
func TestSinkPushesDataUpdateOnCacheChange(t *testing.T) {
	vin := "7FATESTVIN000001"
	id := vehicle.Identity{ID: "guid1", VIN: vin, Make: "Tesla"}
	w := newFakeWatcher(id)

	settings := map[string]any{
		"listen_addr":   "127.0.0.1:0",
		"max_push_hz":   100.0, // fast so the test does not wait on coalescing
		"max_consumers": 4,
	}
	raw, _ := yaml.Marshal(settings)
	var node yaml.Node
	_ = yaml.Unmarshal(raw, &node)

	sk, err := newSink(&node, w, nil)
	require.NoError(t, err)
	handler, err := sk.Handler()
	require.NoError(t, err)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/streaming/test-token"
	consumer := mock.NewStreamConsumer(wsURL, vin)
	require.NoError(t, consumer.Connect(context.Background()))
	defer consumer.Close()

	// Wait for the control:hello ack so the subscribe is confirmed.
	_, ok := consumer.WaitForFrame("control:hello", time.Second)
	require.True(t, ok, "expected control:hello after subscribe")

	// Mutate the cache: a driving frame WITH a location (shift_state is gated on
	// having one, so a real drive frame carries lat/lng).
	w.set("guid1", vehicle.Snapshot{
		State: &vehicle.State{
			Power: vehicle.PowerOnline, Gear: vehicle.GearDrive,
			SpeedMps: 20, OdometerMeters: 1609344, BatteryLevelPct: 72,
			Location: &vehicle.Location{Latitude: 37.5, Longitude: -122.2},
		},
		FetchedAt: time.UnixMilli(1700000000000),
	})

	frame, ok := consumer.WaitForFrame("data:update", time.Second)
	require.True(t, ok, "expected a data:update frame after cache change")
	assert.Contains(t, frame, `"tag":"`+vin+`"`)
	// shift_state D for GearDrive.
	assert.Contains(t, frame, `,D,`)
	// speed 20 m/s -> 45 mph.
	assert.Contains(t, frame, "45,")
}

// TestSinkReEmitsUnchangedColumns is the drive-distance regression guard: a second
// frame whose odometer (and other columns) are unchanged from the first must STILL
// carry them. The old per-frame diff blanked unchanged columns, so a drive boundary
// that landed on a stopped frame (odometer not ticking) got a NULL odometer and the
// drive's distance became NULL. Full frames mean every position TeslaMate persists
// has a usable odometer.
func TestSinkReEmitsUnchangedColumns(t *testing.T) {
	vin := "7FATESTVIN000003"
	id := vehicle.Identity{ID: "guid3", VIN: vin, Make: "Tesla"}
	w := newFakeWatcher(id)

	settings := map[string]any{"listen_addr": "127.0.0.1:0", "max_push_hz": 100.0, "max_consumers": 4}
	raw, _ := yaml.Marshal(settings)
	var node yaml.Node
	_ = yaml.Unmarshal(raw, &node)

	sk, err := newSink(&node, w, nil)
	require.NoError(t, err)
	handler, err := sk.Handler()
	require.NoError(t, err)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/streaming/test-token"
	consumer := mock.NewStreamConsumer(wsURL, vin)
	require.NoError(t, consumer.Connect(context.Background()))
	defer consumer.Close()

	_, ok := consumer.WaitForFrame("control:hello", time.Second)
	require.True(t, ok, "expected control:hello after subscribe")

	// A stopped-but-in-drive frame: odometer 1609344 m -> 1000 mi, speed 0.
	stopped := func(asOf int64) vehicle.Snapshot {
		return vehicle.Snapshot{
			State: &vehicle.State{
				Power: vehicle.PowerOnline, Gear: vehicle.GearDrive,
				SpeedMps: 0, OdometerMeters: 1609344, BatteryLevelPct: 72,
				Location: &vehicle.Location{Latitude: 37.5, Longitude: -122.2},
			},
			FetchedAt: time.UnixMilli(asOf),
		}
	}

	w.set("guid3", stopped(1700000000000))
	f1, ok := consumer.WaitForFrame("data:update", time.Second)
	require.True(t, ok, "expected first data:update")
	assert.Contains(t, f1, ",1000,", "first frame carries odometer")

	// Same columns, new AsOf: nothing moved, but the odometer MUST still be present.
	w.set("guid3", stopped(1700000001000))
	var f2 string
	require.Eventually(t, func() bool {
		for _, f := range consumer.Frames() {
			if strings.Contains(f, "data:update") && strings.Contains(f, "1700000001000") {
				f2 = f
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond, "expected second data:update")
	assert.Contains(t, f2, ",1000,", "second (unchanged) frame must re-emit odometer, not blank it")
}
