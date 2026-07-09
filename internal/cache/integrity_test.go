package cache

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedMerger builds a Merger with the integrity gate wired (shared counters,
// quiet logger), so a test exercises the real MergeStream path WITH the gate.
func gatedMerger(id string) (*Merger, *integrityCounters) {
	ic := &integrityCounters{}
	m := NewMerger(id)
	m.integrity = ic
	m.logger = log.New(io.Discard, "", 0)
	return m, ic
}

// seedPoll establishes a trusted reference snapshot via the poll path at pollAt.
func seedPoll(m *Merger, st *vehicle.State, pollAt time.Time) {
	m.MergePoll(vehicle.Snapshot{State: st, FetchedAt: pollAt}, 0)
}

// TestIntegrityRejectsImplausibleField is the safety case's REJECTION half: an
// injected physically-impossible / regressing stream value of each kind is
// dropped (the counter for its reason increments and the served value holds
// last-known), while every other field in the same frame still merges.
func TestIntegrityRejectsImplausibleField(t *testing.T) {
	t0 := time.UnixMilli(1_000_000)
	// Trusted reference: parked, 10 km odometer, 50% SOC, a fix in San Francisco.
	ref := func() *vehicle.State {
		return &vehicle.State{
			Power:           vehicle.PowerOnline,
			OdometerMeters:  10_000,
			BatteryLevelPct: 50,
			SpeedMps:        0,
			Location:        &vehicle.Location{Latitude: 37.7749, Longitude: -122.4194},
		}
	}

	tests := []struct {
		name   string
		frame  vehicle.Snapshot
		reason integrityReason
		// check asserts the rejected field held last-known and any clean co-field
		// in the same frame still applied.
		check func(t *testing.T, got vehicle.Snapshot)
	}{
		{
			name: "odometer regress",
			frame: vehicle.Snapshot{
				State:         &vehicle.State{OdometerMeters: 9_000, BatteryLevelPct: 51},
				StreamPresent: vehicle.StreamOdometer | vehicle.StreamSOC,
				FetchedAt:     t0.Add(2 * time.Second),
			},
			reason: reasonOdometerRegress,
			check: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 10_000, got.State.OdometerMeters, "regressed odometer held last-known")
				assert.Equal(t, 51.0, got.State.BatteryLevelPct, "clean SOC in the same frame applied")
			},
		},
		{
			name: "odometer impossible jump",
			frame: vehicle.Snapshot{
				// +1000 km in 2 s -> 500 km/s, far over the ceiling.
				State:         &vehicle.State{OdometerMeters: 1_010_000},
				StreamPresent: vehicle.StreamOdometer,
				FetchedAt:     t0.Add(2 * time.Second),
			},
			reason: reasonOdometerRegress,
			check: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 10_000, got.State.OdometerMeters, "impossible odometer jump held last-known")
			},
		},
		{
			name: "soc over 100",
			frame: vehicle.Snapshot{
				State:         &vehicle.State{BatteryLevelPct: 250, SpeedMps: 12},
				StreamPresent: vehicle.StreamSOC | vehicle.StreamSpeed,
				FetchedAt:     t0.Add(2 * time.Second),
			},
			reason: reasonSOCRange,
			check: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 50.0, got.State.BatteryLevelPct, "out-of-range SOC held last-known")
				assert.Equal(t, 12.0, got.State.SpeedMps, "clean speed in the same frame applied")
			},
		},
		{
			name: "soc negative",
			frame: vehicle.Snapshot{
				State:         &vehicle.State{BatteryLevelPct: -5},
				StreamPresent: vehicle.StreamSOC,
				FetchedAt:     t0.Add(2 * time.Second),
			},
			reason: reasonSOCRange,
			check: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 50.0, got.State.BatteryLevelPct, "negative SOC held last-known")
			},
		},
		{
			name: "speed over ceiling",
			frame: vehicle.Snapshot{
				State:         &vehicle.State{SpeedMps: 9_999, BatteryLevelPct: 49},
				StreamPresent: vehicle.StreamSpeed | vehicle.StreamSOC,
				FetchedAt:     t0.Add(2 * time.Second),
			},
			reason: reasonSpeedRange,
			check: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 0.0, got.State.SpeedMps, "impossible speed held last-known")
				assert.Equal(t, 49.0, got.State.BatteryLevelPct, "clean SOC in the same frame applied")
			},
		},
		{
			name: "gps teleport",
			frame: vehicle.Snapshot{
				// San Francisco -> New York (~4100 km) in 2 s is a teleport.
				State:         &vehicle.State{Location: &vehicle.Location{Latitude: 40.7128, Longitude: -74.0060}},
				StreamPresent: vehicle.StreamLoc,
				FetchedAt:     t0.Add(2 * time.Second),
			},
			reason: reasonGPSTeleport,
			check: func(t *testing.T, got vehicle.Snapshot) {
				assert.InDelta(t, 37.7749, got.State.Location.Latitude, 1e-9, "teleport fix held last-known")
				assert.InDelta(t, -122.4194, got.State.Location.Longitude, 1e-9, "teleport fix held last-known")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ic := gatedMerger("v1")
			seedPoll(m, ref(), t0)
			m.MergeStream(tc.frame, 0)
			got := m.Latest()
			assert.Equal(t, int64(1), ic.Snapshot()[string(tc.reason)], "the reason counter incremented exactly once")
			tc.check(t, got)
		})
	}
}

