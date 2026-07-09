package v1

// source_mapping.go is the Tesla Fleet -> canonical half of the hub: the inverse
// of sink_mapping.go's VehicleData encode. A vehicle_data payload pulled from the
// Tesla Fleet API (imperial units, Tesla wire strings/ints) is decoded into the
// source-neutral vehicle.Snapshot (SI units, typed enums) that everything
// downstream consumes. Where the sink encode converts canonical->imperial and
// canonical-enum->Tesla-string, this decode runs each conversion the other way.
// Pointer wire fields that arrived nil are left at the canonical zero value.
//
// Field correspondence (Tesla vehicle_data -> canonical vehicle.State):
//
//  Tesla Fleet vehicle_data field          canonical vehicle.State    notes
//  ------------------------------          -----------------------    -----
//  state ("online"/"asleep"/...)           Power                      string -> Power enum
//  vehicle_state.is_user_present           UserPresent                pass through
//  drive_state.timestamp (ms)              LastUpdate                 ms -> time.Time
//  drive_state.latitude/longitude          Location.Lat/Lon           pass through
//  drive_state.gps_as_of (s)               Location.TimeStamp         s -> time.Time
//  drive_state.heading (deg)               HeadingDeg                 pass through
//  drive_state.speed (mph)                 SpeedMps                   mph -> m/s
//  drive_state.shift_state ("D"/"R"/...)   Gear                       string -> Gear enum
//  drive_state.power (kW)                  DrivePowerKw               pass through (sign preserved)
//  vehicle_state.odometer (mi)             OdometerMeters             mi -> m
//  charge_state.battery_range (mi)         RangeKm                    mi -> km
//  charge_state.battery_level (%)          BatteryLevelPct            pass through
//  charge_state.charge_limit_soc (%)       BatteryLimitPct            pass through
//  charge_state.charging_state             Charger / Plug             string -> Charger/Plug enums
//  charge_state.charge_port_door_open      ChargePortOpen             pass through
//  charge_state.time_to_full_charge (h)    TimeToEndOfChargeMin       hours -> minutes
//  climate_state.inside_temp (C)           CabinTempC                 already Celsius
//  climate_state.outside_temp (C)          OutsideTempC               already Celsius
//  climate_state.driver_temp_setting (C)   DriverSetpointC            already Celsius
//  climate_state.seat_heater_* (0..3)      SeatHeat* ("Off"/"Level_n") int -> level string
//  climate_state.steering_wheel_heater     SteeringWheelHeat          bool -> "Off"/"Level_1"
//  climate_state.is_front_defroster_on     DefrostStatus              bool -> "Off"/"On"
//  climate_state.is_preconditioning        PreconditioningStatus      bool -> "off"/"active"
//  vehicle_state.sentry_mode               GearGuardStatus            bool -> "disabled"/"enabled"
//  vehicle_state.tpms_soft_warning_*       Tpms*                      bool -> "OK"/"WARN"
//  vehicle_state.locked                    Door*Locked                bool -> "locked"/"unlocked"
//  vehicle_state.df/dr/pf/pr (1/0)         Door*Closed                1/0 -> "open"/"closed"
//  vehicle_state.fd_window/... (1/0)       Window*Closed              1/0 -> "open"/"closed"
//  vehicle_state.ft / rt (1/0)             FrunkClosed / LiftgateClosed 1/0 -> "open"/"closed"
//  vehicle_state.car_version               OtaVersion                 pass through
//  vehicle_state.software_update.status    OtaStatus                  pass through
//  vehicle_state.software_update.version   OtaAvailableVersion        pass through
//  vehicle_state.software_update.*_perc    OtaDownload/InstallProgress pass through
//  charge_state.charger_power (kW)         LiveSession.PowerKw        when charging
//  charge_state.charger_actual_current (A) LiveSession.CurrentA       when charging
//  charge_state.charge_energy_added (kWh)  LiveSession.TotalCharged   when charging
//  charge_state.time_to_full_charge (h)    LiveSession.TimeRemaining  hours -> seconds

