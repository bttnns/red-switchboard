package cache

import (
	"context"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
)

// TestMergeStreamChargePowerKeepsPollLive: a Fleet Telemetry charge frame carries
// ONLY PowerKw (AC/DCChargingPower). It must update the live session's power
// without zeroing the poll-owned fields (current, energy added, time-remaining,
// fast-charger flag) -- otherwise TeslaMate blanks "Remaining Time" and logs 0 kWh
// added mid-session.
func TestMergeStreamChargePowerKeepsPollLive(t *testing.T) {
	m := NewMerger("v1")
	// A poll establishes the full charging session.
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Charger: vehicle.ChargerCharging, BatteryLevelPct: 53},
		Live:      &vehicle.LiveSession{PowerKw: 2.7, CurrentA: 12, TotalChargedEnergy: 1.5, TimeRemainingSec: 3600},
		FetchedAt: time.Now(),
	}, time.Hour)
	// A stream frame pushes only fresh power (+ SOC).
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{BatteryLevelPct: 54},
		Live:          &vehicle.LiveSession{PowerKw: 3.1},
		StreamPresent: vehicle.StreamSOC | vehicle.StreamChargePower,
		FetchedAt:     time.Now(),
	}, time.Hour)
	got := m.Latest()
	if assert.NotNil(t, got.Live) {
		assert.Equal(t, 3.1, got.Live.PowerKw, "power updated from the stream")
		assert.Equal(t, 12.0, got.Live.CurrentA, "current preserved from the poll")
		assert.Equal(t, 1.5, got.Live.TotalChargedEnergy, "energy added preserved from the poll")
		assert.Equal(t, 3600, got.Live.TimeRemainingSec, "time remaining preserved from the poll")
	}
	assert.Equal(t, 54.0, got.State.BatteryLevelPct, "SOC updated from the stream")
}

// TestMergeStreamChargeEnergyInUpdatesSession: a Fleet Telemetry frame carrying
// AC/DCChargingEnergyIn must update the live session's cumulative kWh (the source
// of TeslaMate's required charge_energy_added over a fully-streamed charge) while
// preserving the poll-owned fields it did not carry.
func TestMergeStreamChargeEnergyInUpdatesSession(t *testing.T) {
	m := NewMerger("v1")
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Charger: vehicle.ChargerCharging},
		Live:      &vehicle.LiveSession{PowerKw: 7, CurrentA: 30, TotalChargedEnergy: 2, TimeRemainingSec: 1800},
		FetchedAt: time.Now(),
	}, time.Hour)
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{},
		Live:          &vehicle.LiveSession{TotalChargedEnergy: 4.5},
		StreamPresent: vehicle.StreamChargeEnergyIn,
		FetchedAt:     time.Now(),
	}, time.Hour)
	got := m.Latest()
	if assert.NotNil(t, got.Live) {
		assert.Equal(t, 4.5, got.Live.TotalChargedEnergy, "energy updated from the stream")
		assert.Equal(t, 7.0, got.Live.PowerKw, "power preserved from the poll")
		assert.Equal(t, 30.0, got.Live.CurrentA, "current preserved from the poll")
		assert.Equal(t, 1800, got.Live.TimeRemainingSec, "time remaining preserved from the poll")
	}
}

// TestMergeStreamZeroPowerKeepsStoredSession: the decode now surfaces a 0-power
// charge frame (so the cache can detect charge-end), but the merge must still NOT
// zero the stored session -- it keeps the last non-zero power until a poll refreshes
// the session. Otherwise a charge-end frame would blank TeslaMate's live charge row.
func TestMergeStreamZeroPowerKeepsStoredSession(t *testing.T) {
	m := NewMerger("v1")
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Charger: vehicle.ChargerCharging},
		Live:      &vehicle.LiveSession{PowerKw: 3, TotalChargedEnergy: 5},
		FetchedAt: time.Now(),
	}, time.Hour)
	// A 0-power stream frame (charge-end signal) must not zero the stored session.
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{},
		Live:          &vehicle.LiveSession{PowerKw: 0},
		StreamPresent: vehicle.StreamChargePower,
		FetchedAt:     time.Now(),
	}, time.Hour)
	got := m.Latest()
	if assert.NotNil(t, got.Live) {
		assert.Equal(t, 3.0, got.Live.PowerKw, "stored power kept (merge ignores the 0)")
		assert.Equal(t, 5.0, got.Live.TotalChargedEnergy, "energy preserved")
	}
}

