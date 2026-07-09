package v1

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testIDs = IDs{ID: 42, VehicleID: 1000000000042, VIN: "RIV0000000000042", DisplayName: "Test Rivian"}
var testCfg = Cfg{CarType: "model3", Model: "R1T"}

// fixedTime makes snapshot timestamps deterministic for snapshot comparison.
var fixedTime = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// TestVehicleConfigPrefersDecodedModel: a snapshot carrying the source's real
// car_type/trim emits those (so a Tesla shows its true model), not the cfg
// placeholder; a snapshot without them holds last-known from prev, then falls
// back to cfg, then the built-in default.
func TestVehicleConfigPrefersDecodedModel(t *testing.T) {
	t.Parallel()
	// Real values decoded from vehicle_data win over the cfg placeholder.
	vs := &vehicle.State{Power: vehicle.PowerOnline, LastUpdate: fixedTime, CarType: "models2", TrimBadging: "models"}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	require.NotNil(t, out.VehicleConfig)
	assert.Equal(t, "models2", deref(out.VehicleConfig.CarType))
	assert.Equal(t, "models", deref(out.VehicleConfig.TrimBadging))

	// A later summary poll (no vehicle_config) holds last-known via prev.
	prev := out
	summary := &vehicle.State{Power: vehicle.PowerSleep, LastUpdate: fixedTime}
	out2 := VehicleData(&prev, summary, nil, testIDs, testCfg, time.Time{}, nil)
	assert.Equal(t, "models2", deref(out2.VehicleConfig.CarType))
	assert.Equal(t, "models", deref(out2.VehicleConfig.TrimBadging))

	// With neither a decoded value nor prev, fall back to the cfg placeholder.
	out3 := VehicleData(nil, summary, nil, testIDs, testCfg, time.Time{}, nil)
	assert.Equal(t, "model3", deref(out3.VehicleConfig.CarType))
	assert.Equal(t, "R1T", deref(out3.VehicleConfig.TrimBadging))
}

func deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

// --- enum / shift_state ---------------------------------------------------

func TestShiftState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		gear vehicle.Gear
		want string
	}{
		{vehicle.GearDrive, "D"},
		{vehicle.GearReverse, "R"},
		{vehicle.GearNeutral, "N"},
		{vehicle.GearPark, "P"},
		{vehicle.GearUnknown, "P"},
	}
	for _, c := range cases {
		vs := &vehicle.State{Gear: c.gear, Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime}
		out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
		got := deref(out.DriveState.ShiftState)
		if got != c.want {
			t.Errorf("gear %v: shift_state = %q, want %q", c.gear, got, c.want)
		}
	}
}

// --- charging_state -------------------------------------------------------

func TestChargingState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		charger vehicle.ChargerState
		plug    vehicle.ChargePlug
		want    string
	}{
		{"active", vehicle.ChargerCharging, vehicle.PlugConnected, "Charging"},
		{"connecting", vehicle.ChargerCharging, vehicle.PlugConnected, "Charging"},
		{"ready/stopped", vehicle.ChargerIdle, vehicle.PlugConnected, "Stopped"},
		{"unplugged", vehicle.ChargerUnknown, vehicle.PlugDisconnected, "Disconnected"},
		{"none", vehicle.ChargerDisconnect, vehicle.PlugUnknown, "Disconnected"},
	}
	for _, c := range cases {
		vs := &vehicle.State{Charger: c.charger, Plug: c.plug, Power: vehicle.PowerOnline, LastUpdate: fixedTime}
		out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
		got := deref(out.ChargeState.ChargingState)
		if got != c.want {
			t.Errorf("%s: charging_state = %q, want %q", c.name, got, c.want)
		}
	}
}

// --- top-level state ------------------------------------------------------

func TestTopState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		power vehicle.Power
		want  string
	}{
		{"sleep", vehicle.PowerSleep, "asleep"},
		{"online", vehicle.PowerOnline, "online"},
		{"offline", vehicle.PowerOffline, "offline"},
		{"unknown->offline", vehicle.PowerUnknown, "offline"},
	}
	for _, c := range cases {
		vs := &vehicle.State{Power: c.power, LastUpdate: fixedTime}
		out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
		if out.State != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, out.State, c.want)
		}
	}
}

