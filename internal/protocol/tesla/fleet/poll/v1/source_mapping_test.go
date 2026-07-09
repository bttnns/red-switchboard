package v1

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRoundTripThroughSink proves the source decode (DecodeVehicleData) is the
// inverse of the sink encode (VehicleData) for every canonical field both
// directions support. The sink encoder is the mock here: we build a canonical
// State (+ LiveSession), run it through VehicleData to get the Tesla wire shape,
// then decode that back and assert the canonical values survive the loop (within
// imperial<->SI rounding). This is the "test using the mocking" the spec asks for.
func TestRoundTripThroughSink(t *testing.T) {
	t.Parallel()
	in := &vehicle.State{
		Power:             vehicle.PowerOnline,
		UserPresent:       true,
		Gear:              vehicle.GearDrive,
		OdometerMeters:    160934, // ~100 mi
		RangeKm:           322,    // ~200 mi
		BatteryLevelPct:   78,
		BatteryLimitPct:   90,
		Charger:           vehicle.ChargerCharging,
		Plug:              vehicle.PlugConnected,
		ChargePortOpen:    true,
		SpeedMps:          20, // ~44.7 mph
		HeadingDeg:        90,
		CabinTempC:        21,
		Location:          &vehicle.Location{Latitude: 37.5, Longitude: -122.3, TimeStamp: fixedTime},
		LastUpdate:        fixedTime,
		SeatHeatFrontLeft: "Level_2",
		GearGuardStatus:   "enabled",
		DrivePowerKw:      ptrF64(42), // kW, signed; round-trips unchanged
	}
	// Keep power in the AC range (< dcChargeThresholdKw): the sink drops
	// charger_actual_current for DC sessions (to avoid a TeslaMate smallint
	// overflow), so only an AC session round-trips current.
	live := &vehicle.LiveSession{
		PowerKw:            7,
		CurrentA:           32,
		TotalChargedEnergy: 23.4,
		TimeRemainingSec:   1800,
	}

	// Encode canonical -> Tesla wire via the sink (the mock).
	wired := VehicleData(nil, in, live, testIDs, testCfg, time.Time{}, nil)

	// Decode Tesla wire -> canonical via the code under test.
	snap := DecodeVehicleData(&wired)
	require.NotNil(t, snap.State)
	out := snap.State

	// Liveness / presence.
	assert.Equal(t, vehicle.PowerOnline, out.Power)
	assert.True(t, out.UserPresent)

	// Gear, charger, plug enums round-trip exactly.
	assert.Equal(t, vehicle.GearDrive, out.Gear)
	assert.Equal(t, vehicle.ChargerCharging, out.Charger)
	assert.Equal(t, vehicle.PlugConnected, out.Plug)
	assert.True(t, out.ChargePortOpen)

	// Battery percentages are integers on the wire; exact.
	assert.Equal(t, 78.0, out.BatteryLevelPct)
	assert.Equal(t, 90.0, out.BatteryLimitPct)

	// Odometer: m -> mi -> m. The wire mi is a float, so allow 1 m of rounding.
	assert.InDelta(t, in.OdometerMeters, out.OdometerMeters, 1.0)

	// Range: km -> mi (int) -> km. The sink truncates km->mi to a float then
	// decode rounds mi->km, so allow a couple km of slack from the int hops.
	assert.InDelta(t, in.RangeKm, out.RangeKm, 2.0)

	// Speed: m/s -> mph -> m/s.
	assert.InDelta(t, in.SpeedMps, out.SpeedMps, 0.01)

	// Heading: float deg -> int -> float.
	assert.Equal(t, 90.0, out.HeadingDeg)

	// Location survives.
	require.NotNil(t, out.Location)
	assert.InDelta(t, 37.5, out.Location.Latitude, 0.0001)
	assert.InDelta(t, -122.3, out.Location.Longitude, 0.0001)

	// Cabin temp (Celsius both directions).
	assert.InDelta(t, 21.0, out.CabinTempC, 0.01)

	// Seat heater level string round-trips.
	assert.Equal(t, "Level_2", out.SeatHeatFrontLeft)

	// Gear Guard / sentry round-trips to enabled.
	assert.Equal(t, "enabled", out.GearGuardStatus)

	// Drive power (kW) round-trips, sign and magnitude preserved.
	require.NotNil(t, out.DrivePowerKw)
	assert.InDelta(t, 42.0, *out.DrivePowerKw, 0.01)

	// Live charging session reconstructed from charge_state.
	require.NotNil(t, snap.Live)
	assert.InDelta(t, 7.0, snap.Live.PowerKw, 0.01)
	assert.InDelta(t, 32.0, snap.Live.CurrentA, 0.01)
	assert.InDelta(t, 23.4, snap.Live.TotalChargedEnergy, 0.01)
	assert.InDelta(t, 1800, snap.Live.TimeRemainingSec, 1.0)
}

