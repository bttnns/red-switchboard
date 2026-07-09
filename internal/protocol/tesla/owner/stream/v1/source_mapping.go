package v1

// source_mapping.go is the Owner streaming -> canonical half: the INVERSE of the
// data:update sink encode (internal/protocol/tesla/stream/v1/stream_encode.go).
// The Owner streaming API pushes data:update frames whose value is a CSV in the
// exact column order TeslaMate subscribes with, so this decoder parses that CSV
// and maps the columns back into vehicle.Snapshot with imperial->SI conversions
// (the inverse of the encode's SI->imperial). Only the columns a frame carries
// (non-empty) are marked present; the cache merges field-by-field so a frame
// that blanked a column cannot clobber the last-known value.
//
// The column order MUST stay in lockstep with the sink's dataColumns. It is not
// shared as a constant (the sink and source are sibling packages); instead the
// round-trip test in source_mapping_test.go encodes a snapshot with the sink's
// exported EncodeDataUpdate and decodes it here, pinning the contract.

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// ownerColumns is the value CSV column order, matching TeslaMate's
// TeslaApi.Stream @columns and the sink's dataColumns. Index 0 is the timestamp
// prefix; the rest are the 12 data columns.
var ownerColumns = []string{
	"speed", "odometer", "soc", "elevation", "est_heading", "est_lat", "est_lng",
	"power", "shift_state", "range", "est_range", "heading",
}

// column index constants (1-based into the value CSV, after the timestamp).
const (
	colSpeed = iota + 1
	colOdometer
	colSOC
	colElevation
	colEstHeading
	colEstLat
	colEstLng
	colPower
	colShiftState
	colRange
	colEstRange
	colHeading
)

// dataUpdateFrame is the subset of the data:update frame the source decodes.
type dataUpdateFrame struct {
	MsgType string `json:"msg_type"`
	Tag     string `json:"tag"`
	Value   string `json:"value"`
}

// DecodeDataUpdate parses a data:update frame into the tag it carries and a
// canonical snapshot. An empty column means "unchanged this frame" and is left
// at its zero value (the presence-aware merge holds the last-known value). A
// frame whose value has no populated column returns a zero StreamPresent (the
// caller drops it before the cache). Non-data:update frames return ok=false.
func DecodeDataUpdate(frame string) (tag string, snap vehicle.Snapshot, ok bool) {
	var f dataUpdateFrame
	if err := json.Unmarshal([]byte(frame), &f); err != nil {
		return "", vehicle.Snapshot{}, false
	}
	if f.MsgType != "data:update" {
		return "", vehicle.Snapshot{}, false
	}
	parts := strings.Split(f.Value, ",")
	if len(parts) < 1 {
		return f.Tag, vehicle.Snapshot{}, true
	}

	st := &vehicle.State{}
	var live *vehicle.LiveSession
	var present vehicle.StreamField

	// Timestamp prefix (ms epoch). Carries the stream frame time for StreamFields
	// AND the decoded Location's TimeStamp; fall back to now when the prefix is
	// junk so the frame time is never the dishonest Go-zero.
	now := time.Now()
	ts := now
	if ms, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64); err == nil && ms > 0 {
		ts = time.UnixMilli(ms)
	}

	if v := field(parts, colSpeed); v != "" {
		if mph, err := strconv.ParseFloat(v, 64); err == nil {
			st.SpeedMps = units.MphToMps(mph)
			present |= vehicle.StreamSpeed
		}
	}
	if v := field(parts, colOdometer); v != "" {
		if mi, err := strconv.ParseFloat(v, 64); err == nil {
			st.OdometerMeters = int(math.Round(units.MilesToMeters(mi)))
			present |= vehicle.StreamOdometer
		}
	}
	if v := field(parts, colSOC); v != "" {
		if pct, err := strconv.ParseFloat(v, 64); err == nil {
			st.BatteryLevelPct = pct
			present |= vehicle.StreamSOC
		}
	}
	// est_heading and heading carry the same value (the encode writes both).
	// Prefer heading; fall back to est_heading.
	if v := field(parts, colHeading); v != "" {
		if h, err := strconv.ParseFloat(v, 64); err == nil {
			st.HeadingDeg = h
			present |= vehicle.StreamHeading
		}
	} else if v := field(parts, colEstHeading); v != "" {
		if h, err := strconv.ParseFloat(v, 64); err == nil {
			st.HeadingDeg = h
			present |= vehicle.StreamHeading
		}
	}
	latStr, lngStr := field(parts, colEstLat), field(parts, colEstLng)
	if latStr != "" && lngStr != "" {
		lat, err1 := strconv.ParseFloat(latStr, 64)
		lng, err2 := strconv.ParseFloat(lngStr, 64)
		if err1 == nil && err2 == nil {
			st.Location = &vehicle.Location{Latitude: lat, Longitude: lng, TimeStamp: ts}
			present |= vehicle.StreamLoc
		}
	}
	if v := field(parts, colPower); v != "" {
		// The Owner stream's power column is drive motor power (negative while
		// driving) or charge power (positive while charging). The canonical
		// model has a live-charge-session slot only, so decode positive power
		// to Live.PowerKw and drop negative drive power (no canonical home);
		// this matches the sink encode, which emits Live.PowerKw, so a charge
		// frame round-trips and a drive frame simply omits power.
		if kw, err := strconv.ParseFloat(v, 64); err == nil && kw > 0 {
			live = &vehicle.LiveSession{PowerKw: kw}
			present |= vehicle.StreamChargePower
		}
	}
	if v := field(parts, colShiftState); v != "" {
		st.Gear = teslafleet.GearFromTesla(v)
		if st.Gear != vehicle.GearUnknown {
			present |= vehicle.StreamGear
		}
	}
	if v := field(parts, colRange); v != "" {
		if mi, err := strconv.ParseFloat(v, 64); err == nil {
			st.RangeKm = int(math.Round(units.MilesToKm(mi)))
			present |= vehicle.StreamRange
		}
	}
	// elevation / est_range: not in the canonical model.

	return f.Tag, vehicle.Snapshot{
		State:         st,
		Live:          live,
		FetchedAt:     now,
		StreamFields:  ts,
		StreamPresent: present,
	}, true
}

// field returns the trimmed CSV cell at 1-based index i, or "" if absent. parts
// is the value split on ","; index 0 is the timestamp.
func field(parts []string, i int) string {
	if i < 0 || i >= len(parts) {
		return ""
	}
	return strings.TrimSpace(parts[i])
}