// --- unit conversions (known numbers) -------------------------------------

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestUnitConversions(t *testing.T) {
	t.Parallel()
	// 16093.44 m = 10 miles; 160.9344 km = 100 miles; 10 m/s = 22.36936 mph.
	vs := &vehicle.State{
		OdometerMeters: 16093, // ~10 miles
		RangeKm:        161,   // ~100 miles
		SpeedMps:       10,    // m/s
		CabinTempC:     21.5,
		OutsideTempC:   23.0,
		Power:          vehicle.PowerOnline,
		UserPresent:    true,
		LastUpdate:     fixedTime,
	}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)

	if odo := deref(out.VehicleState.Odometer); !approx(odo, units.MetersToMiles(16093)) {
		t.Errorf("odometer = %v, want %v", odo, units.MetersToMiles(16093))
	}
	if r := deref(out.ChargeState.IdealBatteryRange); !approx(r, units.KmToMiles(161)) {
		t.Errorf("range = %v, want %v", r, units.KmToMiles(161))
	}
	if sp := deref(out.DriveState.Speed); !approx(sp, units.MpsToMph(10)) {
		t.Errorf("speed = %v, want %v", sp, units.MpsToMph(10))
	}
	if temp := deref(out.ClimateState.InsideTemp); !approx(temp, 21.5) {
		t.Errorf("inside_temp = %v, want 21.5 (passthrough)", temp)
	}
	if out.ClimateState.OutsideTemp == nil || !approx(deref(out.ClimateState.OutsideTemp), 23.0) {
		t.Errorf("outside_temp = %v, want 23.0 (passthrough)", out.ClimateState.OutsideTemp)
	}
	// A source that does not report ambient temp (canonical 0) serves null, not 0 °C.
	noTemp := VehicleData(nil, &vehicle.State{Power: vehicle.PowerOnline, LastUpdate: fixedTime}, nil, testIDs, testCfg, time.Time{}, nil)
	if noTemp.ClimateState.OutsideTemp != nil {
		t.Errorf("outside_temp = %v, want null when unreported", deref(noTemp.ClimateState.OutsideTemp))
	}
	// range/odometer must be non-null always.
	if out.ChargeState.IdealBatteryRange == nil || out.ChargeState.ChargeEnergyAdded == nil {
		t.Error("ideal_battery_range and charge_energy_added must be non-null")
	}
}

// --- charge_energy_added: live vs fallback --------------------------------

func TestChargeEnergyAdded(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{Charger: vehicle.ChargerCharging, Plug: vehicle.PlugConnected, BatteryLevelPct: 50, Power: vehicle.PowerOnline, LastUpdate: fixedTime}

	// With a live session, use TotalChargedEnergy.
	live := &vehicle.LiveSession{PowerKw: 11.5, CurrentA: 48, TotalChargedEnergy: 7.3}
	out := VehicleData(nil, vs, live, testIDs, testCfg, time.Time{}, nil)
	if e := deref(out.ChargeState.ChargeEnergyAdded); !approx(e, 7.3) {
		t.Errorf("live charge_energy_added = %v, want 7.3", e)
	}
	// charger_power is rounded to a whole number (TeslaMate casts it to :integer).
	if p := deref(out.ChargeState.ChargerPower); !approx(p, 12) {
		t.Errorf("charger_power = %v, want 12 (round 11.5)", p)
	}
	if c := deref(out.ChargeState.ChargerActualCurrent); c != 48 {
		t.Errorf("charger_actual_current = %v, want 48", c)
	}

	// Without a live session, carry prev's value (non-null, no reset to 0).
	prev := out
	out2 := VehicleData(&prev, vs, nil, testIDs, testCfg, time.Time{}, nil)
	if e := deref(out2.ChargeState.ChargeEnergyAdded); !approx(e, 7.3) {
		t.Errorf("fallback charge_energy_added = %v, want carried 7.3", e)
	}
	// First sight with no live + no prev => 0 (non-null).
	out3 := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	if out3.ChargeState.ChargeEnergyAdded == nil || deref(out3.ChargeState.ChargeEnergyAdded) != 0 {
		t.Errorf("first-sight fallback charge_energy_added should be 0 non-null, got %v", out3.ChargeState.ChargeEnergyAdded)
	}
}