// TestMergeStreamDeltaFrameDoesNotClobber: Fleet Telemetry sends field-level
// deltas, not synchronized snapshots. A lat-only frame must update location
// without zeroing the speed an earlier frame supplied (hold-last-known). This is
// the section-11 async-fields invariant.
func TestMergeStreamDeltaFrameDoesNotClobber(t *testing.T) {
	m := NewMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 30, Location: &vehicle.Location{Latitude: 1, Longitude: 2}},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamLoc,
		FetchedAt:     time.UnixMilli(1000),
	}, 0)
	// A later frame carries ONLY a new location; speed is absent.
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Location: &vehicle.Location{Latitude: 3, Longitude: 4}},
		StreamPresent: vehicle.StreamLoc,
		FetchedAt:     time.UnixMilli(2000),
	}, 0)
	got := m.Latest()
	assert.Equal(t, 30.0, got.State.SpeedMps, "absent speed must not be zeroed by a lat-only frame")
	assert.Equal(t, 3.0, got.State.Location.Latitude, "present location updated")
	assert.Equal(t, vehicle.StreamSpeed|vehicle.StreamLoc, got.StreamPresent, "presence accumulated")
}

// TestMergeStreamStoppedCarKeepsZeroSpeed: a frame carrying speed=0 (the car
// stopped) must update speed to 0, not be dropped as a keepalive. Presence is
// the gate, not the value.
func TestMergeStreamStoppedCarKeepsZeroSpeed(t *testing.T) {
	m := NewMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 30},
		StreamPresent: vehicle.StreamSpeed,
		FetchedAt:     time.UnixMilli(1000),
	}, 0)
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{SpeedMps: 0},
		StreamPresent: vehicle.StreamSpeed,
		FetchedAt:     time.UnixMilli(2000),
	}, 0)
	got := m.Latest()
	assert.Equal(t, 0.0, got.State.SpeedMps, "speed=0 (stopped) must be applied, not dropped")
}

// TestMergeAsOfIsMonotonicAcrossSinks: AsOf is the single emitted timestamp both
// sinks serve. It must track the freshest poll/stream time AND never regress, even
// when a staler poll lands after a fresh stream frame -- the regression is exactly
// what made TeslaMate write end_date < start_date and crash.
func TestMergeAsOfIsMonotonicAcrossSinks(t *testing.T) {
	m := NewMerger("v1")
	// Fresh stream frame at t=5000 advances AsOf.
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 30},
		StreamPresent: vehicle.StreamSpeed,
		FetchedAt:     time.UnixMilli(5000),
	}, 0)
	assert.Equal(t, time.UnixMilli(5000), m.Latest().AsOf, "stream frame sets AsOf")
	// A staler poll (FetchedAt=1000) must NOT pull AsOf backward.
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, RangeKm: 400},
		FetchedAt: time.UnixMilli(1000),
	}, 0)
	assert.Equal(t, time.UnixMilli(5000), m.Latest().AsOf, "stale poll must not regress AsOf")
	// A newer poll advances it.
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, RangeKm: 400},
		FetchedAt: time.UnixMilli(9000),
	}, 0)
	assert.Equal(t, time.UnixMilli(9000), m.Latest().AsOf, "newer poll advances AsOf")
}

// TestMergePollCopyBackIsPresenceAware: while the stream is fresh, a staler poll
// must not regress the fields the stream has supplied, but the poll's value for a
// field the stream never supplied must pass through.
func TestMergePollCopyBackIsPresenceAware(t *testing.T) {
	m := NewMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 30},
		StreamPresent: vehicle.StreamSpeed, // stream owns speed only; not range
		FetchedAt:     time.UnixMilli(5000),
	}, 0)
	// Older poll tries to regress speed to 0 and supply range 400.
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 0, RangeKm: 400},
		FetchedAt: time.UnixMilli(1000),
	}, 0)
	got := m.Latest()
	assert.Equal(t, 30.0, got.State.SpeedMps, "stream-owned speed protected from stale poll")
	assert.Equal(t, 400, got.State.RangeKm, "poll-supplied range (stream never supplied it) passes through")
}

