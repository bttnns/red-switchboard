package v1

import (
	"math"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
)

// datum helpers build a *protos.Datum carrying one Value oneof variant, so the
// fixture reads like a recorded frame and exercises the real oneof decode path.
func datumDouble(k protos.Field, v float64) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: v}}}
}
func datumFloat(k protos.Field, v float32) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: v}}}
}
func datumInt(k protos.Field, v int32) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: v}}}
}
func datumLong(k protos.Field, v int64) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_LongValue{LongValue: v}}}
}
func datumString(k protos.Field, v string) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_StringValue{StringValue: v}}}
}
func datumShift(k protos.Field, s protos.ShiftState) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_ShiftStateValue{ShiftStateValue: s}}}
}
func datumLocation(k protos.Field, lat, lng float64) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: lat, Longitude: lng}}}}
}
func datumBool(k protos.Field, v bool) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_BooleanValue{BooleanValue: v}}}
}
func datumChargeState(k protos.Field, v protos.DetailedChargeStateValue) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_DetailedChargeStateValue{DetailedChargeStateValue: v}}}
}
func datumSentry(k protos.Field, v protos.SentryModeState) *protos.Datum {
	return &protos.Datum{Key: k, Value: &protos.Value{Value: &protos.Value_SentryModeStateValue{SentryModeStateValue: v}}}
}

func TestDecodePayload_drivingFrame(t *testing.T) {
	t.Parallel()
	// A driving frame: speed 60 mph, odometer 12345.6 mi, SOC 72%, gear D,
	// heading 90, location, rated range 250 mi. Fields are spread across oneof
	// variants (Double/Float/Int/Long) to prove numVal coalesces them all.
	payload := &protos.Payload{
		Data: []*protos.Datum{
			datumDouble(protos.Field_VehicleSpeed, 60), // mph
			datumFloat(protos.Field_Odometer, 12345.6), // miles
			datumInt(protos.Field_Soc, 72),             // %
			datumShift(protos.Field_Gear, protos.ShiftState_ShiftStateD),
			datumLong(protos.Field_GpsHeading, 90), // deg
			datumLocation(protos.Field_Location, 37.4, -122.1),
			datumDouble(protos.Field_RatedRange, 250), // miles
		},
	}
	snap := DecodePayload(payload)
	st := snap.State
	if st == nil {
		t.Fatal("nil state")
	}
	// Liveness is the cache merge's job (set when a frame carries data); the
	// decode itself leaves Power at Unknown.
	if st.Power != vehicle.PowerUnknown {
		t.Errorf("Power: want Unknown (set by merge), got %v", st.Power)
	}
	if snap.StreamPresent == 0 {
		t.Fatal("StreamPresent: want non-zero, got 0")
	}
	if got := st.SpeedMps; !almostEq(got, units.MphToMps(60)) {
		t.Errorf("SpeedMps: want %g, got %g", units.MphToMps(60), got)
	}
	if got := st.OdometerMeters; got != int(math.Round(units.MilesToMeters(12345.6))) {
		t.Errorf("OdometerMeters: want %d, got %d", int(math.Round(units.MilesToMeters(12345.6))), got)
	}
	if st.BatteryLevelPct != 72 {
		t.Errorf("BatteryLevelPct: want 72, got %g", st.BatteryLevelPct)
	}
	if st.Gear != vehicle.GearDrive {
		t.Errorf("Gear: want Drive, got %v", st.Gear)
	}
	if st.HeadingDeg != 90 {
		t.Errorf("HeadingDeg: want 90, got %g", st.HeadingDeg)
	}
	if st.RangeKm != int(math.Round(units.MilesToKm(250))) {
		t.Errorf("RangeKm: want %d, got %d", int(math.Round(units.MilesToKm(250))), st.RangeKm)
	}
	if st.Location == nil || st.Location.Latitude != 37.4 || st.Location.Longitude != -122.1 {
		t.Errorf("Location: want 37.4,-122.1, got %+v", st.Location)
	}
	// Location must carry the frame time (receive time, == FetchedAt), never the
	// dishonest Go-zero that crashes TeslaMate's DateTime.from_unix! downstream.
	if st.Location != nil && (st.Location.TimeStamp.IsZero() || !st.Location.TimeStamp.Equal(snap.FetchedAt)) {
		t.Errorf("Location.TimeStamp: want non-zero == FetchedAt %v, got %v", snap.FetchedAt, st.Location.TimeStamp)
	}
	if snap.Live != nil {
		t.Errorf("Live: want nil (no charging power), got %+v", snap.Live)
	}
}

