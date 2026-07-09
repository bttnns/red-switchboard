package v1

import (
	"strings"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// TestEncodeDataUpdateColumnOrder asserts the value column order matches
// TeslaApi.Stream.@columns exactly: timestamp,speed,odometer,soc,elevation,
// est_heading,est_lat,est_lng,power,shift_state,range,est_range,heading. A frame
// whose order differs is silently dropped by TeslaMate, so this is load-bearing.
func TestEncodeDataUpdateColumnOrder(t *testing.T) {
	now := time.UnixMilli(1700000000000)
	snap := vehicle.Snapshot{
		State: &vehicle.State{
			Power:           vehicle.PowerOnline,
			Gear:            vehicle.GearDrive,
			SpeedMps:        20, // 20 m/s -> ~45 mph
			HeadingDeg:      90,
			OdometerMeters:  16093, // 10 miles
			BatteryLevelPct: 72,
			RangeKm:         400, // ~249 miles
			Location:        &vehicle.Location{Latitude: 37.5, Longitude: -122.2},
		},
		FetchedAt: now,
	}
	got := EncodeDataUpdate("VIN123", snap)
	if got == "" {
		t.Fatal("expected a frame, got empty")
	}
	// The value field is a comma-joined string prefixed by the timestamp.
	valueStart := strings.Index(got, `"value":"`) + len(`"value":"`)
	valueEnd := strings.Index(got[valueStart:], `"`)
	value := got[valueStart : valueStart+valueEnd]
	cols := strings.Split(value, ",")
	// timestamp + 12 data columns = 13 fields.
	if len(cols) != 13 {
		t.Fatalf("expected 13 fields (ts+12 cols), got %d: %q", len(cols), value)
	}
	if cols[0] != "1700000000000" {
		t.Errorf("timestamp col = %q, want 1700000000000", cols[0])
	}
	// elevation (idx 4) and est_range (idx 11) are unsupported -> empty.
	if cols[4] != "" {
		t.Errorf("elevation col = %q, want empty (unsupported)", cols[4])
	}
	if cols[11] != "" {
		t.Errorf("est_range col = %q, want empty (unsupported)", cols[11])
	}
	// est_heading (idx 5) and heading (idx 12) both carry the heading.
	if cols[5] != "90" || cols[12] != "90" {
		t.Errorf("heading cols = %q/%q, want 90/90", cols[5], cols[12])
	}
	// shift_state (idx 9) is D for GearDrive.
	if cols[9] != "D" {
		t.Errorf("shift_state col = %q, want D", cols[9])
	}
}

// TestEncodeDataUpdateUnitConversions: speed m/s->mph, odometer m->mi, range
// km->mi, all integer-rounded as TeslaMate expects.
func TestEncodeDataUpdateUnitConversions(t *testing.T) {
	snap := vehicle.Snapshot{
		State: &vehicle.State{
			SpeedMps:       10,      // 22.37 mph -> 22
			OdometerMeters: 1609344, // 1000 miles exactly
			RangeKm:        200,     // 124.27 mi -> 124
		},
		FetchedAt: time.UnixMilli(1),
	}
	got := EncodeDataUpdate("t", snap)
	valueStart := strings.Index(got, `"value":"`) + len(`"value":"`)
	valueEnd := strings.Index(got[valueStart:], `"`)
	cols := strings.Split(got[valueStart:valueStart+valueEnd], ",")
	if cols[1] != "22" {
		t.Errorf("speed = %q, want 22", cols[1])
	}
	if cols[2] != "1000" {
		t.Errorf("odometer = %q, want 1000", cols[2])
	}
	if cols[10] != "124" {
		t.Errorf("range = %q, want 124", cols[10])
	}
}

// TestEncodeDataUpdateEmptyHandling: a zero-ish state (only Power set, no live
// fields, no location) emits empty data columns, NOT misleading zeros. TeslaMate
// treats "" as nil (hold-last-known), so a parked/idle frame does not reset a
// drive. shift_state is blank here too: it is gated on having a location.
func TestEncodeDataUpdateEmptyHandling(t *testing.T) {
	snap := vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark},
		FetchedAt: time.UnixMilli(1),
	}
	got := EncodeDataUpdate("t", snap)
	valueStart := strings.Index(got, `"value":"`) + len(`"value":"`)
	valueEnd := strings.Index(got[valueStart:], `"`)
	cols := strings.Split(got[valueStart:valueStart+valueEnd], ",")
	// All data columns empty: no location means no shift_state either.
	for i, want := range []string{"", "", "", "", "", "", "", "", "", "", "", ""} {
		if cols[i+1] != want {
			t.Errorf("col[%d] = %q, want %q", i+1, cols[i+1], want)
		}
	}
}