// --- charger_power: always a whole, non-null number (TeslaMate :integer cast) ---

// TestChargerPowerWholeNumber pins the P6 invariant: charger_power must serialize
// as a non-null, integer-valued JSON number. TeslaMate casts charge_state.
// charger_power to a DB :integer and a fractional value (e.g. an AC 7.7 kW frame)
// fails the cast with "Invalid charge data: charger_power is invalid", dropping
// the charge row. So a zero/absent power emits a literal numeric 0, never blank.
func TestChargerPowerWholeNumber(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{Charger: vehicle.ChargerCharging, Plug: vehicle.PlugConnected, BatteryLevelPct: 50, Power: vehicle.PowerOnline, LastUpdate: fixedTime}

	tests := []struct {
		name string
		live *vehicle.LiveSession
		want float64
	}{
		{"no live session -> 0", nil, 0},
		{"zero power -> 0", &vehicle.LiveSession{PowerKw: 0}, 0},
		{"fractional AC rounds up", &vehicle.LiveSession{PowerKw: 7.7, CurrentA: 32}, 8},
		{"fractional AC rounds down", &vehicle.LiveSession{PowerKw: 7.3, CurrentA: 32}, 7},
		{"whole AC unchanged", &vehicle.LiveSession{PowerKw: 11, CurrentA: 48}, 11},
		{"DC fast charge whole", &vehicle.LiveSession{PowerKw: 175.4, CurrentA: 220}, 175},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := VehicleData(nil, vs, tc.live, testIDs, testCfg, time.Time{}, nil)
			p := out.ChargeState.ChargerPower
			if p == nil {
				t.Fatalf("charger_power is nil (serializes null); want numeric %v", tc.want)
			}
			if *p != math.Trunc(*p) {
				t.Errorf("charger_power = %v, must be a whole number for TeslaMate's :integer cast", *p)
			}
			if *p != tc.want {
				t.Errorf("charger_power = %v, want %v", *p, tc.want)
			}
		})
	}

	// JSON proof: a zero/charging frame emits charger_power as numeric 0, not null
	// or a blank token.
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	b, err := json.Marshal(out.ChargeState)
	if err != nil {
		t.Fatalf("marshal charge_state: %v", err)
	}
	if !strings.Contains(string(b), `"charger_power":0`) {
		t.Errorf(`charger_power did not serialize as numeric 0; got %s`, b)
	}
}

// --- software_update status mapping ---------------------------------------

func TestSoftwareUpdateStatus(t *testing.T) {
	t.Parallel()
	cases := []struct{ ota, want string }{
		{"installing", "installing"},
		{"downloading", "downloading"},
		{"ready_to_download", "downloading"},
		{"ready_to_install", "available"},
		{"scheduled_to_install", "available"},
		{"awaiting_install", "available"},
		{"install_countdown", "available"},
		{"preparing", "available"},
		{"idle", ""},
		{"install_success", ""},
		{"", ""},
		{"Installing", "installing"}, // case-insensitive robustness
	}
	for _, c := range cases {
		vs := &vehicle.State{OtaStatus: c.ota, OtaAvailableVersion: "2026.10.1", Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime}
		out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
		if got := deref(out.VehicleState.SoftwareUpdate.Status); got != c.want {
			t.Errorf("ota %q: status = %q, want %q", c.ota, got, c.want)
		}
	}
}

// --- doors/locks -> ints/bool ---------------------------------------------

