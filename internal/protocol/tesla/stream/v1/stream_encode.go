// Package v1 is the shared data:update STREAMING SINK: it serves the legacy
// Tesla streaming WebSocket shape (data:update / data:error / control:hello) that
// stock TeslaMate consumes via TeslaApi.Stream, reading from the canonical cache.
// It is source-agnostic (the cache may be filled by Fleet Telemetry, Owner, or a
// Rivian poll) and registers ITSELF in the streamsink registry under BOTH
// stream-family keys (tesla-fleet-stream-v1 and tesla-owner-stream-v1), so it
// pairs with whichever streaming source is configured.
//
// The encode reuses the tesla-fleet-poll-v1 REST sink's ShiftState enum helper
// (same canonical->Tesla mapping, different transport) and internal/units for the
// SI->imperial conversions TeslaMate's columns expect.
package v1

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// dataColumns is the exact column order TeslaMate's TeslaApi.Stream @columns
// parses. A frame whose order does not match is silently dropped, so this MUST
// stay in lockstep with lib/tesla_api/stream.ex.
var dataColumns = []string{
	"speed", "odometer", "soc", "elevation", "est_heading", "est_lat", "est_lng",
	"power", "shift_state", "range", "est_range", "heading",
}

// numColumns is the count of data columns (excluding the timestamp prefix),
// derived from dataColumns so the two cannot drift.
var numColumns = len(dataColumns)

// EncodeDataUpdate builds the data:update JSON frame for one snapshot. tag is the
// consumer-supplied vehicle id (VIN or integer vehicle_id; the sink accepts
// both). Fields a stream source cannot supply (elevation, est_range) are emitted
// empty, matching MyTeslaMate's behavior. OdometerMeters/RangeKm are widened to
// float64 before unit conversion. Returns "" for a nil-state snapshot (the
// broadcaster drops it).
//
// This emits the FULL column set for the current snapshot: every field the cache
// knows is sent on every frame. The cache already holds last-known values, so
// re-sending unchanged columns is free and guarantees a drive boundary can never
// land on a frame missing odometer or location (a NULL there breaks TeslaMate's
// drive distance / start_drive).
func EncodeDataUpdate(tag string, snap vehicle.Snapshot) string {
	ts, cols := dataColumnsFor(snap)
	if cols == nil {
		return ""
	}
	return encodeFrame(tag, ts, cols)
}

// dataColumnsFor returns the timestamp prefix and the 12 data:update columns for
// a snapshot, or ("", nil) for a nil-state snapshot. The broadcaster sends the full
// column set every frame (the cache already holds last-known values).
func dataColumnsFor(snap vehicle.Snapshot) (string, []string) {
	st := snap.State
	if st == nil {
		return "", nil
	}
	cols := make([]string, numColumns)

	if st.SpeedMps != 0 {
		cols[0] = strconv.Itoa(int(math.Round(units.MpsToMph(st.SpeedMps))))
	}
	if st.OdometerMeters != 0 {
		cols[1] = strconv.FormatFloat(units.MetersToMiles(float64(st.OdometerMeters)), 'f', -1, 64)
	}
	if st.BatteryLevelPct != 0 {
		cols[2] = strconv.Itoa(int(math.Round(st.BatteryLevelPct)))
	}
	// cols[3] elevation: not in the canonical model; emit empty.
	if st.HeadingDeg != 0 {
		h := strconv.Itoa(int(math.Round(st.HeadingDeg)))
		cols[4] = h
		cols[11] = h
	}
	hasLoc := st.Location != nil
	if hasLoc {
		cols[5] = strconv.FormatFloat(st.Location.Latitude, 'f', -1, 64)
		cols[6] = strconv.FormatFloat(st.Location.Longitude, 'f', -1, 64)
	}
	// power: live charge kW when a session is present, else numeric 0 for any
	// genuinely-online frame (parked-awake included), blank when asleep. TeslaMate
	// reads a blank/nil power as "fake online" and refuses to fetch vehicle_data, so
	// a numeric 0 is the "real online" signal that lets a wake refresh poll-only
	// fields (SoC, charge_limit_soc, climate). Blank only when Asleep/Offline/Unknown
	// keeps TeslaMate's sleep accounting correct. Safe because the cache decouples
	// TeslaMate from the real car: TRS never wakes it, so this is data quality, not
	// battery. Still gated on hasLoc to avoid an untested power-without-fix shape.
	if snap.Live != nil && snap.Live.PowerKw != 0 {
		cols[7] = strconv.Itoa(int(math.Round(snap.Live.PowerKw)))
	} else if st.Power == vehicle.PowerOnline && hasLoc {
		cols[7] = "0"
	}
	// shift_state is gated on having a location: a "D" frame with nil est_lat/
	// est_lng makes TeslaMate's Position insert fail validate_required, which
	// crashes start_drive's hard {:ok,_}= match and abandons the drive (NULL
	// distance, fragmentation). Hold last-known shift_state (blank) until a fix
	// arrives so the gear and its location land in the same frame.
	if st.Gear != vehicle.GearUnknown && hasLoc {
		cols[8] = teslafleet.ShiftState(st.Gear)
	}
	if st.RangeKm != 0 {
		cols[9] = strconv.Itoa(int(math.Round(units.KmToMiles(float64(st.RangeKm)))))
	}
	// cols[10] est_range: not in the canonical model; emit empty.

	// Timestamp prefix: the canonical monotonic AsOf the cache stamped -- the SAME
	// value the REST vehicle_data sink emits -- so a consumer reading both surfaces
	// never sees the poll and stream clocks disagree.
	ts := strconv.FormatInt(snap.AsOfTime().UnixMilli(), 10)
	return ts, cols
}

// encodeFrame builds a data:update frame from a pre-built timestamp + column
// vector.
func encodeFrame(tag, ts string, cols []string) string {
	value := ts + "," + strings.Join(cols, ",")
	return fmt.Sprintf(`{"msg_type":"data:update","tag":%q,"value":%q}`, tag, value)
}

// EncodeError builds a data:error frame. The broadcaster sends one when the
// vehicle is offline/stale or a subscribe is rejected, matching the real Tesla
// streaming behavior TeslaMate's stream.ex handles.
func EncodeError(tag, kind, message string) string {
	return fmt.Sprintf(`{"msg_type":"data:error","tag":%q,"error_type":%q,"error":%q}`, tag, kind, message)
}

// EncodeHello builds the control:hello keepalive frame. connectionTimeoutMs is
// the keepalive interval the consumer should expect (TeslaMate uses it to detect
// a stalled connection).
func EncodeHello(connectionTimeoutMs int) string {
	return fmt.Sprintf(`{"msg_type":"control:hello","connection_timeout":%d}`, connectionTimeoutMs)
}

// helloInterval is how often the sink sends control:hello. MyTeslaMate uses 10s;
// it doubles as the consumer's liveness expectation.
const helloInterval = 10 * time.Second