// TestEncodeDataUpdateShiftStateGatedOnLocation is the P1 crash-fix invariant: a
// gear-before-location frame (gear set, est_lat/est_lng nil) must NEVER emit
// shift_state. A "D" with nil lat/lng makes TeslaMate's Position insert fail
// validate_required, crashing start_drive's hard {:ok,_}= match (NULL-distance
// drives + fragmentation). The gear is held back (blank) until its location lands.
func TestEncodeDataUpdateShiftStateGatedOnLocation(t *testing.T) {
	const (
		colPower = 8 // power column index in the comma-split value (ts at 0)
		colShift = 9 // shift_state column index
		colLat   = 6 // est_lat
		colLng   = 7 // est_lng
	)
	cases := []struct {
		name      string
		gear      vehicle.Gear
		loc       *vehicle.Location
		wantShift string
		wantPower string
	}{
		{"drive no location", vehicle.GearDrive, nil, "", ""},
		{"reverse no location", vehicle.GearReverse, nil, "", ""},
		{"neutral no location", vehicle.GearNeutral, nil, "", ""},
		{"park no location", vehicle.GearPark, nil, "", ""},
		{"drive with location", vehicle.GearDrive, &vehicle.Location{Latitude: 37.5, Longitude: -122.2}, "D", "0"},
		// P8c: online+parked with a location now emits power 0 (real-online signal).
		{"park with location", vehicle.GearPark, &vehicle.Location{Latitude: 37.5, Longitude: -122.2}, "P", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := vehicle.Snapshot{
				State: &vehicle.State{
					Power:    vehicle.PowerOnline,
					Gear:     tc.gear,
					Location: tc.loc,
				},
				FetchedAt: time.UnixMilli(1),
			}
			got := EncodeDataUpdate("t", snap)
			valueStart := strings.Index(got, `"value":"`) + len(`"value":"`)
			valueEnd := strings.Index(got[valueStart:], `"`)
			cols := strings.Split(got[valueStart:valueStart+valueEnd], ",")
			if cols[colShift] != tc.wantShift {
				t.Errorf("shift_state = %q, want %q", cols[colShift], tc.wantShift)
			}
			if cols[colPower] != tc.wantPower {
				t.Errorf("power = %q, want %q", cols[colPower], tc.wantPower)
			}
			// Hard invariant: shift_state and lat/lng are never split across the
			// us-vs-them boundary. A non-empty shift_state requires both coords.
			if cols[colShift] != "" && (cols[colLat] == "" || cols[colLng] == "") {
				t.Errorf("shift_state %q emitted with nil lat/lng (lat=%q lng=%q): the crash condition",
					cols[colShift], cols[colLat], cols[colLng])
			}
		})
	}
}