func TestDoorsAndLocks(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{
		DoorFrontLeftLocked:   "locked",
		DoorFrontRightLocked:  "locked",
		DoorRearLeftLocked:    "locked",
		DoorRearRightLocked:   "locked",
		DoorFrontLeftClosed:   "open", // driver door open
		DoorFrontRightClosed:  "closed",
		FrunkClosed:           "open", // frunk open
		WindowFrontLeftClosed: "open",
		Power:                 vehicle.PowerOnline,
		UserPresent:           true,
		LastUpdate:            fixedTime,
	}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	if !deref(out.VehicleState.Locked) {
		t.Error("all doors locked => locked=true")
	}
	if deref(out.VehicleState.Df) != 1 {
		t.Error("driver door open => df=1")
	}
	if deref(out.VehicleState.Pf) != 0 {
		t.Error("passenger door closed => pf=0")
	}
	if deref(out.VehicleState.Ft) != 1 {
		t.Error("frunk open => ft=1")
	}
	if deref(out.VehicleState.FdWindow) != 1 {
		t.Error("front-left window open => fd_window=1")
	}

	// One unlocked door => locked=false.
	vs.DoorRearRightLocked = "unlocked"
	out2 := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	if deref(out2.VehicleState.Locked) {
		t.Error("one unlocked door => locked=false")
	}
}

// --- hold-last-known ------------------------------------------------------

func TestHoldLastKnown(t *testing.T) {
	t.Parallel()
	// Good snapshot first.
	good := &vehicle.State{
		Gear:            vehicle.GearDrive,
		OdometerMeters:  16093,
		RangeKm:         161,
		BatteryLevelPct: 72,
		Location:        &vehicle.Location{Latitude: 37.5, Longitude: -122.3, TimeStamp: fixedTime},
		Power:           vehicle.PowerOnline,
		UserPresent:     true,
		LastUpdate:      fixedTime,
	}
	prev := VehicleData(nil, good, nil, testIDs, testCfg, time.Time{}, nil)

	// Junk snapshot: gear/mileage/range dropped to zero (sentinel scrubbed),
	// no location. Load-bearing fields should hold last-known.
	junk := &vehicle.State{
		Gear:           vehicle.GearUnknown, // dropped
		OdometerMeters: 0,                   // dropped
		RangeKm:        0,                   // dropped
		Location:       nil,
		Power:          vehicle.PowerOnline,
		UserPresent:    true,
		LastUpdate:     fixedTime,
	}
	out := VehicleData(&prev, junk, nil, testIDs, testCfg, time.Time{}, nil)

	if got := deref(out.DriveState.ShiftState); got != "D" {
		t.Errorf("hold shift_state = %q, want held D", got)
	}
	if got := deref(out.VehicleState.Odometer); !approx(got, deref(prev.VehicleState.Odometer)) {
		t.Errorf("hold odometer = %v, want held %v", got, deref(prev.VehicleState.Odometer))
	}
	if got := deref(out.ChargeState.IdealBatteryRange); !approx(got, deref(prev.ChargeState.IdealBatteryRange)) {
		t.Errorf("hold range = %v, want held %v", got, deref(prev.ChargeState.IdealBatteryRange))
	}
	// battery_level/usable_battery_level dropped to 0 (asleep/offline summary) must
	// hold last-known, not emit a false 0 (the phantom SoC-dip-to-0 bug).
	if got := deref(out.ChargeState.BatteryLevel); got != 72 {
		t.Errorf("hold battery_level = %d, want held 72", got)
	}
	if got := deref(out.ChargeState.UsableBatteryLevel); got != 72 {
		t.Errorf("hold usable_battery_level = %d, want held 72", got)
	}
	if got := deref(out.DriveState.Latitude); !approx(got, 37.5) {
		t.Errorf("hold latitude = %v, want held 37.5", got)
	}
}

// --- all five sub-objects always non-null ---------------------------------

func TestSubObjectsNonNull(t *testing.T) {
	t.Parallel()
	// nil snapshot (first poll pending) still yields a valid asleep payload.
	out := VehicleData(nil, nil, nil, testIDs, testCfg, time.Time{}, nil)
	if out.State != "asleep" {
		t.Errorf("nil snapshot state = %q, want asleep", out.State)
	}
	if out.ChargeState == nil || out.ClimateState == nil || out.DriveState == nil || out.VehicleConfig == nil || out.VehicleState == nil {
		t.Fatal("all five sub-objects must be non-null even with nil snapshot")
	}
	if out.VehicleConfig.CarType == nil {
		t.Error("vehicle_config.car_type must be non-null")
	}
	if out.ChargeState.ChargeEnergyAdded == nil || out.ChargeState.IdealBatteryRange == nil {
		t.Error("charge_energy_added and ideal_battery_range must be non-null")
	}
}

