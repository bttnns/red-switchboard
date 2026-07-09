package v1

import (
	"math"
	"testing"
	"time"

	streamsink "github.com/bttnns/red-switchboard/internal/protocol/tesla/stream/v1"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// TestDecodeDataUpdateDriving parses a driving data:update frame (the shape
// TeslaMate/the real Owner endpoint push) into a canonical snapshot, asserting
// the imperial->SI inverse conversions and the presence bits.
func TestDecodeDataUpdateDriving(t *testing.T) {
	t.Parallel()
	// value: ts,speed,odometer,soc,elevation,est_heading,est_lat,est_lng,
	//        power,shift_state,range,est_range,heading
	// speed=60mph, odo=12345.6mi, soc=78, est_heading=90, lat/lng, power=-30
	// (driving, dropped), shift=D, range=210mi, heading=90.
	frame := `{"msg_type":"data:update","tag":"5YJSA11111111111","value":"1700000000000,60,12345.6,78,120,90,37.7749,-122.4194,-30,D,210,205,90"}`

	tag, snap, ok := DecodeDataUpdate(frame)
	if !ok {
		t.Fatal("expected ok")
	}
	if tag != "5YJSA11111111111" {
		t.Errorf("tag = %q", tag)
	}
	st := snap.State
	if st == nil {
		t.Fatal("nil state")
	}
	if got := units.MpsToMph(st.SpeedMps); math.Abs(got-60) > 0.01 {
		t.Errorf("speed = %.2f mps (%.1f mph), want 60mph", st.SpeedMps, got)
	}
	if got := units.MetersToMiles(float64(st.OdometerMeters)); math.Abs(got-12345.6) > 0.01 {
		t.Errorf("odometer = %d m (%.1f mi), want 12345.6mi", st.OdometerMeters, got)
	}
	if st.BatteryLevelPct != 78 {
		t.Errorf("soc = %v, want 78", st.BatteryLevelPct)
	}
	if st.HeadingDeg != 90 {
		t.Errorf("heading = %v, want 90", st.HeadingDeg)
	}
	if st.Location == nil || st.Location.Latitude != 37.7749 || st.Location.Longitude != -122.4194 {
		t.Errorf("location = %+v, want 37.7749/-122.4194", st.Location)
	}
	// Location must carry the frame time (the ts prefix), never the dishonest
	// Go-zero that crashes TeslaMate's DateTime.from_unix! downstream.
	if st.Location != nil && !st.Location.TimeStamp.Equal(time.UnixMilli(1700000000000)) {
		t.Errorf("location timestamp = %v, want frame time 1700000000000ms", st.Location.TimeStamp)
	}
	// Drive power (negative) has no canonical home: no live session.
	if snap.Live != nil {
		t.Errorf("live = %+v, want nil (drive power dropped)", snap.Live)
	}
	if st.Gear != vehicle.GearDrive {
		t.Errorf("gear = %v, want GearDrive", st.Gear)
	}
	if got := units.KmToMiles(float64(st.RangeKm)); math.Abs(got-210) > 0.1 {
		t.Errorf("range = %d km (%.1f mi), want 210mi", st.RangeKm, got)
	}
	want := vehicle.StreamSpeed | vehicle.StreamOdometer | vehicle.StreamSOC |
		vehicle.StreamHeading | vehicle.StreamLoc | vehicle.StreamGear | vehicle.StreamRange
	if snap.StreamPresent != want {
		t.Errorf("stream present = %b, want %b", snap.StreamPresent, want)
	}
	if !snap.StreamFields.Equal(time.UnixMilli(1700000000000)) {
		t.Errorf("stream fields = %v, want 1700000000000ms", snap.StreamFields)
	}
}

// TestDecodeDataUpdateChargingPower asserts positive power (charging) maps to the
// live session slot and sets the charge-power presence bit.
func TestDecodeDataUpdateChargingPower(t *testing.T) {
	t.Parallel()
	frame := `{"msg_type":"data:update","tag":"VIN","value":"1700000000000,0,1000,80,0,0,0,0,42,P,300,290,0"}`
	_, snap, ok := DecodeDataUpdate(frame)
	if !ok {
		t.Fatal("expected ok")
	}
	if snap.Live == nil || snap.Live.PowerKw != 42 {
		t.Errorf("live = %+v, want PowerKw=42", snap.Live)
	}
	if snap.StreamPresent&vehicle.StreamChargePower == 0 {
		t.Errorf("charge-power presence not set")
	}
}

// TestDecodeDataUpdateEmptyColumnsHeld asserts an empty column is NOT marked
// present (the presence-aware merge holds the last-known value).
func TestDecodeDataUpdateEmptyColumnsHeld(t *testing.T) {
	t.Parallel()
	// Only speed and soc present; the rest blank (ts,speed,"",soc, then 9 blanks).
	frame := `{"msg_type":"data:update","tag":"VIN","value":"1700000000000,55,,80,,,,,,,,,"}`
	_, snap, ok := DecodeDataUpdate(frame)
	if !ok {
		t.Fatal("expected ok")
	}
	want := vehicle.StreamSpeed | vehicle.StreamSOC
	if snap.StreamPresent != want {
		t.Errorf("stream present = %b, want %b (only speed+soc)", snap.StreamPresent, want)
	}
	if snap.State.OdometerMeters != 0 || snap.State.RangeKm != 0 || snap.State.Gear != vehicle.GearUnknown {
		t.Errorf("absent fields not zero: odo=%d range=%d gear=%v", snap.State.OdometerMeters, snap.State.RangeKm, snap.State.Gear)
	}
}

// TestDecodeDataUpdateKeepaliveDropped asserts a frame with no populated column
// yields zero StreamPresent (the caller drops it before the cache).
func TestDecodeDataUpdateKeepaliveDropped(t *testing.T) {
	t.Parallel()
	frame := `{"msg_type":"data:update","tag":"VIN","value":"1700000000000,,,,,,,,,,,"}`
	_, snap, ok := DecodeDataUpdate(frame)
	if !ok {
		t.Fatal("expected ok")
	}
	if snap.StreamPresent != 0 {
		t.Errorf("keepalive stream present = %b, want 0", snap.StreamPresent)
	}
}

// TestDecodeDataUpdateNonDataUpdateRejected asserts control:hello / data:error
// frames are rejected (ok=false) so the dialer ignores them.
func TestDecodeDataUpdateNonDataUpdateRejected(t *testing.T) {
	t.Parallel()
	for _, frame := range []string{
		`{"msg_type":"control:hello","connection_timeout":30000}`,
		`{"msg_type":"data:error","tag":"VIN","error_type":"vehicle_error","error":"asleep"}`,
		`not json`,
	} {
		if _, _, ok := DecodeDataUpdate(frame); ok {
			t.Errorf("decoded non-data:update frame: %s", frame)
		}
	}
}

// TestRoundTripEncodeDecode pins the column-order contract between the sink
// encode and the Owner source decode: encode a snapshot, decode the frame, and
// assert every field survives the round trip. If the sink's column order drifts
// from the source's, this test breaks instead of silently mis-mapping columns.
func TestRoundTripEncodeDecode(t *testing.T) {
	t.Parallel()
	src := vehicle.Snapshot{
		State: &vehicle.State{
			SpeedMps:        units.MphToMps(55),
			OdometerMeters:  int(math.Round(units.MilesToMeters(12345.0))),
			BatteryLevelPct: 80,
			HeadingDeg:      270,
			Location:        &vehicle.Location{Latitude: 40.0, Longitude: -105.0},
			Gear:            vehicle.GearDrive,
			RangeKm:         int(math.Round(units.MilesToKm(200))),
		},
		Live:         &vehicle.LiveSession{PowerKw: 11},
		FetchedAt:    time.UnixMilli(1700000000000),
		StreamFields: time.UnixMilli(1700000000000),
	}

	frame := streamsink.EncodeDataUpdate("VIN", src)
	if frame == "" {
		t.Fatal("encode returned empty frame")
	}
	tag, got, ok := DecodeDataUpdate(frame)
	if !ok {
		t.Fatalf("decode failed: %s", frame)
	}
	if tag != "VIN" {
		t.Errorf("tag = %q", tag)
	}
	if math.Abs(got.State.SpeedMps-src.State.SpeedMps) > 0.01 {
		t.Errorf("speed round-trip: got %.4f want %.4f", got.State.SpeedMps, src.State.SpeedMps)
	}
	if got.State.OdometerMeters != src.State.OdometerMeters {
		t.Errorf("odometer round-trip: got %d want %d", got.State.OdometerMeters, src.State.OdometerMeters)
	}
	if got.State.BatteryLevelPct != src.State.BatteryLevelPct {
		t.Errorf("soc round-trip: got %v want %v", got.State.BatteryLevelPct, src.State.BatteryLevelPct)
	}
	if got.State.HeadingDeg != src.State.HeadingDeg {
		t.Errorf("heading round-trip: got %v want %v", got.State.HeadingDeg, src.State.HeadingDeg)
	}
	if got.State.Location == nil || got.State.Location.Latitude != src.State.Location.Latitude || got.State.Location.Longitude != src.State.Location.Longitude {
		t.Errorf("location round-trip: got %+v want %+v", got.State.Location, src.State.Location)
	}
	if got.State.Gear != src.State.Gear {
		t.Errorf("gear round-trip: got %v want %v", got.State.Gear, src.State.Gear)
	}
	if got.State.RangeKm != src.State.RangeKm {
		t.Errorf("range round-trip: got %d want %d", got.State.RangeKm, src.State.RangeKm)
	}
	if got.Live == nil || got.Live.PowerKw != src.Live.PowerKw {
		t.Errorf("power round-trip: got %+v want PowerKw=%v", got.Live, src.Live.PowerKw)
	}
	// Every encoded field must be present on decode.
	want := vehicle.StreamSpeed | vehicle.StreamOdometer | vehicle.StreamSOC |
		vehicle.StreamHeading | vehicle.StreamLoc | vehicle.StreamGear | vehicle.StreamRange | vehicle.StreamChargePower
	if got.StreamPresent != want {
		t.Errorf("round-trip present = %b, want %b\nframe: %s", got.StreamPresent, want, frame)
	}

}
