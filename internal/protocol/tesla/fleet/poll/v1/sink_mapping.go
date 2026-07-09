// Package translate holds the pure functions that map a canonical vehicle
// snapshot (internal/vehicle State + optional LiveSession) into the Tesla Fleet
// API vehicle_data shape stock TeslaMate consumes. This is the canonical->Tesla
// half of the hub: a source plugin has already normalized its vendor wire shape
// into the neutral vehicle.State, so everything here is a pure function of the
// canonical inputs (no I/O, no clock beyond what the caller passes), and the
// mapping is exhaustively unit and snapshot tested in translate_test.go.
//
// Three classes of transform live here:
//   - unit conversions (canonical is SI/metric; Tesla wants imperial).
//     Centralized, tested converters: meters->miles, km->miles, m/s->mph.
//   - enum lookups (canonical typed enums -> the exact load-bearing Tesla
//     strings the FSM branches on). Unknown values fall back to a safe default.
//   - hold-last-known: when a load-bearing canonical field arrived zero/empty
//     (the source dropped a junk sentinel), reuse prev's translated value rather
//     than emitting a null or wrong value that would break drive/charge
//     detection.
package v1

import (
	"log"
	"math"
	"strings"
	"time"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// IDs carries the resolved Tesla-side identity for a vehicle (minted by idmap)
// plus the display fields, so VehicleData is a pure function with no idmap
// dependency.
type IDs struct {
	ID          int64
	VehicleID   int64
	VIN         string
	DisplayName string
}

// Cfg carries the fuzzy constants the mapping needs from configuration.
type Cfg struct {
	// CarType is the Tesla car_type placeholder (e.g. "model3"); vehicle_config
	// must be non-null with a car_type or the FSM crashes.
	CarType string
	// Model is the source model string (R1T/R1S/...) used for trim_badging.
	Model string
}

// ShiftState maps the canonical Gear to the Tesla shift_state the FSM branches
// on. Park/unknown -> "P". Exported so the streaming data:update encoder
// (internal/protocol/tesla/stream/v1) reuses the same mapping (DRY).
func ShiftState(gear vehicle.Gear) string {
	switch gear {
	case vehicle.GearDrive:
		return "D"
	case vehicle.GearReverse:
		return "R"
	case vehicle.GearNeutral:
		return "N"
	default: // GearPark, GearUnknown
		return "P"
	}
}

// TopState maps the canonical Power to the Tesla top-level state the FSM
// branches on: online / asleep / offline. Exported for reuse by the streaming
// sink.
func TopState(p vehicle.Power) string {
	switch p {
	case vehicle.PowerOnline:
		return "online"
	case vehicle.PowerSleep:
		return "asleep"
	default: // PowerOffline, PowerUnknown
		return "offline"
	}
}

// chargingState maps the canonical charger/plug enums to the Tesla
// charging_state the FSM branches on: Charging / Stopped / Disconnected.
func chargingState(c vehicle.ChargerState, plug vehicle.ChargePlug) string {
	if plug == vehicle.PlugDisconnected {
		return "Disconnected"
	}
	switch c {
	case vehicle.ChargerCharging:
		return "Charging"
	case vehicle.ChargerIdle:
		return "Stopped"
	case vehicle.ChargerDisconnect:
		return "Disconnected"
	default: // ChargerUnknown
		return "Disconnected"
	}
}

// softwareUpdateStatus maps a source's OTA status to the Tesla
// software_update.status the FSM branches on: installing / downloading /
// available / "". Match the real raw values case-insensitively.
func softwareUpdateStatus(otaStatus string) string {
	switch strings.ToLower(otaStatus) {
	case "installing":
		return "installing"
	case "downloading", "ready_to_download":
		return "downloading"
	case "ready_to_install", "scheduled_to_install", "awaiting_install", "install_countdown", "preparing":
		return "available"
	default:
		return ""
	}
}

// openClosedToInt maps a door/window/closure "open"/"closed" to the Tesla 1/0
// ints. Empty/unknown -> 0 (closed) so we never report a phantom-open panel.
func openClosedToInt(v string) int {
	if v == "open" {
		return 1
	}
	return 0
}

// allLocked reports whether every provided lock string is "locked". An empty
// string (no reading) is treated as not-locked so we don't over-report secure.
func allLocked(locks ...string) bool {
	for _, l := range locks {
		if l != "locked" {
			return false
		}
	}
	return len(locks) > 0
}

// VehicleData is the single source of truth mapping a canonical snapshot to the
// Tesla vehicle_data payload. prev is the previously translated payload (nil on
// first call) and enables hold-last-known for load-bearing fields whose source
// arrived empty/zero. live may be nil (no active charging session).
func VehicleData(prev *wire.VehicleData, vs *vehicle.State, live *vehicle.LiveSession, ids IDs, cfg Cfg, asOf time.Time, logger *log.Logger) wire.VehicleData {
	// With no snapshot at all, serve a minimal asleep payload (all five
	// sub-objects non-null) so TeslaMate doesn't discard or error.
	if vs == nil {
		return asleepFallback(prev, ids, cfg)
	}

	state := TopState(vs.Power)

	// Snapshot timestamp: the canonical monotonic AsOf the cache stamped -- the
	// SAME value the streaming sink emits, so the two surfaces never disagree and
	// TeslaMate can't discard a poll as stale mid-drive or write end < start at a
	// transition. Fall back to the GPS fix / LastUpdate / now heuristic only when
	// AsOf is unset (un-wired callers, e.g. direct unit tests).
	snapTime := asOf
	if snapTime.IsZero() {
		snapTime = vs.LastUpdate
		if vs.Location != nil && !vs.Location.TimeStamp.IsZero() {
			snapTime = vs.Location.TimeStamp
		}
		if snapTime.IsZero() {
			snapTime = time.Now()
		}
	}
	nowMs := snapTime.UnixMilli()

	out := wire.VehicleData{
		ID:          ids.ID,
		VehicleID:   ids.VehicleID,
		VIN:         ids.VIN,
		DisplayName: ids.DisplayName,
		State:       state,
		APIVersion:  ptrInt(undocumentedAPIVersion),
		InService:   ptrBool(false),
	}

	out.DriveState = driveState(prev, vs, nowMs)
	out.ChargeState = chargeState(prev, vs, live, nowMs)
	out.ClimateState = climateState(vs, nowMs)
	out.VehicleState = vehicleState(prev, vs, ids, nowMs)
	out.VehicleConfig = vehicleConfig(prev, vs, cfg, nowMs)

	return out
}

// State derives just the top-level Tesla state ("online"/"asleep"/"offline")
// from a snapshot, matching the State field VehicleData would produce. It lets
// the cheap products/summary/status paths get the state without running the full
// translation (all five sub-objects) or touching hold-last-known.
func State(vs *vehicle.State) string {
	if vs == nil {
		return "asleep"
	}
	return TopState(vs.Power)
}

func driveState(prev *wire.VehicleData, vs *vehicle.State, nowMs int64) *wire.DriveState {
	ds := &wire.DriveState{
		Timestamp: ptrI64(nowMs),
	}
	// drive_state.power (kW, signed): feeds TeslaMate drives.power_max. Only the
	// poll source carries it (Fleet Telemetry has no drive-power field); nil when
	// absent. Already kW, so pass straight through.
	if vs.DrivePowerKw != nil {
		ds.Power = ptrF64(*vs.DrivePowerKw)
	}

	// shift_state is load-bearing. Unknown gear => hold last-known.
	if vs.Gear == vehicle.GearUnknown && prev != nil && prev.DriveState != nil && prev.DriveState.ShiftState != nil {
		ds.ShiftState = prev.DriveState.ShiftState
	} else {
		ss := ShiftState(vs.Gear)
		ds.ShiftState = ptrStr(ss)
	}

	// Location: hold last-known when absent so positions stay continuous.
	if vs.Location != nil {
		ds.Latitude = ptrF64(vs.Location.Latitude)
		ds.Longitude = ptrF64(vs.Location.Longitude)
		ds.GpsAsOf = ptrI64(vs.Location.TimeStamp.UnixMilli() / 1000)
	} else if prev != nil && prev.DriveState != nil {
		ds.Latitude = prev.DriveState.Latitude
		ds.Longitude = prev.DriveState.Longitude
		ds.GpsAsOf = ptrI64(nowMs / 1000)
	} else {
		ds.GpsAsOf = ptrI64(nowMs / 1000)
	}

	ds.Heading = ptrInt(int(vs.HeadingDeg))
	ds.Speed = ptrF64(units.MpsToMph(vs.SpeedMps))

	return ds
}

func chargeState(prev *wire.VehicleData, vs *vehicle.State, live *vehicle.LiveSession, nowMs int64) *wire.ChargeState {
	// battery_level is load-bearing and a reporting car is never at 0% (it bricks
	// well above 0). A zero is a junk reading -- e.g. the asleep/offline summary
	// snapshot carries no SOC -- so hold last-known rather than emit a false 0,
	// mirroring the range/odometer guards below. Without this, a stream-recycle
	// offline->online flip serves the cached 0 and TeslaMate logs a phantom SOC dip.
	level := ptrInt(int(vs.BatteryLevelPct))
	if vs.BatteryLevelPct == 0 && prev != nil && prev.ChargeState != nil && prev.ChargeState.BatteryLevel != nil {
		level = prev.ChargeState.BatteryLevel
	}
	cs := &wire.ChargeState{
		Timestamp:    ptrI64(nowMs),
		BatteryLevel: level,
	}
	cs.UsableBatteryLevel = level
	// charge_limit_soc is load-bearing and never legitimately 0 (the user minimum is
	// ~50%). A 0 is a junk reading from an asleep/offline summary snapshot, so hold
	// last-known rather than log a phantom charge-limit dip to 0% and back, mirroring
	// the battery_level/range guards.
	cs.ChargeLimitSoc = ptrInt(int(vs.BatteryLimitPct))
	if vs.BatteryLimitPct == 0 && prev != nil && prev.ChargeState != nil && prev.ChargeState.ChargeLimitSoc != nil {
		cs.ChargeLimitSoc = prev.ChargeState.ChargeLimitSoc
	}
	cs.BatteryHeaterOn = ptrBool(vs.BatteryHeaterOn)

	// Range is load-bearing and MUST be non-null. The three range fields always
	// carry the same value, so derive one pointer: the fresh reading, or the
	// held-last-known value when the current reading is junk (zero).
	rng := ptrF64(units.KmToMiles(float64(vs.RangeKm)))
	if vs.RangeKm == 0 && prev != nil && prev.ChargeState != nil && prev.ChargeState.IdealBatteryRange != nil {
		rng = prev.ChargeState.IdealBatteryRange
	}
	cs.IdealBatteryRange = rng
	cs.EstBatteryRange = rng
	cs.BatteryRange = rng

	cs.ChargingState = ptrStr(chargingState(vs.Charger, vs.Plug))
	cs.ChargePortDoorOpen = ptrBool(vs.ChargePortOpen)

	if live != nil {
		// charger_power must be whole: TeslaMate casts it to a DB :integer, and a
		// fractional AC value (e.g. 7.7 kW) fails the cast ("Invalid charge data").
		cs.ChargerPower = ptrF64(math.Round(live.PowerKw))
		cs.ChargerActualCurrent = ptrInt(int(live.CurrentA))
		// charge_energy_added is load-bearing and MUST be non-null.
		cs.ChargeEnergyAdded = ptrF64(live.TotalChargedEnergy)
		// time_to_full: prefer the live session's seconds; a streamed charge carries
		// PowerKw (live != nil) but no TimeRemainingSec, so fall back to the canonical
		// minutes (StreamTimeToFull) before giving up and emitting 0.
		switch {
		case live.TimeRemainingSec > 0:
			cs.TimeToFullCharge = ptrF64(float64(live.TimeRemainingSec) / 3600.0)
		case vs.TimeToEndOfChargeMin > 0:
			cs.TimeToFullCharge = ptrF64(float64(vs.TimeToEndOfChargeMin) / 60.0)
		default:
			cs.TimeToFullCharge = ptrF64(0)
		}
	} else {
		cs.ChargerPower = ptrF64(0)
		cs.ChargerActualCurrent = ptrInt(0)
		// Fallback: keep charge_energy_added non-null. Without a live session we
		// have no reliable cumulative energy, so carry the previous value (so an
		// interrupted session doesn't reset to 0) or 0 on first sight. We do NOT
		// synthesize energy from ΔSOC × pack capacity: a fabricated value would
		// feed straight into TeslaMate's efficiency/capacity derivation (which keys
		// on charge_energy_added over a >10 min session) and corrupt it. The metered
		// live session is the only trustworthy source; absent it, hold last-known.
		if prev != nil && prev.ChargeState != nil && prev.ChargeState.ChargeEnergyAdded != nil {
			cs.ChargeEnergyAdded = prev.ChargeState.ChargeEnergyAdded
		} else {
			cs.ChargeEnergyAdded = ptrF64(0)
		}
		if vs.TimeToEndOfChargeMin > 0 {
			cs.TimeToFullCharge = ptrF64(float64(vs.TimeToEndOfChargeMin) / 60.0)
		} else {
			cs.TimeToFullCharge = ptrF64(0)
		}
	}
	cs.ChargeRate = ptrF64(0)

	// AC vs DC: prefer the source's fast-charger flag (a tapered DC session stays DC
	// even when its delivered power drops below the threshold), falling back to
	// delivered power when no flag is set. DC fast charging is high power and
	// single-stage (no AC phases); AC charging is lower power across mains phases.
	// TeslaMate uses fast_charger_present + charger_phases to categorize the session.
	// Mirrors poll.isDC so the cadence layer and the sink agree on the same session.
	if live != nil && (live.FastCharger || live.PowerKw >= dcChargeThresholdKw) {
		// DC fast charging. AC voltage/current/phases are not meaningful here,
		// and crucially TeslaMate's phase detection multiplies
		// charger_actual_current * charger_voltage in a smallint column: real DC
		// values (e.g. 220 A * 400 V = 88000) overflow smallint and crash charge
		// finalization. Leave them unset; charger_power carries DC energy.
		cs.FastChargerPresent = ptrBool(true)
		cs.FastChargerType = ptrStr("Combo")
		cs.FastChargerBrand = ptrStr("Rivian")
		cs.ChargerPhases = nil
		cs.ChargerVoltage = nil
		cs.ChargerActualCurrent = nil
	} else if live != nil && live.PowerKw > 0 {
		// AC charging: report mains phases + voltage so TeslaMate computes AC
		// energy. Keep current*voltage within smallint range (AC <= ~48 A).
		cs.FastChargerPresent = ptrBool(false)
		cs.ChargerPhases = ptrInt(2)
		cs.ChargerVoltage = ptrInt(240)
	} else {
		cs.FastChargerPresent = ptrBool(false)
	}

	return cs
}

// dcChargeThresholdKw is the power above which we treat a session as DC fast
// charging rather than AC. AC charging tops out around 11 kW; DC fast charging
// is tens to >200 kW. Mirrors defaultDCThresholdKw in internal/poll; kept
// duplicated rather than coupling two unrelated packages over one constant.
const dcChargeThresholdKw = 25.0

func climateState(vs *vehicle.State, nowMs int64) *wire.ClimateState {
	cs := &wire.ClimateState{
		InsideTemp: ptrF64(vs.CabinTempC), // already °C
		Timestamp:  ptrI64(nowMs),
	}
	// 0 = unreported (sources that don't expose ambient temp, e.g. Rivian). A car
	// genuinely at 0.0 °C ambient also reads as unreported here; rare enough to accept.
	if vs.OutsideTempC != 0 {
		cs.OutsideTemp = ptrF64(vs.OutsideTempC)
	}

	// Seat heaters: source "Off"/"Level_1..3" -> Tesla 0..3 (nil when unknown).
	cs.SeatHeaterLeft = seatHeatLevel(vs.SeatHeatFrontLeft)
	cs.SeatHeaterRight = seatHeatLevel(vs.SeatHeatFrontRight)
	cs.SeatHeaterRearLeft = seatHeatLevel(vs.SeatHeatRearLeft)
	cs.SeatHeaterRearRight = seatHeatLevel(vs.SeatHeatRearRight)

	// Steering wheel heater: any non-Off level is on.
	cs.SteeringWheelHeater = ptrBool(isOn(vs.SteeringWheelHeat))

	// Front defroster: any non-Off state is on.
	cs.IsFrontDefrosterOn = ptrBool(isOn(vs.DefrostStatus))

	// Preconditioning: {active, complete_maintain, initiate} are "running". When
	// on, climate is on.
	pre := preconditioningActive(vs.PreconditioningStatus)
	cs.IsPreconditioning = ptrBool(pre)
	cs.IsClimateOn = ptrBool(pre)

	// Driver setpoint (°C), pass through like inside_temp; nil when not reported.
	if vs.DriverSetpointC != 0 {
		cs.DriverTempSetting = ptrF64(vs.DriverSetpointC)
	}

	return cs
}

// seatHeatLevel maps source seat-heat strings ("Off"/"Level_1".."Level_3") to
// the Tesla 0..3 seat_heater level. Unknown/empty -> nil (null when unknown).
func seatHeatLevel(v string) *int {
	switch v {
	case "Off":
		return ptrInt(0)
	case "Level_1":
		return ptrInt(1)
	case "Level_2":
		return ptrInt(2)
	case "Level_3":
		return ptrInt(3)
	default:
		return nil
	}
}

// isOn reports whether an "Off"/"Level_x" style control is engaged: any
// non-empty value other than "Off" (case-insensitive) is on.
func isOn(v string) bool {
	return v != "" && !strings.EqualFold(v, "Off")
}

// preconditioningActive reports whether the preconditioning status indicates the
// cabin is actively preconditioning: {active, complete_maintain, initiate}.
func preconditioningActive(status string) bool {
	switch strings.ToLower(status) {
	case "active", "complete_maintain", "initiate":
		return true
	default:
		return false
	}
}

func vehicleState(prev *wire.VehicleData, vs *vehicle.State, ids IDs, nowMs int64) *wire.VehicleState {
	out := &wire.VehicleState{
		Timestamp:     ptrI64(nowMs),
		VehicleName:   ptrStr(ids.DisplayName),
		IsUserPresent: ptrBool(vs.UserPresent),
		// Gear Guard is Rivian's Sentry equivalent: armed/recording when the
		// video feature is enabled or actively engaged.
		SentryMode: ptrBool(gearGuardArmed(vs.GearGuardStatus)),
	}

	// TPMS: the pollable status strings carry "OK" when healthy; any other
	// non-empty value is a soft warning. Unknown/empty -> nil.
	out.TpmsSoftWarningFl = tpmsWarning(vs.TpmsFrontLeft)
	out.TpmsSoftWarningFr = tpmsWarning(vs.TpmsFrontRight)
	out.TpmsSoftWarningRl = tpmsWarning(vs.TpmsRearLeft)
	out.TpmsSoftWarningRr = tpmsWarning(vs.TpmsRearRight)

	// Numeric pressures (bar): subscription-only via REST (zero), but streamed
	// from Fleet Telemetry. Emit only a nonzero reading so an unstreamed car stays nil.
	if vs.TpmsPressureFlBar != 0 {
		out.TpmsPressureFl = ptrF64(vs.TpmsPressureFlBar)
	}
	if vs.TpmsPressureFrBar != 0 {
		out.TpmsPressureFr = ptrF64(vs.TpmsPressureFrBar)
	}
	if vs.TpmsPressureRlBar != 0 {
		out.TpmsPressureRl = ptrF64(vs.TpmsPressureRlBar)
	}
	if vs.TpmsPressureRrBar != 0 {
		out.TpmsPressureRr = ptrF64(vs.TpmsPressureRrBar)
	}

	// odometer is load-bearing (numeric) and MUST be non-null. Hold last-known
	// when the raw mileage arrived zero (junk dropped upstream).
	if vs.OdometerMeters == 0 && prev != nil && prev.VehicleState != nil && prev.VehicleState.Odometer != nil {
		out.Odometer = prev.VehicleState.Odometer
	} else {
		out.Odometer = ptrF64(units.MetersToMiles(float64(vs.OdometerMeters)))
	}

	out.Locked = ptrBool(allLocked(
		vs.DoorFrontLeftLocked, vs.DoorFrontRightLocked,
		vs.DoorRearLeftLocked, vs.DoorRearRightLocked,
	))

	out.Df = ptrInt(openClosedToInt(invertClosed(vs.DoorFrontLeftClosed)))
	out.Dr = ptrInt(openClosedToInt(invertClosed(vs.DoorRearLeftClosed)))
	out.Pf = ptrInt(openClosedToInt(invertClosed(vs.DoorFrontRightClosed)))
	out.Pr = ptrInt(openClosedToInt(invertClosed(vs.DoorRearRightClosed)))
	out.Ft = ptrInt(openClosedToInt(invertClosed(vs.FrunkClosed)))
	out.Rt = ptrInt(openClosedToInt(invertClosed(vs.LiftgateClosed)))

	out.FdWindow = ptrInt(openClosedToInt(invertClosed(vs.WindowFrontLeftClosed)))
	out.FpWindow = ptrInt(openClosedToInt(invertClosed(vs.WindowFrontRightClosed)))
	out.RdWindow = ptrInt(openClosedToInt(invertClosed(vs.WindowRearLeftClosed)))
	out.RpWindow = ptrInt(openClosedToInt(invertClosed(vs.WindowRearRightClosed)))

	if vs.OtaVersion != "" {
		out.CarVersion = ptrStr(vs.OtaVersion)
	} else if prev != nil && prev.VehicleState != nil && prev.VehicleState.CarVersion != nil {
		out.CarVersion = prev.VehicleState.CarVersion
	} else {
		out.CarVersion = ptrStr("")
	}

	out.SoftwareUpdate = &wire.SoftwareUpdate{
		Status:       ptrStr(softwareUpdateStatus(vs.OtaStatus)),
		Version:      ptrStr(vs.OtaAvailableVersion),
		DownloadPerc: ptrInt(vs.OtaDownloadProgress),
		InstallPerc:  ptrInt(int(math.Round(vs.OtaInstallProgress))),
	}

	return out
}

// gearGuardArmed reports whether Gear Guard video is armed/recording.
// enabled/engaged -> true; disabled/empty -> false. Case-insensitive.
func gearGuardArmed(status string) bool {
	switch strings.ToLower(status) {
	case "enabled", "engaged":
		return true
	default:
		return false
	}
}

// tpmsWarning maps a tire-pressure status string to a soft-warning bool: nil
// when unknown/empty, false when "OK", true for any other status (low/fault/
// etc). Conservative: only flag when the source affirmatively reports non-OK.
func tpmsWarning(status string) *bool {
	if status == "" {
		return nil
	}
	return ptrBool(!strings.EqualFold(status, "OK"))
}

// invertClosed normalizes a closed-sensor string ("closed"/"open") into the
// open/closed string openClosedToInt expects, mapping empty to "closed".
func invertClosed(closed string) string {
	if closed == "open" {
		return "open"
	}
	return "closed"
}

// vehicleConfig builds the Tesla vehicle_config. car_type/trim_badging prefer the
// value the source decoded for THIS snapshot (vs, e.g. a real Tesla's "models2"/
// "models"), then hold-last-known from prev (a cheap summary/asleep poll carries
// no vehicle_config), then the configured placeholder, so a real car shows its
// true model instead of the model3/R1T fallback.
func vehicleConfig(prev *wire.VehicleData, vs *vehicle.State, cfg Cfg, nowMs int64) *wire.VehicleConfig {
	carType := firstNonEmpty(stateCarType(vs), prevCarType(prev), cfg.CarType, "model3")
	// trim_badging is cosmetic and nullable; NEVER fall back to a literal model.
	// The old "R1T" default mislabeled every Tesla as a Rivian when no vehicle_config
	// was cached (e.g. right after a restart). cfg.Model is the Rivian source's real
	// model and is intentionally kept for that path.
	trim := firstNonEmpty(stateTrim(vs), prevTrim(prev), cfg.Model)
	carType, trim = teslaMateCarTypeTrim(carType, trim)
	vc := &wire.VehicleConfig{
		CarType:   ptrStr(carType),
		Rhd:       ptrBool(false),
		Timestamp: ptrI64(nowMs),
	}
	if trim != "" {
		vc.TrimBadging = ptrStr(trim)
	}
	return vc
}

// teslaModelNames are the human names for the Tesla lines TeslaMate does NOT render
// (their car_type maps to a nil model upstream). The sink surfaces these as the trim
// badge under a fake "model3" car_type so e.g. a Cybertruck shows "Model 3 Cybertruck"
// instead of a blank model.
var teslaModelNames = map[string]string{
	"cybertruck": "Cybertruck",
	"roadster":   "Roadster",
	"semi":       "Semi",
	"cybercab":   "Cybercab",
}

// teslaMateCarTypeTrim remaps ONLY the Tesla lines TeslaMate cannot render
// (Cybertruck/Roadster/Semi/Cybercab) to a fake "model3" with the model name as the
// trim badge, mirroring the Rivian passthrough. EVERY other car_type passes through
// untouched, so a value TeslaMate does understand (incl. the "lychee"/"tamarind"
// refresh codenames and "models2" it maps to S/X) is never clobbered to model3.
func teslaMateCarTypeTrim(carType, trim string) (string, string) {
	if name := teslaModelNames[carType]; name != "" {
		return "model3", name
	}
	return carType, trim
}

func stateCarType(vs *vehicle.State) string {
	if vs == nil {
		return ""
	}
	return vs.CarType
}

func stateTrim(vs *vehicle.State) string {
	if vs == nil {
		return ""
	}
	return vs.TrimBadging
}

func prevCarType(prev *wire.VehicleData) string {
	if prev == nil || prev.VehicleConfig == nil {
		return ""
	}
	return derefStr(prev.VehicleConfig.CarType)
}

func prevTrim(prev *wire.VehicleData) string {
	if prev == nil || prev.VehicleConfig == nil {
		return ""
	}
	return derefStr(prev.VehicleConfig.TrimBadging)
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// asleepFallback builds a minimal but valid asleep payload when no canonical
// snapshot is available yet (first poll pending). It reuses prev where possible
// so TeslaMate sees last-known rather than nulls.
func asleepFallback(prev *wire.VehicleData, ids IDs, cfg Cfg) wire.VehicleData {
	if prev != nil {
		out := *prev
		out.ID = ids.ID
		out.VehicleID = ids.VehicleID
		out.VIN = ids.VIN
		out.DisplayName = ids.DisplayName
		out.State = "asleep"
		return out
	}
	nowMs := time.Now().UnixMilli()
	return wire.VehicleData{
		ID:          ids.ID,
		VehicleID:   ids.VehicleID,
		VIN:         ids.VIN,
		DisplayName: ids.DisplayName,
		State:       "asleep",
		APIVersion:  ptrInt(undocumentedAPIVersion),
		InService:   ptrBool(false),
		ChargeState: &wire.ChargeState{
			BatteryLevel:      ptrInt(0),
			IdealBatteryRange: ptrF64(0),
			EstBatteryRange:   ptrF64(0),
			BatteryRange:      ptrF64(0),
			ChargeEnergyAdded: ptrF64(0),
			ChargingState:     ptrStr("Disconnected"),
			Timestamp:         ptrI64(nowMs),
		},
		ClimateState: &wire.ClimateState{
			IsClimateOn: ptrBool(false),
			Timestamp:   ptrI64(nowMs),
		},
		DriveState: &wire.DriveState{
			ShiftState: nil,
			Timestamp:  ptrI64(nowMs),
			GpsAsOf:    ptrI64(nowMs / 1000),
		},
		VehicleConfig: vehicleConfig(prev, nil, cfg, nowMs),
		VehicleState: &wire.VehicleState{
			Odometer:    ptrF64(0),
			Locked:      ptrBool(true),
			VehicleName: ptrStr(ids.DisplayName),
			SoftwareUpdate: &wire.SoftwareUpdate{
				Status:  ptrStr(""),
				Version: ptrStr(""),
			},
			Timestamp: ptrI64(nowMs),
		},
	}
}

// pointer helpers (local to translate; wire's are unexported).
func ptrStr(s string) *string   { return &s }
func ptrInt(i int) *int         { return &i }
func ptrI64(i int64) *int64     { return &i }
func ptrF64(f float64) *float64 { return &f }
func ptrBool(b bool) *bool      { return &b }

const undocumentedAPIVersion = 71