// --- climate / seat heaters -----------------------------------------------

func TestSeatHeaters(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{
		SeatHeatFrontLeft:  "Off",
		SeatHeatFrontRight: "Level_2",
		SeatHeatRearLeft:   "Level_3",
		SeatHeatRearRight:  "", // unknown -> nil
		Power:              vehicle.PowerOnline,
		UserPresent:        true,
		LastUpdate:         fixedTime,
	}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	cs := out.ClimateState
	require.NotNil(t, cs)

	require.NotNil(t, cs.SeatHeaterLeft)
	assert.Equal(t, 0, *cs.SeatHeaterLeft, "Off -> 0")
	require.NotNil(t, cs.SeatHeaterRight)
	assert.Equal(t, 2, *cs.SeatHeaterRight, "Level_2 -> 2")
	require.NotNil(t, cs.SeatHeaterRearLeft)
	assert.Equal(t, 3, *cs.SeatHeaterRearLeft, "Level_3 -> 3")
	assert.Nil(t, cs.SeatHeaterRearRight, "empty/unknown -> nil")
}

func TestSteeringWheelHeater(t *testing.T) {
	t.Parallel()
	on := VehicleData(nil, &vehicle.State{
		SteeringWheelHeat: "Level_1", Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime,
	}, nil, testIDs, testCfg, time.Time{}, nil)
	require.NotNil(t, on.ClimateState.SteeringWheelHeater)
	assert.True(t, *on.ClimateState.SteeringWheelHeater, "Level_1 -> true")

	off := VehicleData(nil, &vehicle.State{
		SteeringWheelHeat: "Off", Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime,
	}, nil, testIDs, testCfg, time.Time{}, nil)
	require.NotNil(t, off.ClimateState.SteeringWheelHeater)
	assert.False(t, *off.ClimateState.SteeringWheelHeater, "Off -> false")
}

func TestClimateOnAndPreconditioning(t *testing.T) {
	t.Parallel()
	active := VehicleData(nil, &vehicle.State{
		PreconditioningStatus: "active", Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime,
	}, nil, testIDs, testCfg, time.Time{}, nil)
	require.NotNil(t, active.ClimateState.IsPreconditioning)
	assert.True(t, *active.ClimateState.IsPreconditioning)
	require.NotNil(t, active.ClimateState.IsClimateOn)
	assert.True(t, *active.ClimateState.IsClimateOn, "preconditioning active -> is_climate_on true")

	off := VehicleData(nil, &vehicle.State{
		PreconditioningStatus: "off", Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime,
	}, nil, testIDs, testCfg, time.Time{}, nil)
	assert.False(t, deref(off.ClimateState.IsPreconditioning))
	assert.False(t, deref(off.ClimateState.IsClimateOn))
}

func TestFrontDefrosterAndDriverTemp(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{
		DefrostStatus:   "On",
		DriverSetpointC: 21.5,
		Power:           vehicle.PowerOnline,
		UserPresent:     true,
		LastUpdate:      fixedTime,
	}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	require.NotNil(t, out.ClimateState.IsFrontDefrosterOn)
	assert.True(t, *out.ClimateState.IsFrontDefrosterOn)
	require.NotNil(t, out.ClimateState.DriverTempSetting)
	assert.InDelta(t, 21.5, *out.ClimateState.DriverTempSetting, 0.01)

	// Unreported setpoint -> nil; defrost Off -> false.
	out2 := VehicleData(nil, &vehicle.State{
		DefrostStatus: "Off", Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime,
	}, nil, testIDs, testCfg, time.Time{}, nil)
	assert.Nil(t, out2.ClimateState.DriverTempSetting, "zero setpoint -> nil")
	assert.False(t, deref(out2.ClimateState.IsFrontDefrosterOn))
}

func TestSentryMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status string
		want   bool
	}{
		{"enabled", true},
		{"engaged", true},
		{"disabled", false},
		{"", false},
		{"Enabled", true}, // case-insensitive
	}
	for _, c := range cases {
		vs := &vehicle.State{GearGuardStatus: c.status, Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime}
		out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
		require.NotNil(t, out.VehicleState.SentryMode)
		assert.Equalf(t, c.want, *out.VehicleState.SentryMode, "gearGuardStatus %q", c.status)
	}
}

