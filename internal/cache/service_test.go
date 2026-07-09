package cache

import (
	"context"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestService builds an in-memory (no asof_file) Service, failing the test on
// the construction error so callers stay terse.
func newTestService(t *testing.T, ident []vehicle.Identity, staleAfter time.Duration) *Service {
	t.Helper()
	svc, err := NewService(ident, staleAfter, "", "", 0, nil)
	require.NoError(t, err)
	return svc
}

// TestServiceMergePollAndStream: the Service routes a poll write through MergePoll
// and a stream write through Put, both landing in the same per-vehicle Merger so a
// consumer reading Latest sees the merged result.
func TestServiceMergePollAndStream(t *testing.T) {
	ident := []vehicle.Identity{{ID: "g1", VIN: "VIN1"}}
	svc := newTestService(t, ident, 0)

	// Poll supplies the non-streamed + initial state.
	svc.MergePoll("g1", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, CabinTempC: 20},
		FetchedAt: time.UnixMilli(1000),
	})
	// Stream supplies a streamed field.
	_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 15},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.UnixMilli(2000),
	})

	got := svc.Latest("g1")
	assert.Equal(t, 20.0, got.State.CabinTempC, "poll-owned field retained")
	assert.Equal(t, 15.0, got.State.SpeedMps, "streamed field applied")
	assert.Equal(t, vehicle.GearDrive, got.State.Gear)
}

// TestServicePutRejectsUnknownID: a push for an unknown vehicle is a no-op
// (deny-by-default); it does not create a Merger or fill the cache.
func TestServicePutRejectsUnknownID(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
	_ = svc.Put(context.Background(), "rogue", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 99},
		FetchedAt: time.UnixMilli(1000),
	})
	assert.Equal(t, vehicle.Snapshot{}, svc.Latest("rogue"), "unknown id rejected")
}

// TestServicePutDropsKeepalive: a frame carrying no streamed field
// (StreamPresent == 0) never reaches the Merger (no spurious subscriber notify).
func TestServicePutDropsKeepalive(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
	_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline}, // no streamed field
		FetchedAt: time.UnixMilli(1000),
	})
	assert.Equal(t, vehicle.Snapshot{}, svc.Latest("g1"), "keepalive frame dropped")
}

// TestServicePutTriggersPollOnChargeEnd: Put wires the boundary detector to the
// poll trigger, so a charge-end frame (power>0 -> 0) fires a poll for the vehicle,
// while starting a charge does not.
func TestServicePutTriggersPollOnChargeEnd(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
	var triggered []string
	svc.SetPollTrigger(func(id string) { triggered = append(triggered, id) })

	// Establish a live charge (power > 0). No prior session, so no close boundary.
	_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
		State:         &vehicle.State{Charger: vehicle.ChargerCharging},
		Live:          &vehicle.LiveSession{PowerKw: 3},
		StreamPresent: vehicle.StreamChargePower,
		FetchedAt:     time.UnixMilli(1000),
	})
	assert.Empty(t, triggered, "starting a charge is not a close boundary")

	// A 0-power frame = charge ended -> trigger a poll.
	_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
		State:         &vehicle.State{},
		Live:          &vehicle.LiveSession{PowerKw: 0},
		StreamPresent: vehicle.StreamChargePower,
		FetchedAt:     time.UnixMilli(2000),
	})
	assert.Equal(t, []string{"g1"}, triggered, "charge-end fires a poll")
}

// TestServiceOnStreamDisconnect: when the stream connection drops, OnStreamDisconnect
// fires the poll trigger so the cache gets a fresh state promptly. This prevents a
// mid-drive stream disconnect from leaving stale GearDrive in the cache until the
// next scheduled poll.
func TestServiceOnStreamDisconnect(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
	var triggered []string
	svc.SetPollTrigger(func(id string) { triggered = append(triggered, id) })

	// Establish a driving stream state.
	_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
		State:         &vehicle.State{Gear: vehicle.GearDrive, SpeedMps: 10},
		StreamPresent: vehicle.StreamGear | vehicle.StreamSpeed,
		FetchedAt:     time.UnixMilli(1000),
	})
	triggered = nil // reset; drive-start boundary fired above, clear it

	// Stream disconnects without a GearPark frame.
	svc.OnStreamDisconnect(context.Background(), "g1")
	assert.Equal(t, []string{"g1"}, triggered, "disconnect fires a poll trigger")

	// Unknown id: no-op, no panic.
	svc.OnStreamDisconnect(context.Background(), "unknown")
}