// TestRoundTripParked covers the non-charging branch: a parked, unplugged car
// produces no live session and the parked gear/charger/plug enums round-trip.
func TestRoundTripParked(t *testing.T) {
	t.Parallel()
	in := &vehicle.State{
		Power:           vehicle.PowerSleep,
		Gear:            vehicle.GearPark,
		Charger:         vehicle.ChargerDisconnect,
		Plug:            vehicle.PlugDisconnected,
		BatteryLevelPct: 55,
		OdometerMeters:  321868, // ~200 mi
		RangeKm:         200,
		LastUpdate:      fixedTime,
	}

	wired := VehicleData(nil, in, nil, testIDs, testCfg, time.Time{}, nil)
	snap := DecodeVehicleData(&wired)
	require.NotNil(t, snap.State)
	out := snap.State

	assert.Equal(t, vehicle.PowerSleep, out.Power)
	assert.Equal(t, vehicle.GearPark, out.Gear)
	assert.Equal(t, vehicle.ChargerDisconnect, out.Charger)
	assert.Equal(t, vehicle.PlugDisconnected, out.Plug)
	assert.Nil(t, snap.Live, "no live session when not charging")
	assert.InDelta(t, in.OdometerMeters, out.OdometerMeters, 1.0)
}

// TestDecodeDrivePower covers drive_state.power decode: a regen (negative)
// reading is preserved with its sign, and an absent reading leaves the canonical
// slot nil so the sink emits null.
func TestDecodeDrivePower(t *testing.T) {
	t.Parallel()

	regen := DecodeVehicleData(&wire.VehicleData{
		State:      "online",
		DriveState: &wire.DriveState{Power: ptrF64(-18)},
	})
	require.NotNil(t, regen.State)
	require.NotNil(t, regen.State.DrivePowerKw)
	assert.InDelta(t, -18.0, *regen.State.DrivePowerKw, 0.01)

	absent := DecodeVehicleData(&wire.VehicleData{
		State:      "online",
		DriveState: &wire.DriveState{},
	})
	require.NotNil(t, absent.State)
	assert.Nil(t, absent.State.DrivePowerKw, "absent power stays nil")
}

// TestDecodeFixture decodes the real captured vehicle_data sample (from the
// unofficial Tesla API cassette) and asserts the decode produces sane canonical
// values, including the imperial->SI conversions.
func TestDecodeFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "fleet_vehicle_data.json"))
	require.NoError(t, err)

	var env struct {
		Response wire.VehicleData `json:"response"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))

	snap := DecodeVehicleData(&env.Response)
	require.NotNil(t, snap.State)
	st := snap.State

	// Top-level state "online".
	assert.Equal(t, vehicle.PowerOnline, st.Power)

	// shift_state null -> parked/unknown gear.
	assert.Equal(t, vehicle.GearUnknown, st.Gear)

	// battery_level 90, charge_limit_soc 90.
	assert.Equal(t, 90.0, st.BatteryLevelPct)
	assert.Equal(t, 90.0, st.BatteryLimitPct)

	// charging_state "Complete" -> idle + plugged.
	assert.Equal(t, vehicle.ChargerIdle, st.Charger)
	assert.Equal(t, vehicle.PlugConnected, st.Plug)
	assert.True(t, st.ChargePortOpen)

	// odometer 57509.856033 mi -> meters.
	wantOdo := int(math.Round(units.MilesToMeters(57509.856033)))
	assert.Equal(t, wantOdo, st.OdometerMeters)

	// battery_range 224.81 mi -> km.
	wantRange := int(math.Round(units.MilesToKm(224.81)))
	assert.Equal(t, wantRange, st.RangeKm)

	// Location parsed.
	require.NotNil(t, st.Location)
	assert.InDelta(t, 3.972, st.Location.Latitude, 0.0001)
	assert.InDelta(t, -4.9147, st.Location.Longitude, 0.0001)

	// inside_temp 27.0 C / outside_temp 23.0 C pass through.
	assert.InDelta(t, 27.0, st.CabinTempC, 0.01)
	assert.InDelta(t, 23.0, st.OutsideTempC, 0.01)

	// car_version passes through.
	assert.Equal(t, "2020.36.16 3e9e4e8dd287", st.OtaVersion)

	// vehicle_config car_type/trim_badging decode (so the sink shows the real model).
	assert.Equal(t, "models2", st.CarType)
	assert.Equal(t, "p90d", st.TrimBadging)

	// is_user_present false, sentry_mode false, locked false.
	assert.False(t, st.UserPresent)
	assert.Equal(t, "disabled", st.GearGuardStatus)
	assert.Equal(t, "unlocked", st.DoorFrontLeftLocked)

	// "Complete" is not actively charging -> no live session.
	assert.Nil(t, snap.Live)

	// drive_state.timestamp -> LastUpdate populated.
	assert.False(t, st.LastUpdate.IsZero())
	assert.WithinDuration(t, time.UnixMilli(1604976818416).UTC(), st.LastUpdate, time.Second)
}