func TestSoftwareUpdatePerc(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{
		OtaStatus:           "installing",
		OtaAvailableVersion: "2026.10.1",
		OtaDownloadProgress: 100,
		OtaInstallProgress:  42.6,
		Power:               vehicle.PowerOnline,
		UserPresent:         true,
		LastUpdate:          fixedTime,
	}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	su := out.VehicleState.SoftwareUpdate
	require.NotNil(t, su)
	require.NotNil(t, su.DownloadPerc)
	assert.Equal(t, 100, *su.DownloadPerc)
	require.NotNil(t, su.InstallPerc)
	assert.Equal(t, 43, *su.InstallPerc, "42.6 rounds to 43")
}

func TestTpmsSoftWarning(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{
		TpmsFrontLeft:  "LOW",
		TpmsFrontRight: "OK",
		TpmsRearLeft:   "", // unknown
		TpmsRearRight:  "OK",
		Power:          vehicle.PowerOnline,
		UserPresent:    true,
		LastUpdate:     fixedTime,
	}
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	vst := out.VehicleState

	require.NotNil(t, vst.TpmsSoftWarningFl)
	assert.True(t, *vst.TpmsSoftWarningFl, "LOW -> warning true")
	require.NotNil(t, vst.TpmsSoftWarningFr)
	assert.False(t, *vst.TpmsSoftWarningFr, "OK -> warning false")
	assert.Nil(t, vst.TpmsSoftWarningRl, "empty status -> nil (unknown)")

	// Numeric pressures remain nil (subscription-only, never requested).
	assert.Nil(t, vst.TpmsPressureFl)
	assert.Nil(t, vst.TpmsPressureFr)
	assert.Nil(t, vst.TpmsPressureRl)
	assert.Nil(t, vst.TpmsPressureRr)
}

// --- expected-snapshot JSON -----------------------------------------------

// update regenerates the expected snapshot files when set: UPDATE_SNAPSHOTS=1 go test
var update = os.Getenv("UPDATE_SNAPSHOTS") != ""

func TestDrivingSnapshot(t *testing.T) {
	t.Parallel()
	vs := drivingSnapshot()
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	assertExpectedSnapshot(t, "driving.json", out)
}

// TestDrivePower pins the sink emit of drive_state.power (TeslaMate
// drives.power_max): a present canonical value (incl. negative regen) passes
// through unchanged, and an absent value emits null.
func TestDrivePower(t *testing.T) {
	t.Parallel()
	base := func() *vehicle.State {
		return &vehicle.State{Gear: vehicle.GearDrive, Power: vehicle.PowerOnline, UserPresent: true, LastUpdate: fixedTime}
	}

	vs := base()
	vs.DrivePowerKw = ptrF64(123)
	out := VehicleData(nil, vs, nil, testIDs, testCfg, time.Time{}, nil)
	if p := out.DriveState.Power; p == nil || !approx(*p, 123) {
		t.Fatalf("drive power = %v, want 123", out.DriveState.Power)
	}

	regen := base()
	regen.DrivePowerKw = ptrF64(-31)
	out = VehicleData(nil, regen, nil, testIDs, testCfg, time.Time{}, nil)
	if p := out.DriveState.Power; p == nil || !approx(*p, -31) {
		t.Fatalf("regen drive power = %v, want -31", out.DriveState.Power)
	}

	out = VehicleData(nil, base(), nil, testIDs, testCfg, time.Time{}, nil)
	if out.DriveState.Power != nil {
		t.Fatalf("absent drive power = %v, want nil", *out.DriveState.Power)
	}
}

func TestChargingSnapshot(t *testing.T) {
	t.Parallel()
	vs := chargingSnapshot()
	live := &vehicle.LiveSession{
		PowerKw: 175.0, CurrentA: 220, TotalChargedEnergy: 23.4, TimeRemainingSec: 1800,
	}
	out := VehicleData(nil, vs, live, testIDs, testCfg, time.Time{}, nil)
	assertExpectedSnapshot(t, "charging.json", out)
}