// TestEncodeDataUpdatePowerRealOnline is the P8c invariant: the power column is a
// "real online" signal for TeslaMate. A numeric 0 (or live charge kW) tells
// TeslaMate the car is genuinely awake so it refetches vehicle_data and refreshes
// poll-only fields (SoC, charge_limit_soc, climate) on a wake; a blank power reads
// as "fake online" and is emitted only when the car is Asleep/Offline/Unknown, so
// sleep accounting stays correct. Online is gated on hasLoc to avoid an untested
// power-without-fix wire shape.
func TestEncodeDataUpdatePowerRealOnline(t *testing.T) {
	const colPower = 8 // power column index in the comma-split value (ts at 0)
	loc := &vehicle.Location{Latitude: 37.5, Longitude: -122.2}
	cases := []struct {
		name      string
		power     vehicle.Power
		gear      vehicle.Gear
		loc       *vehicle.Location
		live      *vehicle.LiveSession
		wantPower string
	}{
		// New P8c behavior: parked-but-awake now reads real-online (was blank).
		{"online parked with location", vehicle.PowerOnline, vehicle.GearPark, loc, nil, "0"},
		// Unchanged: a driving frame already emitted 0.
		{"online driving with location", vehicle.PowerOnline, vehicle.GearDrive, loc, nil, "0"},
		// Unchanged: an active charge session emits the rounded live kW.
		{"active charge session", vehicle.PowerOnline, vehicle.GearPark, loc, &vehicle.LiveSession{PowerKw: 11.4}, "11"},
		// Sleep preserved: asleep stays blank so TeslaMate records sleep.
		{"asleep", vehicle.PowerSleep, vehicle.GearPark, loc, nil, ""},
		{"offline", vehicle.PowerOffline, vehicle.GearPark, loc, nil, ""},
		{"unknown", vehicle.PowerUnknown, vehicle.GearPark, loc, nil, ""},
		// Still gated on hasLoc: an online frame without a fix holds blank.
		{"online parked no location", vehicle.PowerOnline, vehicle.GearPark, nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := vehicle.Snapshot{
				State: &vehicle.State{
					Power:    tc.power,
					Gear:     tc.gear,
					Location: tc.loc,
				},
				Live:      tc.live,
				FetchedAt: time.UnixMilli(1),
			}
			got := EncodeDataUpdate("t", snap)
			valueStart := strings.Index(got, `"value":"`) + len(`"value":"`)
			valueEnd := strings.Index(got[valueStart:], `"`)
			cols := strings.Split(got[valueStart:valueStart+valueEnd], ",")
			if cols[colPower] != tc.wantPower {
				t.Errorf("power = %q, want %q", cols[colPower], tc.wantPower)
			}
		})
	}
}

// TestEncodeDataUpdateNilState returns empty so the broadcaster can drop it.
func TestEncodeDataUpdateNilState(t *testing.T) {
	if got := EncodeDataUpdate("t", vehicle.Snapshot{}); got != "" {
		t.Errorf("expected empty frame for nil state, got %q", got)
	}
}

// TestEncodeDataUpdateStreamTimestamp: when StreamFields is fresher than
// FetchedAt, the frame timestamp is the stream time (the frame was built from
// stream-merged content).
func TestEncodeDataUpdateStreamTimestamp(t *testing.T) {
	poll := time.UnixMilli(1000)
	stream := time.UnixMilli(5000)
	snap := vehicle.Snapshot{
		State:        &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark},
		FetchedAt:    poll,
		StreamFields: stream,
	}
	got := EncodeDataUpdate("t", snap)
	valueStart := strings.Index(got, `"value":"`) + len(`"value":"`)
	valueEnd := strings.Index(got[valueStart:], `"`)
	cols := strings.Split(got[valueStart:valueStart+valueEnd], ",")
	if cols[0] != "5000" {
		t.Errorf("timestamp = %q, want stream-time 5000", cols[0])
	}
}

// TestEncodeErrorFrame / TestEncodeHelloFrame: the non-data frames the handshake
// and offline path use.
func TestEncodeErrorFrame(t *testing.T) {
	got := EncodeError("VIN", "vehicle_error", "Vehicle is offline")
	if !strings.Contains(got, `"msg_type":"data:error"`) || !strings.Contains(got, `"Vehicle is offline"`) {
		t.Errorf("unexpected error frame: %s", got)
	}
}

func TestEncodeHelloFrame(t *testing.T) {
	got := EncodeHello(30000)
	if !strings.Contains(got, `"msg_type":"control:hello"`) || !strings.Contains(got, `"connection_timeout":30000`) {
		t.Errorf("unexpected hello frame: %s", got)
	}
}