// TestIntegrityPassesNormalFrames is the safety case's NO-FALSE-POSITIVE half: a
// battery of entirely legitimate frames (a fast highway drive, a DC charge SOC
// ramp, a 0->full charge, a post-tunnel GPS jump, a flat-and-full battery, a
// stopped car) must ALL pass untouched, with the rejection counters at zero.
func TestIntegrityPassesNormalFrames(t *testing.T) {
	t0 := time.UnixMilli(1_000_000)

	tests := []struct {
		name  string
		ref   *vehicle.State
		refAt time.Time
		frame vehicle.Snapshot
		// want asserts the value MERGED (was not rejected).
		want func(t *testing.T, got vehicle.Snapshot)
	}{
		{
			name:  "fast highway drive: 40 m/s (144 km/h)",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 35},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{SpeedMps: 40},
				StreamPresent: vehicle.StreamSpeed,
				FetchedAt:     t0.Add(time.Second),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 40.0, got.State.SpeedMps, "a legitimate fast speed passes")
			},
		},
		{
			name:  "track speed: 90 m/s (324 km/h, near a Plaid top speed)",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 80},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{SpeedMps: 90},
				StreamPresent: vehicle.StreamSpeed,
				FetchedAt:     t0.Add(time.Second),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 90.0, got.State.SpeedMps, "near-top-speed passes")
			},
		},
		{
			name:  "odometer advance during a fast drive: +30 km in 5 min",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, OdometerMeters: 100_000},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{OdometerMeters: 130_000},
				StreamPresent: vehicle.StreamOdometer,
				FetchedAt:     t0.Add(5 * time.Minute),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 130_000, got.State.OdometerMeters, "a normal odometer advance passes")
			},
		},
		{
			name:  "DC charge SOC ramp: 20 -> 35 in 5 min",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, BatteryLevelPct: 20},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{BatteryLevelPct: 35},
				StreamPresent: vehicle.StreamSOC,
				FetchedAt:     t0.Add(5 * time.Minute),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 35.0, got.State.BatteryLevelPct, "a DC charge ramp passes")
			},
		},
		{
			name:  "flat battery: SOC 0",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, BatteryLevelPct: 3},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{BatteryLevelPct: 0},
				StreamPresent: vehicle.StreamSOC,
				FetchedAt:     t0.Add(time.Minute),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 0.0, got.State.BatteryLevelPct, "SOC 0 (flat) passes, is not treated as out-of-range")
			},
		},
		{
			name:  "full battery: SOC 100",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, BatteryLevelPct: 98},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{BatteryLevelPct: 100},
				StreamPresent: vehicle.StreamSOC,
				FetchedAt:     t0.Add(time.Minute),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 100.0, got.State.BatteryLevelPct, "SOC 100 (full) passes")
			},
		},
		{
			name:  "stopped car: speed 0",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, SpeedMps: 12},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{SpeedMps: 0},
				StreamPresent: vehicle.StreamSpeed,
				FetchedAt:     t0.Add(time.Second),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.Equal(t, 0.0, got.State.SpeedMps, "speed 0 (stopped) passes")
			},
		},
		{
			name:  "post-tunnel GPS jump: 2 km after a 90 s GPS gap",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, Location: &vehicle.Location{Latitude: 37.7749, Longitude: -122.4194}},
			refAt: t0,
			frame: vehicle.Snapshot{
				// ~2.2 km north; over 90 s that is ~24 m/s, well under the ceiling.
				State:         &vehicle.State{Location: &vehicle.Location{Latitude: 37.7949, Longitude: -122.4194}},
				StreamPresent: vehicle.StreamLoc,
				FetchedAt:     t0.Add(90 * time.Second),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.InDelta(t, 37.7949, got.State.Location.Latitude, 1e-9, "a post-tunnel GPS jump passes")
			},
		},
		{
			name:  "normal city GPS step: 200 m in 10 s",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, Location: &vehicle.Location{Latitude: 37.7749, Longitude: -122.4194}},
			refAt: t0,
			frame: vehicle.Snapshot{
				State:         &vehicle.State{Location: &vehicle.Location{Latitude: 37.7767, Longitude: -122.4194}},
				StreamPresent: vehicle.StreamLoc,
				FetchedAt:     t0.Add(10 * time.Second),
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.InDelta(t, 37.7767, got.State.Location.Latitude, 1e-9, "a normal GPS step passes")
			},
		},
		{
			name:  "parked GPS jitter: a few meters with no time elapsed",
			ref:   &vehicle.State{Power: vehicle.PowerOnline, Location: &vehicle.Location{Latitude: 37.7749, Longitude: -122.4194}},
			refAt: t0,
			frame: vehicle.Snapshot{
				// ~5 m, same receive instant: below minTeleportMeters, never checked.
				State:         &vehicle.State{Location: &vehicle.Location{Latitude: 37.77494, Longitude: -122.4194}},
				StreamPresent: vehicle.StreamLoc,
				FetchedAt:     t0,
			},
			want: func(t *testing.T, got vehicle.Snapshot) {
				assert.InDelta(t, 37.77494, got.State.Location.Latitude, 1e-9, "parked jitter passes")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ic := gatedMerger("v1")
			seedPoll(m, tc.ref, tc.refAt)
			m.MergeStream(tc.frame, 0)
			got := m.Latest()
			for reason, n := range ic.Snapshot() {
				assert.Equal(t, int64(0), n, "no false positive: %s must not trip", reason)
			}
			tc.want(t, got)
		})
	}
}