func TestDecodePayload_chargingPower(t *testing.T) {
	t.Parallel()
	payload := &protos.Payload{
		Data: []*protos.Datum{
			datumDouble(protos.Field_DCChargingPower, 42.5),
		},
	}
	snap := DecodePayload(payload)
	if snap.Live == nil || snap.Live.PowerKw != 42.5 {
		t.Fatalf("Live.PowerKw: want 42.5, got %+v", snap.Live)
	}
}

func TestDecodePayload_zeroPowerSurfacedForChargeEnd(t *testing.T) {
	t.Parallel()
	// A 0-power charge frame is now surfaced (Live.PowerKw=0 + StreamChargePower) so
	// the cache can detect charge-end and trigger a terminal poll. The merge keeps
	// the last non-zero power for the served session (see internal/cache merge).
	snap := DecodePayload(&protos.Payload{Data: []*protos.Datum{datumDouble(protos.Field_DCChargingPower, 0)}})
	if snap.Live == nil || snap.Live.PowerKw != 0 {
		t.Fatalf("zero power: want Live{PowerKw:0}, got %+v", snap.Live)
	}
	if snap.StreamPresent&vehicle.StreamChargePower == 0 {
		t.Error("zero power: want StreamChargePower present so the cache can detect charge-end")
	}
}

func TestDecodePayload_gearStringFallback(t *testing.T) {
	t.Parallel()
	// Older firmware sends a bare "R" string instead of the ShiftState enum.
	if got := DecodePayload(&protos.Payload{Data: []*protos.Datum{datumString(protos.Field_Gear, "R")}}).State.Gear; got != vehicle.GearReverse {
		t.Errorf("string gear R: want Reverse, got %v", got)
	}
}

func TestDecodePayload_unknownKeysIgnored(t *testing.T) {
	t.Parallel()
	// Forward-compat: a Datum with an unknown Field must not panic or affect the
	// known fields. Use a high enum number Tesla has not assigned yet.
	payload := &protos.Payload{
		Data: []*protos.Datum{
			datumDouble(protos.Field(9999), 1),
			datumInt(protos.Field_Soc, 50),
		},
	}
	snap := DecodePayload(payload)
	if snap.State.BatteryLevelPct != 50 {
		t.Errorf("SOC after unknown key: want 50, got %g", snap.State.BatteryLevelPct)
	}
}

func TestDecodePayload_climate(t *testing.T) {
	t.Parallel()
	// A parked-but-awake climate frame: cabin and outside temps only (J1a). These
	// carry no driving/charge field, so they prove a temp-only frame still has a
	// presence bit set and is not treated as an empty keepalive.
	payload := &protos.Payload{
		Data: []*protos.Datum{
			datumFloat(protos.Field_InsideTemp, 21.5),
			datumDouble(protos.Field_OutsideTemp, 18),
		},
	}
	snap := DecodePayload(payload)
	st := snap.State
	if st == nil {
		t.Fatal("nil state")
	}
	if snap.StreamPresent&(vehicle.StreamCabinTemp|vehicle.StreamOutsideTemp) == 0 {
		t.Fatalf("StreamPresent: want cabin+outside temp bits, got %x", snap.StreamPresent)
	}
	if !almostEq(st.CabinTempC, 21.5) {
		t.Errorf("CabinTempC: want 21.5, got %g", st.CabinTempC)
	}
	if !almostEq(st.OutsideTempC, 18) {
		t.Errorf("OutsideTempC: want 18, got %g", st.OutsideTempC)
	}
}

