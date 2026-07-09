package cache

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fleetpoll "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	fleetstream "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/stream/v1"
	streamenc "github.com/bttnns/red-switchboard/internal/protocol/tesla/stream/v1"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/require"
	"github.com/teslamotors/fleet-telemetry/protos"
)

// TestTimestampMonotonicityConformance is the end-to-end lock on the storm + NULL
// drives class of bug. It drives a realistic INTERLEAVED poll/stream/restart
// sequence through the merge seam, and after every step pushes the SERVED snapshot
// through BOTH real sink-encode paths (the legacy data:update stream sink and the
// REST vehicle_data sink). It then asserts two invariants over the whole sequence:
//
//  1. Monotonic non-decreasing: every PAYLOAD-clock wire timestamp a consumer sees
//     (the stream data:update prefix and every REST per-object timestamp) is >= the
//     previously served one. A staler poll landing after a fresh stream frame must
//     NOT regress it; that regression is exactly what made TeslaMate write end_date
//     < start_date and crash. gps_as_of is a separate GPS fix-age clock that may
//     legitimately lag, so it is exempt from this invariant (it carries only the
//     never-zero one).
//  2. Never Go-zero: no served wire timestamp (payload clock OR gps_as_of) is
//     time.Time{}'s epoch; the NULL-drive / validate_required class of bug, where a
//     missing timestamp stamp surfaces as a zero (or epoch) time on the wire.
//
// The interleave deliberately includes a staler-poll-after-stream step and a
// simulated restart (a fresh Service over the same persisted AsOf file, resuming
// the clamp from the high-water mark). If the monotonic clamp in merge.commit is
// reverted to a plain assignment, the staler-poll and post-restart steps regress
// and fail invariant 1; if a per-field timestamp stamp is dropped, the affected
// wire field goes to the epoch and fails invariant 2.
func TestTimestampMonotonicityConformance(t *testing.T) {
	const id = "g1"
	ident := []vehicle.Identity{{ID: id, VIN: "VIN1"}}
	asofPath := filepath.Join(t.TempDir(), "asof.json")
	ctx := context.Background()

	svc, err := NewService(ident, time.Hour, asofPath, "", 0, nil)
	require.NoError(t, err)

	// Anchor at a fixed wall-clock instant so the interleave is deterministic and
	// the staleAfter degradation does not fire (all frames are recent relative to
	// each other; absolute age vs time.Now does not matter for the wire timestamps,
	// which derive from the snapshot's own clamped AsOf). Truncate to a whole second
	// and step by whole seconds: gps_as_of is emitted at SECOND resolution
	// (Location.TimeStamp.UnixMilli()/1000) while AsOf is ms, so sub-second offsets
	// would manufacture a spurious cross-resolution "regression" that is a test
	// artifact, not a clamp failure.
	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	at := func(deltaSec int64) time.Time { return base.Add(time.Duration(deltaSec) * time.Second) }

	mon := newWireMonotonicity(t)

	// poll merges a poll frame (with its own FetchedAt) and checks the served wire.
	poll := func(s *Service, fetchedAt time.Time, st *vehicle.State, live *vehicle.LiveSession) {
		s.MergePoll(id, vehicle.Snapshot{State: st, Live: live, FetchedAt: fetchedAt})
		mon.check(t, s.Latest(id))
	}
	// stream merges a stream frame (delta of present fields) and checks the wire.
	stream := func(s *Service, fetchedAt time.Time, st *vehicle.State, present vehicle.StreamField, live *vehicle.LiveSession) {
		require.NoError(t, s.Put(ctx, id, vehicle.Snapshot{
			State: st, Live: live, StreamPresent: present, FetchedAt: fetchedAt,
		}))
		mon.check(t, s.Latest(id))
	}

	loc := func(lat, lng float64, ts time.Time) *vehicle.Location {
		return &vehicle.Location{Latitude: lat, Longitude: lng, TimeStamp: ts}
	}

	// 1. Poll establishes a parked, located vehicle.
	poll(svc, at(1000), &vehicle.State{
		Power: vehicle.PowerOnline, Gear: vehicle.GearPark,
		Location: loc(37.1, -122.1, at(1000)), RangeKm: 400, BatteryLevelPct: 80, CabinTempC: 21,
	}, nil)

	// 2. Stream frame advances time: the car starts driving (gear + location in the
	// same frame, per the P1/P2 invariants).
	stream(svc, at(2000), &vehicle.State{
		Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 20,
		Location: loc(37.2, -122.2, at(2000)),
	}, vehicle.StreamGear|vehicle.StreamSpeed|vehicle.StreamLoc, nil)

	// 3. A STALER poll lands after the fresh stream frame (the classic race: a poll
	// in flight before the stream frame returns late). Its FetchedAt is BELOW the
	// last served time; the clamp must hold the wire timestamp, not regress it.
	poll(svc, at(1500), &vehicle.State{
		Power: vehicle.PowerOnline, Gear: vehicle.GearPark,
		Location: loc(37.05, -122.05, at(1500)), RangeKm: 399, BatteryLevelPct: 80,
	}, nil)

	// 4. Another stream frame, time advancing again.
	stream(svc, at(3000), &vehicle.State{
		Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 25,
		Location: loc(37.3, -122.3, at(3000)),
	}, vehicle.StreamGear|vehicle.StreamSpeed|vehicle.StreamLoc, nil)

	// 5. A fresher poll catches up and advances.
	poll(svc, at(4000), &vehicle.State{
		Power: vehicle.PowerOnline, Gear: vehicle.GearDrive,
		Location: loc(37.35, -122.35, at(4000)), RangeKm: 395, BatteryLevelPct: 79,
	}, nil)

	// Persist the high-water mark, then SIMULATE A RESTART: a fresh Service over the
	// same file resumes the clamp from the persisted AsOf (the P3 seam).
	svc.FlushAsOf()
	svc2, err := NewService(ident, time.Hour, asofPath, "", 0, nil)
	require.NoError(t, err)

	// 6. After the restart, a poll OLDER than the persisted mark arrives (a poll that
	// was behind the stream just before the restart). The clamp must resume at the
	// high-water mark, so the served wire timestamp must NOT regress below step 5.
	poll(svc2, at(2500), &vehicle.State{
		Power: vehicle.PowerOnline, Gear: vehicle.GearDrive,
		Location: loc(37.36, -122.36, at(2500)), RangeKm: 393, BatteryLevelPct: 79,
	}, nil)

	// 7. A post-restart frame driven through the REAL Fleet Telemetry decoder, so the
	// test exercises the full decode -> merge -> sink-encode path on actual wire
	// shape (a protobuf Payload), not just hand-built snapshots. The decoder stamps
	// FetchedAt and Location.TimeStamp at receive time (time.Now), which is after the
	// base anchor (now - 1min), so it advances monotonically. If the P2 stamp is
	// dropped (decoder forgets Location.TimeStamp), gps_as_of goes to the epoch and
	// invariant 2 fires.
	decoded := fleetstream.DecodePayload(&protos.Payload{Data: []*protos.Datum{
		{Key: protos.Field_Gear, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: "D"}}},
		{Key: protos.Field_VehicleSpeed, Value: &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: 40}}},
		{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_LocationValue{
			LocationValue: &protos.LocationValue{Latitude: 37.4, Longitude: -122.4},
		}}},
	}})
	require.NoError(t, svc2.Put(ctx, id, decoded))
	mon.check(t, svc2.Latest(id))
}