import (
	"math"
	"time"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// DecodeVehicleData maps a Tesla Fleet vehicle_data payload into the canonical
// vehicle.Snapshot. It is the inverse of sink_mapping.go's VehicleData. A nil vd
// (or one with no sub-objects) yields an empty snapshot; absent pointer fields
// stay at the canonical zero value (the sink holds last-known).
func DecodeVehicleData(vd *wire.VehicleData) vehicle.Snapshot {
	if vd == nil {
		return vehicle.Snapshot{}
	}

	st := &vehicle.State{
		Power: powerFromTesla(vd.State),
	}

	if ds := vd.DriveState; ds != nil {
		st.LastUpdate = msToTime(ds.Timestamp)
		st.HeadingDeg = float64(derefInt(ds.Heading))
		st.SpeedMps = units.MphToMps(derefF64(ds.Speed))
		st.Gear = GearFromTesla(derefStr(ds.ShiftState))
		// drive_state.power is already kW; preserve sign (negative = regen). nil
		// when not driving, so the canonical slot stays absent.
		if ds.Power != nil {
			p := *ds.Power
			st.DrivePowerKw = &p
		}
		if ds.Latitude != nil && ds.Longitude != nil {
			st.Location = &vehicle.Location{
				Latitude:  *ds.Latitude,
				Longitude: *ds.Longitude,
				TimeStamp: secToTime(ds.GpsAsOf),
			}
		}
	}

	if cs := vd.ChargeState; cs != nil {
		// Odometer comes from vehicle_state below; range is here in charge_state.
		st.RangeKm = int(math.Round(units.MilesToKm(derefF64(cs.BatteryRange))))
		st.BatteryLevelPct = float64(derefInt(cs.BatteryLevel))
		st.BatteryLimitPct = float64(derefInt(cs.ChargeLimitSoc))
		st.Charger = ChargerFromTesla(derefStr(cs.ChargingState))
		st.Plug = PlugFromTesla(derefStr(cs.ChargingState))
		st.ChargePortOpen = derefBool(cs.ChargePortDoorOpen)
		// time_to_full_charge is in hours; canonical wants minutes.
		st.TimeToEndOfChargeMin = int(math.Round(derefF64(cs.TimeToFullCharge) * 60.0))
		st.ChargerVoltageV = derefInt(cs.ChargerVoltage)
		st.BatteryHeaterOn = derefBool(cs.BatteryHeaterOn)
	}

	if cl := vd.ClimateState; cl != nil {
		st.CabinTempC = derefF64(cl.InsideTemp)
		st.OutsideTempC = derefF64(cl.OutsideTemp)
		st.DriverSetpointC = derefF64(cl.DriverTempSetting)
		st.SeatHeatFrontLeft = seatLevelFromTesla(cl.SeatHeaterLeft)
		st.SeatHeatFrontRight = seatLevelFromTesla(cl.SeatHeaterRight)
		st.SeatHeatRearLeft = seatLevelFromTesla(cl.SeatHeaterRearLeft)
		st.SeatHeatRearRight = seatLevelFromTesla(cl.SeatHeaterRearRight)
		st.SteeringWheelHeat = onOffLevel(derefBool(cl.SteeringWheelHeater))
		st.DefrostStatus = onOff(derefBool(cl.IsFrontDefrosterOn))
		st.PreconditioningStatus = preconditioningFromTesla(derefBool(cl.IsPreconditioning))
	}

	if vss := vd.VehicleState; vss != nil {
		st.UserPresent = derefBool(vss.IsUserPresent)
		st.OdometerMeters = int(math.Round(units.MilesToMeters(derefF64(vss.Odometer))))
		st.GearGuardStatus = gearGuardFromTesla(derefBool(vss.SentryMode))

		st.TpmsFrontLeft = tpmsFromTesla(vss.TpmsSoftWarningFl)
		st.TpmsFrontRight = tpmsFromTesla(vss.TpmsSoftWarningFr)
		st.TpmsRearLeft = tpmsFromTesla(vss.TpmsSoftWarningRl)
		st.TpmsRearRight = tpmsFromTesla(vss.TpmsSoftWarningRr)

		st.TpmsPressureFlBar = derefF64(vss.TpmsPressureFl)
		st.TpmsPressureFrBar = derefF64(vss.TpmsPressureFr)
		st.TpmsPressureRlBar = derefF64(vss.TpmsPressureRl)
		st.TpmsPressureRrBar = derefF64(vss.TpmsPressureRr)

		lock := lockedFromTesla(vss.Locked)
		st.DoorFrontLeftLocked = lock
		st.DoorFrontRightLocked = lock
		st.DoorRearLeftLocked = lock
		st.DoorRearRightLocked = lock

		st.DoorFrontLeftClosed = closedFromTesla(vss.Df)
		st.DoorFrontRightClosed = closedFromTesla(vss.Pf)
		st.DoorRearLeftClosed = closedFromTesla(vss.Dr)
		st.DoorRearRightClosed = closedFromTesla(vss.Pr)

		st.WindowFrontLeftClosed = closedFromTesla(vss.FdWindow)
		st.WindowFrontRightClosed = closedFromTesla(vss.FpWindow)
		st.WindowRearLeftClosed = closedFromTesla(vss.RdWindow)
		st.WindowRearRightClosed = closedFromTesla(vss.RpWindow)

		st.FrunkClosed = closedFromTesla(vss.Ft)
		st.LiftgateClosed = closedFromTesla(vss.Rt)

		st.OtaVersion = derefStr(vss.CarVersion)
		if su := vss.SoftwareUpdate; su != nil {
			st.OtaStatus = derefStr(su.Status)
			st.OtaAvailableVersion = derefStr(su.Version)
			st.OtaDownloadProgress = derefInt(su.DownloadPerc)
			st.OtaInstallProgress = float64(derefInt(su.InstallPerc))
		}
	}

	if vc := vd.VehicleConfig; vc != nil {
		st.CarType = derefStr(vc.CarType)
		st.TrimBadging = derefStr(vc.TrimBadging)
	}

	return vehicle.Snapshot{State: st, Live: liveFromTesla(vd.ChargeState)}
}

// liveFromTesla reconstructs the canonical live charging session from the
// charge_state when a session is actively delivering power. Absent power (parked
// or idle) yields nil, matching the sink's "live only while charging" contract.
func liveFromTesla(cs *wire.ChargeState) *vehicle.LiveSession {
	if cs == nil {
		return nil
	}
	if derefStr(cs.ChargingState) != "Charging" {
		return nil
	}
	return &vehicle.LiveSession{
		PowerKw:            derefF64(cs.ChargerPower),
		CurrentA:           float64(derefInt(cs.ChargerActualCurrent)),
		TotalChargedEnergy: derefF64(cs.ChargeEnergyAdded),
		FastCharger:        derefBool(cs.FastChargerPresent),
		// time_to_full_charge is in hours; canonical live wants seconds.
		TimeRemainingSec: int(math.Round(derefF64(cs.TimeToFullCharge) * 3600.0)),
	}
}

// SummaryToSnapshot maps the cheap per-vehicle summary (the no-wake state check)
// into a minimal canonical snapshot carrying only liveness. It is used when a car
// is asleep/offline, so the sink reports the correct power state while holding
// last-known values for everything else (no vehicle_data is fetched, so the car
// is not woken).
func SummaryToSnapshot(s wire.Summary) vehicle.Snapshot {
	now := time.Now()
	power := powerFromTesla(s.State)
	return vehicle.Snapshot{
		State: &vehicle.State{
			Power:       power,
			CloudOnline: power != vehicle.PowerOffline,
			LastUpdate:  now,
		},
		FetchedAt: now,
	}
}

// powerFromTesla collapses the Tesla top-level state into the canonical liveness
// enum: the inverse of sink_mapping.go's topState.
func powerFromTesla(state string) vehicle.Power {
	switch state {
	case "online":
		return vehicle.PowerOnline
	case "asleep":
		return vehicle.PowerSleep
	case "offline":
		return vehicle.PowerOffline
	default:
		return vehicle.PowerUnknown
	}
}

// GearFromTesla maps the Tesla shift_state to the canonical Gear: the inverse of
// sink_mapping.go's shiftState. Empty/null shift_state (parked) -> Park.
// Exported so the Fleet Telemetry stream source reuses the single-letter map
// after normalizing the protobuf ShiftState enum to a letter.
func GearFromTesla(shift string) vehicle.Gear {
	switch shift {
	case "D":
		return vehicle.GearDrive
	case "R":
		return vehicle.GearReverse
	case "N":
		return vehicle.GearNeutral
	case "P":
		return vehicle.GearPark
	default: // "" / null: parked or not reported.
		return vehicle.GearUnknown
	}
}

// ChargerFromTesla maps the Tesla charging_state to the canonical charger enum:
// the inverse of sink_mapping.go's chargingState. "Complete" is a settled,
// plugged-but-idle state. Exported so the stream decoder maps DetailedChargeState
// through the same table.
func ChargerFromTesla(charging string) vehicle.ChargerState {
	switch charging {
	case "Charging":
		return vehicle.ChargerCharging
	case "Stopped", "Complete", "NoPower", "Starting":
		return vehicle.ChargerIdle
	case "Disconnected":
		return vehicle.ChargerDisconnect
	default:
		return vehicle.ChargerUnknown
	}
}

// PlugFromTesla decides cable connectivity from the charging_state: anything but
// Disconnected (and a known state) means a cable is connected. Exported so the
// stream decoder shares the table.
func PlugFromTesla(charging string) vehicle.ChargePlug {
	switch charging {
	case "Charging", "Stopped", "Complete", "NoPower", "Starting":
		return vehicle.PlugConnected
	case "Disconnected":
		return vehicle.PlugDisconnected
	default:
		return vehicle.PlugUnknown
	}
}

// seatLevelFromTesla maps the Tesla 0..3 seat heater level to the canonical
// "Off"/"Level_n" string: the inverse of sink_mapping.go's seatHeatLevel. A nil
// reading (heater absent) -> "" (unknown).
func seatLevelFromTesla(level *int) string {
	if level == nil {
		return ""
	}
	switch *level {
	case 0:
		return "Off"
	case 1:
		return "Level_1"
	case 2:
		return "Level_2"
	case 3:
		return "Level_3"
	default:
		return ""
	}
}

// onOffLevel maps a Tesla on/off bool back to the canonical "Off"/"Level_1"
// control string (the source level granularity is lost on the encode, so a true
// round-trips to the lowest on-level).
func onOffLevel(on bool) string {
	if on {
		return "Level_1"
	}
	return "Off"
}

// onOff maps a Tesla on/off bool back to a canonical "On"/"Off" status string.
func onOff(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

// preconditioningFromTesla maps the Tesla is_preconditioning bool to the
// canonical preconditioning status: the inverse of sink_mapping.go's
// preconditioningActive.
func preconditioningFromTesla(on bool) string {
	if on {
		return "active"
	}
	return "off"
}

// gearGuardFromTesla maps the Tesla sentry_mode bool to the canonical Gear Guard
// status: the inverse of sink_mapping.go's gearGuardArmed.
func gearGuardFromTesla(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// tpmsFromTesla maps a Tesla soft-warning bool back to a canonical tire status
// string: the inverse of sink_mapping.go's tpmsWarning. nil -> "" (unknown).
func tpmsFromTesla(warn *bool) string {
	if warn == nil {
		return ""
	}
	if *warn {
		return "WARN"
	}
	return "OK"
}

// lockedFromTesla maps the Tesla locked bool back to the canonical per-door lock
// string. nil -> "" (unknown). A false (any door unlocked) round-trips every door
// to "unlocked" since the Tesla wire carries a single fleet-wide locked bit.
func lockedFromTesla(locked *bool) string {
	if locked == nil {
		return ""
	}
	if *locked {
		return "locked"
	}
	return "unlocked"
}

// closedFromTesla maps the Tesla 1/0 open/closed int back to the canonical
// "open"/"closed" string: the inverse of sink_mapping.go's openClosedToInt +
// invertClosed. nil -> "" (unknown).
func closedFromTesla(v *int) string {
	if v == nil {
		return ""
	}
	if *v == 1 {
		return "open"
	}
	return "closed"
}

// msToTime converts a millisecond epoch pointer to a UTC time (zero when nil/0).
func msToTime(ms *int64) time.Time {
	if ms == nil || *ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(*ms).UTC()
}

// secToTime converts a second epoch pointer to a UTC time (zero when nil/0).
func secToTime(s *int64) time.Time {
	if s == nil || *s == 0 {
		return time.Time{}
	}
	return time.Unix(*s, 0).UTC()
}

// deref helpers: a nil pointer yields the type's zero value.
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefF64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}