// TestIntegrityZeroToFullChargeRamp walks a whole 0->100 DC charge as a sequence
// of stream frames and asserts not a single one is rejected: the SOC range gate
// accepts the endpoints and every step between.
func TestIntegrityZeroToFullChargeRamp(t *testing.T) {
	m, ic := gatedMerger("v1")
	t0 := time.UnixMilli(1_000_000)
	seedPoll(m, &vehicle.State{Power: vehicle.PowerOnline, BatteryLevelPct: 0}, t0)
	for soc := 0; soc <= 100; soc += 5 {
		m.MergeStream(vehicle.Snapshot{
			State:         &vehicle.State{BatteryLevelPct: float64(soc)},
			StreamPresent: vehicle.StreamSOC,
			FetchedAt:     t0.Add(time.Duration(soc) * 30 * time.Second),
		}, 0)
		assert.Equal(t, float64(soc), m.Latest().State.BatteryLevelPct, "SOC %d accepted", soc)
	}
	for reason, n := range ic.Snapshot() {
		assert.Equal(t, int64(0), n, "0->full charge must not trip %s", reason)
	}
}

// TestIntegrityRealDriveSparseLocationNoTeleport reproduces the 2026-06-21 freeze:
// during a drive, location frames arrive less often than speed frames. Anchoring the
// GPS check to the global last-refresh (the most recent speed frame, ~1 s back)
// divided a ~10 s location delta by ~1 s and manufactured a >150 m/s "teleport"; the
// rejected fix then froze the anchor so every following location was rejected too.
// Anchored to the last LOCATION instead, a legitimate 20 m/s (72 km/h) drive with 1 Hz
// speed frames and 0.1 Hz location frames passes clean and the position advances.
func TestIntegrityRealDriveSparseLocationNoTeleport(t *testing.T) {
	t0 := time.UnixMilli(1_000_000)
	m, ic := gatedMerger("v1")
	seedPoll(m, &vehicle.State{
		Power:    vehicle.PowerOnline,
		Location: &vehicle.Location{Latitude: 37.0000, Longitude: -122.0000},
	}, t0)

	const metersPerDegLat = 111_320.0
	lat := 37.0000
	for sec := 1; sec <= 60; sec++ {
		at := t0.Add(time.Duration(sec) * time.Second)
		if sec%10 == 0 {
			// A location frame every 10 s, ~200 m on from the last (20 m/s).
			lat += 200.0 / metersPerDegLat
			m.MergeStream(vehicle.Snapshot{
				State:         &vehicle.State{Location: &vehicle.Location{Latitude: lat, Longitude: -122.0000}},
				StreamPresent: vehicle.StreamLoc,
				FetchedAt:     at,
			}, 0)
			continue
		}
		// A speed-only frame every other second bumps the global AsOf but must not
		// shrink the GPS check's divisor.
		m.MergeStream(vehicle.Snapshot{
			State:         &vehicle.State{SpeedMps: 20},
			StreamPresent: vehicle.StreamSpeed,
			FetchedAt:     at,
		}, 0)
	}

	assert.Equal(t, int64(0), ic.Snapshot()[string(reasonGPSTeleport)],
		"a real drive with sparse location frames must raise no teleport")
	assert.InDelta(t, lat, m.Latest().State.Location.Latitude, 1e-9,
		"served location advanced with the drive, not frozen at the first fix")
}