// TestACvsDCCharging locks the AC/DC inference and, critically, that DC charging
// does not emit AC voltage*current that overflows TeslaMate's smallint phase
// detection (which crashed charge finalization).
func TestACvsDCCharging(t *testing.T) {
	t.Parallel()
	vs := &vehicle.State{
		Charger: vehicle.ChargerCharging, Plug: vehicle.PlugConnected,
		BatteryLevelPct: 50, Power: vehicle.PowerOnline, LastUpdate: fixedTime,
	}

	// DC fast: high power -> fast_charger_present, no AC phases/voltage/current.
	dc := VehicleData(nil, vs, &vehicle.LiveSession{PowerKw: 175, CurrentA: 220, TotalChargedEnergy: 5}, testIDs, testCfg, time.Time{}, nil)
	cs := dc.ChargeState
	if cs.FastChargerPresent == nil || !*cs.FastChargerPresent {
		t.Error("DC: expected fast_charger_present=true")
	}
	if cs.ChargerPhases != nil || cs.ChargerVoltage != nil || cs.ChargerActualCurrent != nil {
		t.Errorf("DC: AC fields must be nil to avoid smallint overflow, got phases=%v volt=%v amp=%v",
			cs.ChargerPhases, cs.ChargerVoltage, cs.ChargerActualCurrent)
	}
	if cs.ChargerPower == nil || *cs.ChargerPower != 175 {
		t.Error("DC: expected charger_power=175")
	}

	// AC: low power -> phases + voltage set, and voltage*current stays < smallint max.
	ac := VehicleData(nil, vs, &vehicle.LiveSession{PowerKw: 11, CurrentA: 48, TotalChargedEnergy: 2}, testIDs, testCfg, time.Time{}, nil)
	cs = ac.ChargeState
	if cs.FastChargerPresent == nil || *cs.FastChargerPresent {
		t.Error("AC: expected fast_charger_present=false")
	}
	if cs.ChargerPhases == nil || cs.ChargerVoltage == nil || cs.ChargerActualCurrent == nil {
		t.Fatal("AC: expected phases, voltage, current to be set")
	}
	if prod := *cs.ChargerVoltage * *cs.ChargerActualCurrent; prod >= 32768 {
		t.Errorf("AC: voltage*current=%d overflows smallint", prod)
	}
}

func drivingSnapshot() *vehicle.State {
	return &vehicle.State{
		Power:               vehicle.PowerOnline,
		UserPresent:         true,
		Gear:                vehicle.GearDrive,
		Location:            &vehicle.Location{Latitude: 37.7749, Longitude: -122.4194, TimeStamp: fixedTime},
		SpeedMps:            20, // m/s ~ 44.7 mph
		HeadingDeg:          90,
		OdometerMeters:      20000000, // 20,000 km in meters ~ 12427 mi
		RangeKm:             320,      // km ~ 198.8 mi
		BatteryLevelPct:     78,
		BatteryLimitPct:     90,
		CabinTempC:          22.0,
		DoorFrontLeftLocked: "unlocked",
		OtaVersion:          "2026.6.1",
		LastUpdate:          fixedTime,
	}
}

func chargingSnapshot() *vehicle.State {
	return &vehicle.State{
		Power:                vehicle.PowerOnline,
		Gear:                 vehicle.GearPark,
		Location:             &vehicle.Location{Latitude: 37.4, Longitude: -122.1, TimeStamp: fixedTime},
		OdometerMeters:       20000000,
		RangeKm:              260, // km
		BatteryLevelPct:      64,
		BatteryLimitPct:      90,
		CabinTempC:           20.0,
		Charger:              vehicle.ChargerCharging,
		Plug:                 vehicle.PlugConnected,
		ChargePortOpen:       true,
		TimeToEndOfChargeMin: 30,
		DoorFrontLeftLocked:  "locked",
		DoorFrontRightLocked: "locked",
		DoorRearLeftLocked:   "locked",
		DoorRearRightLocked:  "locked",
		OtaVersion:           "2026.6.1",
		LastUpdate:           fixedTime,
	}
}

func assertExpectedSnapshot(t *testing.T, name string, out wire.VehicleData) {
	t.Helper()
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if update {
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read expected snapshot %s (run with UPDATE_SNAPSHOTS=1 to create): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("snapshot %s mismatch.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