// TestMergeStreamThenPollDoesNotRegressStreamFields: a stream write sets the
// streamed live fields; a later poll with an OLDER FetchedAt must NOT overwrite
// them (the stream is fresher). The poll still owns the non-streamed fields it
// carries.
func TestMergeStreamThenPollDoesNotRegressStreamFields(t *testing.T) {
	m := NewMerger("v1")
	streamTime := time.UnixMilli(5000)
	pollTime := time.UnixMilli(1000) // older than the stream

	m.MergeStream(vehicle.Snapshot{
		State: &vehicle.State{
			Power: vehicle.PowerOnline, Gear: vehicle.GearDrive,
			SpeedMps: 30, Location: &vehicle.Location{Latitude: 1, Longitude: 2},
		},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear | vehicle.StreamLoc,
		FetchedAt:     streamTime,
	}, 0)

	// A poll arrives whose own SpeedMps/Location differ but whose FetchedAt is
	// older: the stream values must win.
	m.MergePoll(vehicle.Snapshot{
		State: &vehicle.State{
			Power: vehicle.PowerOnline, Gear: vehicle.GearPark,
			SpeedMps: 0, Location: &vehicle.Location{Latitude: 9, Longitude: 9},
		},
		FetchedAt: pollTime,
	}, 0)

	got := m.Latest()
	assert.Equal(t, 30.0, got.State.SpeedMps, "stream speed preserved against stale poll")
	assert.Equal(t, 1.0, got.State.Location.Latitude, "stream location preserved")
	assert.Equal(t, vehicle.GearDrive, got.State.Gear, "stream gear preserved")
	assert.False(t, got.StreamFields.IsZero(), "StreamFields provenance retained")
}

// TestMergePollOwnsNonStreamedFields: a poll supplies fields the stream does not
// carry (e.g. CabinTempC); a subsequent stream write must NOT clobber them.
func TestMergePollOwnsNonStreamedFields(t *testing.T) {
	m := NewMerger("v1")
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, CabinTempC: 21.5},
		FetchedAt: time.UnixMilli(1000),
	}, 0)
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 10},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.UnixMilli(2000),
	}, 0)

	got := m.Latest()
	assert.Equal(t, 21.5, got.State.CabinTempC, "poll-owned field preserved across stream write")
	assert.Equal(t, 10.0, got.State.SpeedMps, "stream field applied")
}

// TestMergeStreamFirstTakeWhole: the first-ever data from the stream (before any
// poll) is taken whole so the car is not blank until the poll arrives.
func TestMergeStreamFirstTakeWhole(t *testing.T) {
	m := NewMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 10},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.UnixMilli(1000),
	}, 0)
	got := m.Latest()
	assert.Equal(t, vehicle.GearDrive, got.State.Gear)
	assert.Equal(t, 10.0, got.State.SpeedMps)
}

// TestStalledStreamAgesToOffline: when StreamFields is older than staleAfter,
// commit degrades Power online -> offline so a stalled stream cannot produce a
// phantom infinite drive. The values stay (briefe outage tolerated).
func TestStalledStreamAgesToOffline(t *testing.T) {
	m := NewMerger("v1")
	stale := time.Minute
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 10},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.Now().Add(-2 * stale), // older than staleAfter
	}, stale)
	got := m.Latest()
	assert.Equal(t, vehicle.PowerOffline, got.State.Power, "stalled stream degrades to offline")
	assert.Equal(t, 10.0, got.State.SpeedMps, "values retained through the outage")
}

// TestCopyOnWriteIsolation: Latest() returns a snapshot whose State is not
// mutated by a subsequent merge. The copy-on-write invariant lets the read path
// skip a deep copy.
func TestCopyOnWriteIsolation(t *testing.T) {
	m := NewMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 10, Location: &vehicle.Location{Latitude: 1}},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamLoc,
		FetchedAt:     time.UnixMilli(1000),
	}, 0)
	prev := m.Latest()
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 99, Location: &vehicle.Location{Latitude: 2}},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamLoc,
		FetchedAt:     time.UnixMilli(2000),
	}, 0)
	assert.Equal(t, 10.0, prev.State.SpeedMps, "previously returned snapshot must not mutate")
	assert.Equal(t, 1.0, prev.State.Location.Latitude)
	assert.Equal(t, 99.0, m.Latest().State.SpeedMps)
}

// TestSubscribeDropOnSlowConsumer: a subscriber that never drains still lets the
// writer proceed (non-blocking send), and the channel does not block the Merger.
// A second subscriber that does drain still receives events.
func TestSubscribeDropOnSlowConsumer(t *testing.T) {
	m := NewMerger("v1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := m.Subscribe(ctx)
	fast := m.Subscribe(ctx)

	// Burst more events than the buffer: the slow channel fills and drops; the
	// writer never blocks.
	for i := 0; i < subscriberBuf+5; i++ {
		m.MergeStream(vehicle.Snapshot{
			State:         &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: float64(i)},
			StreamPresent: vehicle.StreamSpeed,
			FetchedAt:     time.UnixMilli(int64(i)),
		}, 0)
	}
	// The fast consumer drains at least one event.
	select {
	case <-fast:
	default:
		t.Fatal("fast consumer should have received an event")
	}
	// The slow consumer's channel is bounded; it did not block the writer (we
	// reached here). Drain what's buffered.
	_ = slow
}