// TestIntegrityFirstFrameRangeStillGated: even the first-ever frame (no prior
// poll) gates the absolute range checks; a rejected first SOC is left unknown
// (zero) rather than serving the impossible value, while a clean co-field applies.
func TestIntegrityFirstFrameRangeStillGated(t *testing.T) {
	m, ic := gatedMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{BatteryLevelPct: 250, SpeedMps: 12, Gear: vehicle.GearDrive},
		StreamPresent: vehicle.StreamSOC | vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.UnixMilli(1000),
	}, 0)
	got := m.Latest()
	require.NotNil(t, got.State)
	assert.Equal(t, int64(1), ic.Snapshot()[string(reasonSOCRange)], "first-frame SOC range rejected")
	assert.Equal(t, 0.0, got.State.BatteryLevelPct, "rejected first SOC left unknown, not served impossible")
	assert.Equal(t, 12.0, got.State.SpeedMps, "clean first-frame speed applied")
	assert.Equal(t, vehicle.GearDrive, got.State.Gear, "clean first-frame gear applied")
}

// TestIntegrityFirstFrameNoBaselineSkipsRegression: with no prior fix there is no
// baseline, so the regression/teleport checks cannot fire on the first frame (a
// large odometer or far-flung fix is the legitimate first reading).
func TestIntegrityFirstFrameNoBaselineSkipsRegression(t *testing.T) {
	m, ic := gatedMerger("v1")
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{OdometerMeters: 250_000, Location: &vehicle.Location{Latitude: 40.7128, Longitude: -74.0060}},
		StreamPresent: vehicle.StreamOdometer | vehicle.StreamLoc,
		FetchedAt:     time.UnixMilli(1000),
	}, 0)
	got := m.Latest()
	assert.Equal(t, 250_000, got.State.OdometerMeters, "first odometer reading accepted")
	for reason, n := range ic.Snapshot() {
		assert.Equal(t, int64(0), n, "first frame must not trip %s", reason)
	}
}

// TestIntegrityDisabledWhenNoCounters: a bare NewMerger (no integrity wired)
// passes every frame untouched, so a test or path that does not opt into the gate
// keeps the pre-P12 behavior.
func TestIntegrityDisabledWhenNoCounters(t *testing.T) {
	m := NewMerger("v1")
	seedPoll(m, &vehicle.State{Power: vehicle.PowerOnline, OdometerMeters: 10_000}, time.UnixMilli(1000))
	m.MergeStream(vehicle.Snapshot{
		State:         &vehicle.State{OdometerMeters: 9_000}, // would regress
		StreamPresent: vehicle.StreamOdometer,
		FetchedAt:     time.UnixMilli(2000),
	}, 0)
	assert.Equal(t, 9_000, m.Latest().State.OdometerMeters, "no gate wired: value merges unchanged")
}

// TestIntegrityCountersSnapshotSeedsAllReasons: a fresh counter set reports every
// reason at 0 (present-but-zero series) and a nil set is safe.
func TestIntegrityCountersSnapshotSeedsAllReasons(t *testing.T) {
	var nilC *integrityCounters
	for _, snap := range []map[string]int64{(&integrityCounters{}).Snapshot(), nilC.Snapshot()} {
		assert.Len(t, snap, len(integrityReasons))
		for _, r := range integrityReasons {
			v, ok := snap[string(r)]
			assert.True(t, ok, "reason %s present", r)
			assert.Equal(t, int64(0), v)
		}
	}
}