// wireMonotonicity tracks the highest wire timestamp served so far and asserts the
// two invariants on each served snapshot, exercising the REAL sink-encode paths.
type wireMonotonicity struct {
	highMs   int64             // highest payload-clock wire timestamp (ms) served so far
	hasHigh  bool              // whether highMs has been seeded yet
	prevREST *wire.VehicleData // hold-last-known prev for the REST sink
}

func newWireMonotonicity(t *testing.T) *wireMonotonicity {
	t.Helper()
	return &wireMonotonicity{}
}

var (
	testIDs = fleetpoll.IDs{ID: 42, VehicleID: 42, VIN: "VIN1", DisplayName: "Tester"}
	testCfg = fleetpoll.Cfg{CarType: "model3", Model: "model3"}
)

// check pushes snap through both sink-encode paths, extracts every wire timestamp,
// and asserts invariant 1 (monotonic non-decreasing vs the running high-water mark,
// for the PAYLOAD-clock stamps) and invariant 2 (never the epoch / zero time, for
// every stamp).
func (w *wireMonotonicity) check(t *testing.T, snap vehicle.Snapshot) {
	t.Helper()

	var stamps []wireStamp

	// Stream sink: the data:update frame prefix is the canonical AsOf in ms (a
	// payload-clock stamp the monotonic clamp governs).
	if frame := streamenc.EncodeDataUpdate("VIN1", snap); frame != "" {
		stamps = append(stamps, wireStamp{name: "stream data:update prefix", ms: streamFramePrefixMs(t, frame), monotonic: true})
	}

	// REST sink: every per-object Timestamp derives from the served AsOf (payload
	// clock); gps_as_of is the GPS FIX age, a separate clock that legitimately lags
	// the payload (a stale fix on a fresh payload is normal, e.g. the first poll
	// after a restart repopulates an older location while AsOf holds the high-water
	// mark). So gps_as_of carries only invariant 2, not invariant 1.
	rest := fleetpoll.VehicleData(w.prevREST, snap.State, snap.Live, testIDs, testCfg, snap.AsOfTime(), nil)
	w.prevREST = &rest
	stamps = append(stamps, restStamps(rest)...)

	require.NotEmpty(t, stamps, "a served snapshot must emit at least one wire timestamp")

	for _, s := range stamps {
		// Invariant 2: never Go-zero. time.Time{}.UnixMilli() is a large negative
		// epoch offset; gps_as_of is in SECONDS, so a zero time there is also far
		// below any real instant. Both are caught by requiring a plausibly-recent
		// positive value (after 2001-09-09, Unix second 1e9), which a zero/epoch
		// timestamp never satisfies.
		require.Greaterf(t, s.ms, int64(1_000_000_000_000), // 2001 in ms
			"invariant 2 (never-zero): wire field %q served a zero/epoch timestamp (%d)", s.name, s.ms)

		if !s.monotonic {
			continue
		}
		// Invariant 1: monotonic non-decreasing across the whole sequence.
		if w.hasHigh {
			require.GreaterOrEqualf(t, s.ms, w.highMs,
				"invariant 1 (monotonic): wire field %q regressed: served %d < high-water %d", s.name, s.ms, w.highMs)
		}
		if !w.hasHigh || s.ms > w.highMs {
			w.highMs = s.ms
			w.hasHigh = true
		}
	}
}

