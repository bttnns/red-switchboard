package cache

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// integrity.go cross-checks a STREAM frame's numerics against the last known
// (poll- or stream-derived) values before the merge adopts them. The
// authenticated poll is the trusted reference; a stream frame is lower-trust
// (the listener has no per-vehicle CA yet, that is P10), so a value that is
// physically impossible or a clear regression is rejected FIELD-BY-FIELD: the
// gate clears that field's presence bit so copyStreamed holds the last-known
// value, while the rest of the frame merges normally. Every rejection bumps a
// reason-labeled counter and logs one WARN line.
//
// Thresholds are deliberately GENEROUS: the gate only catches the impossible,
// never the merely surprising. A legitimate fast highway drive, a DC charge SOC
// ramp, and a post-tunnel GPS jump must all pass untouched. The safety case (no
// false positives) is proven by the NORMAL-frame battery in integrity_test.go.

const (
	// maxSpeedMps caps plausible vehicle speed. 150 m/s is 540 km/h, far above any
	// production EV (a Plaid tops out near 322 km/h), so a real drive never trips it
	// while a corrupt sentinel (e.g. a huge Int decoded as speed) does.
	maxSpeedMps = 150.0

	// maxGroundSpeedMps bounds the velocity implied by an odometer or GPS delta over
	// elapsed time. Same 150 m/s ceiling as instantaneous speed: a genuine fast drive
	// stays well under it, but a teleport or an odometer that leaps kilometers in a
	// second does not.
	maxGroundSpeedMps = 150.0

	// odometerSlackMeters tolerates rounding when comparing odometer readings (the
	// decoder rounds miles->meters). Only a regression BEYOND this slack is a real
	// backwards step; a sub-meter wobble is noise, not a rollback.
	odometerSlackMeters = 2

	// minElapsed floors the elapsed time used as the divisor in the implied-velocity
	// checks. Two frames can share a receive timestamp (sub-millisecond apart), and
	// dividing a real delta by ~0 would manufacture an infinite velocity and reject a
	// good frame. One second is below the live-drive frame cadence yet large enough to
	// keep the divisor sane.
	minElapsed = time.Second

	// minTeleportMeters is the smallest GPS move worth velocity-checking. GPS fixes
	// jitter a few meters at rest; only a move larger than this is checked against the
	// velocity ceiling, so parked-car jitter never trips the teleport guard.
	minTeleportMeters = 100.0
)

// integrityReason labels a rejected stream field. Bounded cardinality: these four
// constants are the only label values the counter ever emits.
type integrityReason string

const (
	reasonOdometerRegress integrityReason = "odometer_regress"
	reasonGPSTeleport     integrityReason = "gps_teleport"
	reasonSOCRange        integrityReason = "soc_range"
	reasonSpeedRange      integrityReason = "speed_range"
)

// integrityReasons is the fixed reason set, used to seed the counter snapshot so a
// never-tripped reason still reports 0 (a present-but-zero series, not a missing
// one).
var integrityReasons = []integrityReason{
	reasonOdometerRegress, reasonGPSTeleport, reasonSOCRange, reasonSpeedRange,
}

// integrityCounters is the shared, reason-labeled rejection counter set. One
// instance is owned by the Service and shared across every per-vehicle Merger
// (the label is the reason, never a VIN/IP, so cardinality stays bounded). The
// metrics collector reads Snapshot() at scrape time, keeping the cache
// Prometheus-free like the rest of internal/cache.
type integrityCounters struct {
	odometerRegress atomic.Int64
	gpsTeleport     atomic.Int64
	socRange        atomic.Int64
	speedRange      atomic.Int64
}

func (c *integrityCounters) inc(r integrityReason) {
	switch r {
	case reasonOdometerRegress:
		c.odometerRegress.Add(1)
	case reasonGPSTeleport:
		c.gpsTeleport.Add(1)
	case reasonSOCRange:
		c.socRange.Add(1)
	case reasonSpeedRange:
		c.speedRange.Add(1)
	}
}