// TestServiceOnStreamDisconnectSuppressesRecycle: with a freshness window set, a
// disconnect whose last stream frame is recent (Tesla's ~120s connection recycle)
// is a no-op: the cache is still current and polling it would just bill a redundant
// call. A disconnect after the stream has genuinely fallen silent still polls.
func TestServiceOnStreamDisconnectSuppressesRecycle(t *testing.T) {
	drivingFrame := func(fetchedAt time.Time) vehicle.Snapshot {
		return vehicle.Snapshot{
			State:         &vehicle.State{Gear: vehicle.GearDrive, SpeedMps: 10},
			StreamPresent: vehicle.StreamGear | vehicle.StreamSpeed,
			FetchedAt:     fetchedAt,
		}
	}

	t.Run("fresh stream = recycle, suppressed", func(t *testing.T) {
		svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
		var triggered []string
		svc.SetPollTrigger(func(id string) { triggered = append(triggered, id) })
		svc.SetStreamFreshWithin(60 * time.Second)

		_ = svc.Put(context.Background(), "g1", drivingFrame(time.Now()))
		triggered = nil // clear the drive-start boundary poll

		svc.OnStreamDisconnect(context.Background(), "g1")
		assert.Empty(t, triggered, "a fresh-stream disconnect is a recycle, no poll")
	})

	t.Run("silent stream = genuine drop, polls", func(t *testing.T) {
		svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
		var triggered []string
		svc.SetPollTrigger(func(id string) { triggered = append(triggered, id) })
		svc.SetStreamFreshWithin(60 * time.Second)

		// Last frame is older than the window: the stream has gone quiet.
		_ = svc.Put(context.Background(), "g1", drivingFrame(time.Now().Add(-2*time.Minute)))
		triggered = nil

		svc.OnStreamDisconnect(context.Background(), "g1")
		assert.Equal(t, []string{"g1"}, triggered, "a silent-stream disconnect still polls")
	})
}

// TestServicePutTriggersPollOnSustainedWake: a poll puts the car asleep; the merge
// keeps it asleep on a stream frame, so sustained frames must fire a single wake
// PollNow (so the poll confirms online and pulls poll-only fields) while a lone
// stray frame must not (TeslaMate sleep survives a buffered frame).
func TestServicePutTriggersPollOnSustainedWake(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
	var triggered []string
	svc.SetPollTrigger(func(id string) { triggered = append(triggered, id) })

	// Poll establishes the confirmed-asleep state.
	svc.MergePoll("g1", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerSleep},
		FetchedAt: time.UnixMilli(1000),
	})

	t0 := time.UnixMilli(2000)
	frame := func(at time.Time) {
		_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
			State:         &vehicle.State{SpeedMps: 1},
			StreamPresent: vehicle.StreamSpeed,
			FetchedAt:     at,
		})
	}

	// One stray frame: not a wake.
	frame(t0)
	assert.Empty(t, triggered, "a single stray frame does not wake-poll")
	assert.Equal(t, vehicle.PowerSleep, svc.Latest("g1").State.Power, "merge keeps it asleep")

	// Sustained frames spanning the window: fire exactly once.
	frame(t0.Add(2 * time.Second))
	frame(t0.Add(4 * time.Second))
	frame(t0.Add(6 * time.Second))
	assert.Equal(t, []string{"g1"}, triggered, "sustained wake fires one poll")
	assert.Equal(t, vehicle.PowerSleep, svc.Latest("g1").State.Power,
		"merge still leaves the flip to the poll, not the stream")
}

// TestServiceKnownIDs: KnownIDs reflects the served set.
func TestServiceKnownIDs(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}, {ID: "g2"}}, 0)
	known := svc.KnownIDs()
	assert.True(t, known["g1"])
	assert.True(t, known["g2"])
	assert.False(t, known["rogue"])
}