type wireStamp struct {
	name      string
	ms        int64 // normalized to milliseconds
	monotonic bool  // true for payload-clock stamps the clamp governs (invariant 1)
}

// streamFramePrefixMs parses the timestamp prefix (the first comma-separated field
// of the "value" string) out of a data:update frame.
func streamFramePrefixMs(t *testing.T, frame string) int64 {
	t.Helper()
	// frame is {"msg_type":"data:update","tag":"...","value":"<ts>,<col>,..."}.
	i := strings.Index(frame, `"value":"`)
	require.GreaterOrEqual(t, i, 0, "frame must carry a value")
	rest := frame[i+len(`"value":"`):]
	tsStr := rest[:strings.IndexByte(rest, ',')]
	ms, err := strconv.ParseInt(tsStr, 10, 64)
	require.NoError(t, err, "frame prefix must be an integer ms timestamp")
	return ms
}

// restStamps collects every wire timestamp from a REST vehicle_data payload. The
// per-object Timestamp fields are in ms and are payload-clock stamps (monotonic);
// gps_as_of is in SECONDS (normalized to ms) and is the GPS fix-age clock, checked
// for never-zero only.
func restStamps(vd wire.VehicleData) []wireStamp {
	var out []wireStamp
	add := func(name string, ms *int64) {
		if ms != nil {
			out = append(out, wireStamp{name: name, ms: *ms, monotonic: true})
		}
	}
	if vd.DriveState != nil {
		add("drive_state.timestamp", vd.DriveState.Timestamp)
		if vd.DriveState.GpsAsOf != nil {
			out = append(out, wireStamp{name: "drive_state.gps_as_of", ms: *vd.DriveState.GpsAsOf * 1000})
		}
	}
	if vd.ChargeState != nil {
		add("charge_state.timestamp", vd.ChargeState.Timestamp)
	}
	if vd.ClimateState != nil {
		add("climate_state.timestamp", vd.ClimateState.Timestamp)
	}
	if vd.VehicleState != nil {
		add("vehicle_state.timestamp", vd.VehicleState.Timestamp)
	}
	if vd.VehicleConfig != nil {
		add("vehicle_config.timestamp", vd.VehicleConfig.Timestamp)
	}
	return out
}