// pollSnap builds a poll snapshot with a given liveness + gear at time t.
func pollSnap(power vehicle.Power, gear vehicle.Gear, t time.Time) vehicle.Snapshot {
	return vehicle.Snapshot{State: &vehicle.State{Power: power, Gear: gear}, FetchedAt: t}
}

// TestPollLivenessSettledOfflineBecomesAsleep: a car that was charging/parked and
// then reports "offline" (Tesla's label for deep sleep) is presented as asleep, but
// only after the offline persists past one poll, so TeslaMate's :charging FSM
// completes the charge instead of looping "went offline while charging".
func TestPollLivenessSettledOfflineBecomesAsleep(t *testing.T) {
	base := time.Date(2026, 6, 30, 20, 0, 0, 0, time.UTC)
	m := NewMerger("v1")

	// Charging (online, parked gear).
	m.MergePoll(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark, Charger: vehicle.ChargerCharging},
		FetchedAt: base,
	}, time.Hour)

	// First offline poll: still offline (debounce holds one poll).
	got := m.MergePoll(pollSnap(vehicle.PowerOffline, vehicle.GearUnknown, base.Add(20*time.Minute)), time.Hour)
	assert.Equal(t, vehicle.PowerOffline, got.State.Power, "first offline poll is not yet reclassified")

	// Second consecutive offline poll: confirmed settled -> asleep.
	got = m.MergePoll(pollSnap(vehicle.PowerOffline, vehicle.GearUnknown, base.Add(25*time.Minute)), time.Hour)
	assert.Equal(t, vehicle.PowerSleep, got.State.Power, "a settled car that stays offline is presented as asleep")
}

// TestPollLivenessDrivingStaysOffline: a car lost while DRIVING must stay offline
// across the whole episode so TeslaMate's graduated drive-timeout (phantom-drive)
// guard fires; the gear-cleared offline polls must not flip it to asleep.
func TestPollLivenessDrivingStaysOffline(t *testing.T) {
	base := time.Date(2026, 6, 30, 14, 0, 0, 0, time.UTC)
	m := NewMerger("v1")

	m.MergePoll(pollSnap(vehicle.PowerOnline, vehicle.GearDrive, base), time.Hour)

	for i := 1; i <= 4; i++ {
		got := m.MergePoll(pollSnap(vehicle.PowerOffline, vehicle.GearUnknown, base.Add(time.Duration(i)*time.Minute)), time.Hour)
		assert.Equalf(t, vehicle.PowerOffline, got.State.Power, "offline poll %d during a lost drive stays offline", i)
	}
}

// TestPollLivenessTransientBlipNoReclassify: a single transient offline poll mid-
// charge (count 1) must not reclassify; when the car comes back online the episode
// resets, so a later genuine sleep still debounces from scratch.
func TestPollLivenessTransientBlipNoReclassify(t *testing.T) {
	base := time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC)
	m := NewMerger("v1")

	m.MergePoll(pollSnap(vehicle.PowerOnline, vehicle.GearPark, base), time.Hour)
	got := m.MergePoll(pollSnap(vehicle.PowerOffline, vehicle.GearUnknown, base.Add(time.Minute)), time.Hour)
	assert.Equal(t, vehicle.PowerOffline, got.State.Power, "a lone offline blip is not reclassified")

	// Back online: episode resets.
	got = m.MergePoll(pollSnap(vehicle.PowerOnline, vehicle.GearPark, base.Add(2*time.Minute)), time.Hour)
	assert.Equal(t, vehicle.PowerOnline, got.State.Power)

	// A fresh offline run debounces again from the first poll.
	got = m.MergePoll(pollSnap(vehicle.PowerOffline, vehicle.GearUnknown, base.Add(3*time.Minute)), time.Hour)
	assert.Equal(t, vehicle.PowerOffline, got.State.Power, "the reset episode debounces from scratch")
	got = m.MergePoll(pollSnap(vehicle.PowerOffline, vehicle.GearUnknown, base.Add(8*time.Minute)), time.Hour)
	assert.Equal(t, vehicle.PowerSleep, got.State.Power)
}