func TestDecodePayload_tpms(t *testing.T) {
	t.Parallel()
	// A parked TPMS frame: the four tire pressures in bar only (J1b). Numeric
	// pressures are subscription-only over REST, so the stream is the only path
	// that fills these; the frame proves a tpms-only frame sets the presence bits.
	payload := &protos.Payload{
		Data: []*protos.Datum{
			datumDouble(protos.Field_TpmsPressureFl, 2.9),
			datumDouble(protos.Field_TpmsPressureFr, 2.85),
			datumDouble(protos.Field_TpmsPressureRl, 3.1),
			datumDouble(protos.Field_TpmsPressureRr, 3.05),
		},
	}
	snap := DecodePayload(payload)
	st := snap.State
	if st == nil {
		t.Fatal("nil state")
	}
	want := vehicle.StreamTpmsFl | vehicle.StreamTpmsFr | vehicle.StreamTpmsRl | vehicle.StreamTpmsRr
	if snap.StreamPresent&want != want {
		t.Fatalf("StreamPresent: want all four tpms bits, got %x", snap.StreamPresent)
	}
	if !almostEq(st.TpmsPressureFlBar, 2.9) {
		t.Errorf("TpmsPressureFlBar: want 2.9, got %g", st.TpmsPressureFlBar)
	}
	if !almostEq(st.TpmsPressureFrBar, 2.85) {
		t.Errorf("TpmsPressureFrBar: want 2.85, got %g", st.TpmsPressureFrBar)
	}
	if !almostEq(st.TpmsPressureRlBar, 3.1) {
		t.Errorf("TpmsPressureRlBar: want 3.1, got %g", st.TpmsPressureRlBar)
	}
	if !almostEq(st.TpmsPressureRrBar, 3.05) {
		t.Errorf("TpmsPressureRrBar: want 3.05, got %g", st.TpmsPressureRrBar)
	}
}

func TestDecodePayload_chargeDetail(t *testing.T) {
	t.Parallel()
	// An active-charge frame carrying the J1c charge-detail and state fields:
	// charge limit, time-to-full (hours -> minutes), charger voltage, charge amps,
	// detailed charge state (typed enum), battery heater, lock, and sentry. Proves
	// each maps to its canonical field and sets the matching presence bit.
	payload := &protos.Payload{
		Data: []*protos.Datum{
			datumInt(protos.Field_ChargeLimitSoc, 80),
			datumDouble(protos.Field_TimeToFullCharge, 1.5), // hours
			datumInt(protos.Field_ChargerVoltage, 240),
			datumDouble(protos.Field_ChargeAmps, 32),
			datumChargeState(protos.Field_DetailedChargeState, protos.DetailedChargeStateValue_DetailedChargeStateCharging),
			datumBool(protos.Field_BatteryHeaterOn, true),
			datumBool(protos.Field_Locked, true),
			datumSentry(protos.Field_SentryMode, protos.SentryModeState_SentryModeStateArmed),
		},
	}
	snap := DecodePayload(payload)
	st := snap.State
	if st == nil {
		t.Fatal("nil state")
	}
	want := vehicle.StreamChargeLimit | vehicle.StreamTimeToFull | vehicle.StreamChargerVoltage |
		vehicle.StreamChargeCurrent | vehicle.StreamChargeState | vehicle.StreamBatteryHeater |
		vehicle.StreamLocked | vehicle.StreamSentry
	if snap.StreamPresent&want != want {
		t.Fatalf("StreamPresent: want all charge-detail bits, got %x", snap.StreamPresent)
	}
	if !almostEq(st.BatteryLimitPct, 80) {
		t.Errorf("BatteryLimitPct: want 80, got %g", st.BatteryLimitPct)
	}
	if st.TimeToEndOfChargeMin != 90 {
		t.Errorf("TimeToEndOfChargeMin: want 90, got %d", st.TimeToEndOfChargeMin)
	}
	if st.ChargerVoltageV != 240 {
		t.Errorf("ChargerVoltageV: want 240, got %d", st.ChargerVoltageV)
	}
	if snap.Live == nil || !almostEq(snap.Live.CurrentA, 32) {
		t.Errorf("Live.CurrentA: want 32, got %+v", snap.Live)
	}
	if st.Charger != vehicle.ChargerCharging {
		t.Errorf("Charger: want ChargerCharging, got %v", st.Charger)
	}
	if st.Plug != vehicle.PlugConnected {
		t.Errorf("Plug: want PlugConnected, got %v", st.Plug)
	}
	if !st.BatteryHeaterOn {
		t.Error("BatteryHeaterOn: want true, got false")
	}
	if st.DoorFrontLeftLocked != "locked" {
		t.Errorf("DoorFrontLeftLocked: want locked, got %q", st.DoorFrontLeftLocked)
	}
	if st.GearGuardStatus != "enabled" {
		t.Errorf("GearGuardStatus: want enabled, got %q", st.GearGuardStatus)
	}
}