// Snapshot returns the current per-reason rejection totals, every reason present
// (0 if never tripped). nil receiver yields a zeroed map so a non-streaming serve
// path can wire the collector unconditionally.
func (c *integrityCounters) Snapshot() map[string]int64 {
	out := make(map[string]int64, len(integrityReasons))
	for _, r := range integrityReasons {
		out[string(r)] = 0
	}
	if c == nil {
		return out
	}
	out[string(reasonOdometerRegress)] = c.odometerRegress.Load()
	out[string(reasonGPSTeleport)] = c.gpsTeleport.Load()
	out[string(reasonSOCRange)] = c.socRange.Load()
	out[string(reasonSpeedRange)] = c.speedRange.Load()
	return out
}

// gateStream cross-checks the streamed numerics in next against the last-known
// values in prev and returns the presence bitmask with the bit of every rejected
// field cleared, plus the reasons for each rejection. prev is the trusted
// reference (last served State). The implied-velocity checks divide a field's
// delta by the time since THAT field was last set: gpsAt for the GPS move, odoAt
// for the odometer step. Anchoring each check to its own field's time (not a
// global last-refresh) is load-bearing: location/odometer update less often than
// speed, so dividing a multi-second location delta by the ~1 s gap since the last
// (speed) frame manufactures an impossible velocity and false-rejects a real
// drive -- and once one fix is wrongly held, the frozen anchor rejects every
// frame after it. A nil prev (first frame) or absent field is never rejected.
func gateStream(prev, next *vehicle.State, present vehicle.StreamField, gpsAt, odoAt, now time.Time) (vehicle.StreamField, []integrityReason) {
	if next == nil {
		return present, nil
	}
	var reasons []integrityReason
	reject := func(bit vehicle.StreamField, r integrityReason) {
		present &^= bit
		reasons = append(reasons, r)
	}

	// SOC must be a real percentage. A frame outside [0,100] is corrupt, never a
	// legitimate reading (0 and 100 themselves pass: a flat battery and a full one).
	if present&vehicle.StreamSOC != 0 && (next.BatteryLevelPct < 0 || next.BatteryLevelPct > 100) {
		reject(vehicle.StreamSOC, reasonSOCRange)
	}

	// Speed beyond the ceiling is physically impossible for the car (a corrupt
	// decode, not a fast drive).
	if present&vehicle.StreamSpeed != 0 && (next.SpeedMps < 0 || next.SpeedMps > maxSpeedMps) {
		reject(vehicle.StreamSpeed, reasonSpeedRange)
	}

	// Without a prior fix there is nothing to regress against; the implied-velocity
	// checks below all need prev.
	if prev == nil {
		return present, reasons
	}

	// Odometer: reject a backwards step (beyond rounding slack) or an impossible
	// forward leap for the time since the last odometer reading. A monotonic,
	// plausibly-paced advance passes.
	if present&vehicle.StreamOdometer != 0 && prev.OdometerMeters > 0 {
		delta := next.OdometerMeters - prev.OdometerMeters
		if delta < -odometerSlackMeters {
			reject(vehicle.StreamOdometer, reasonOdometerRegress)
		} else if float64(delta)/elapsedSince(odoAt, now) > maxGroundSpeedMps {
			reject(vehicle.StreamOdometer, reasonOdometerRegress)
		}
	}

	// GPS teleport: a move large enough to matter implying a velocity above the
	// ceiling is a bad fix, not a real position. Small moves (parked jitter, a normal
	// drive step) are below minTeleportMeters and pass untouched.
	if present&vehicle.StreamLoc != 0 && next.Location != nil && prev.Location != nil {
		dist := haversineMeters(prev.Location.Latitude, prev.Location.Longitude, next.Location.Latitude, next.Location.Longitude)
		if dist > minTeleportMeters && dist/elapsedSince(gpsAt, now) > maxGroundSpeedMps {
			reject(vehicle.StreamLoc, reasonGPSTeleport)
		}
	}

	return present, reasons
}

// elapsedSince returns now-since in seconds, floored at minElapsed so two frames
// sharing a sub-millisecond receive instant cannot divide a real delta by ~0 and
// manufacture an infinite velocity. since is the time the compared field was last
// set; a zero since (no baseline yet) yields a large window that never rejects.
func elapsedSince(since, now time.Time) float64 {
	e := now.Sub(since)
	if e < minElapsed {
		e = minElapsed
	}
	return e.Seconds()
}

// haversineMeters returns the great-circle distance in meters between two
// lat/lng points. Used only to bound the velocity a GPS jump implies.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusM * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