func TestDecodePayload_chargingEnergyIn(t *testing.T) {
	t.Parallel()
	// AC/DCChargingEnergyIn is cumulative session kWh, the source for TeslaMate's
	// required charge_energy_added over a fully-streamed charge. Both the AC and DC
	// variants land in the same canonical slot and set StreamChargeEnergyIn.
	for _, tc := range []struct {
		name  string
		field protos.Field
		kwh   float64
	}{
		{"DC", protos.Field_DCChargingEnergyIn, 18.4},
		{"AC", protos.Field_ACChargingEnergyIn, 6.2},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := DecodePayload(&protos.Payload{Data: []*protos.Datum{datumDouble(tc.field, tc.kwh)}})
			if snap.Live == nil || !almostEq(snap.Live.TotalChargedEnergy, tc.kwh) {
				t.Fatalf("Live.TotalChargedEnergy: want %g, got %+v", tc.kwh, snap.Live)
			}
			if snap.StreamPresent&vehicle.StreamChargeEnergyIn == 0 {
				t.Errorf("StreamPresent: want StreamChargeEnergyIn set, got %x", snap.StreamPresent)
			}
		})
	}
}

func TestDecodePayload_empty(t *testing.T) {
	t.Parallel()
	snap := DecodePayload(&protos.Payload{})
	if snap.State == nil || !snap.State.IsZeroish() {
		t.Fatalf("empty payload: want zeroish state, got %+v", snap.State)
	}
	if snap.StreamPresent != 0 {
		t.Errorf("empty payload: want StreamPresent=0 (keepalive), got %x", snap.StreamPresent)
	}
}

func TestDecodePayload_nil(t *testing.T) {
	t.Parallel()
	if got := DecodePayload(nil); got.State != nil {
		t.Fatalf("nil payload: want zero Snapshot, got %+v", got)
	}
}

// payloadOf round-trips: a marshaled Payload decodes back through payloadOf.
func TestPayloadOf_roundTrip(t *testing.T) {
	t.Parallel()
	orig := &protos.Payload{Data: []*protos.Datum{datumInt(protos.Field_Soc, 88)}}
	buf, err := proto.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	got := payloadOf(buf)
	if got == nil || len(got.Data) != 1 || got.Data[0].GetValue().GetIntValue() != 88 {
		t.Fatalf("round-trip: got %+v", got)
	}
	if payloadOf(nil) != nil {
		t.Error("payloadOf(nil) want nil")
	}
	if payloadOf([]byte("garbage")) != nil {
		t.Error("payloadOf(garbage) want nil")
	}
}

func TestDecodePayload_fetchedAtIsRecent(t *testing.T) {
	t.Parallel()
	before := time.Now()
	snap := DecodePayload(&protos.Payload{Data: []*protos.Datum{datumInt(protos.Field_Soc, 1)}})
	if snap.FetchedAt.Before(before) || time.Since(snap.FetchedAt) > time.Second {
		t.Errorf("FetchedAt not recent: %v", snap.FetchedAt)
	}
}

func almostEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
