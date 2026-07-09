package v1

import (
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// This file is the ENCODE half of the Rivian GraphQL sink: it turns the source-
// neutral vehicle.State / vehicle.LiveSession back into the exact Rivian GraphQL
// response shapes the real cloud returns, so a Rivian client (and the package's
// own parse.go) round-trips the output. It is the inverse of source_mapping.go's
// toCanonical: where toCanonical normalizes Rivian enums into canonical ones,
// this re-derives the Rivian wire strings; where toCanonical flattens the
// `{ timeStamp value }` / `{ value updatedAt }` wrappers, this re-wraps them.
//
// Rivian is already metric (the same SI units the canonical model uses), so NO
// unit conversion happens here (the canonical->Tesla sink keeps the imperial
// conversions; this one does not need internal/units).
//
//  canonical vehicle.State    Rivian GraphQL field        Tesla Fleet equivalent (ref)
//  -----------------------    --------------------        ---------------------------
//  Power / CloudOnline        cloudConnection.isOnline,   state ("online"/"asleep")
//                             powerState
//  UserPresent                powerState ("go")           drive_state (is_user_present)
//  CloudLastSync              cloudConnection.lastSync    vehicle_state.timestamp
//  LastUpdate                 (every scalar's timeStamp)  vehicle_state.timestamp
//  Location.{Lat,Lon}         gnssLocation.{latitude,..}  drive_state.{latitude,..}
//  SpeedMps                   gnssSpeed                   drive_state.speed (mph)
//  HeadingDeg                 gnssBearing                 drive_state.heading
//  OdometerMeters             vehicleMileage (meters)     vehicle_state.odometer (mi)
//  RangeKm                    distanceToEmpty (km)        charge_state.battery_range
//  BatteryLevelPct            batteryLevel                charge_state.battery_level
//  BatteryLimitPct            batteryLimit                charge_state.charge_limit_soc
//  Gear                       gearStatus                  drive_state.shift_state
//  Charger                    chargerState                charge_state.charging_state
//  Plug                       chargerStatus               charge_state.conn_charge_cable
//  ChargePortOpen             chargePortState             charge_state.charge_port_door_open
//  TimeToEndOfChargeMin       timeToEndOfCharge (min)     charge_state.minutes_to_full_charge
//  CabinTempC                 cabinClimateInteriorTemp..  climate_state.inside_temp
//  DriverSetpointC            cabinClimateDriverTemp..    climate_state.driver_temp_setting
//  SeatHeat*/SteeringWheel..  seat*Heat/steeringWheelHeat climate_state.seat_heater_*
//  DefrostStatus              defrostDefogStatus          climate_state.defrost_mode
//  PreconditioningStatus      cabinPreconditioningStatus  climate_state.is_preconditioning
//  GearGuardStatus            gearGuardVideoStatus        vehicle_state.sentry_mode
//  Tpms*                      tirePressureStatus*         vehicle_state.tpms_*
//  Door*Locked                door*Locked                 vehicle_state.locked
//  Door*/Window*/*Closed      door*/window*/closure*..    vehicle_state.{df,pf,dr,..}
//  Frunk/Liftgate/..Closed    closureFrunk/Liftgate..     vehicle_state.{ft,rt}
//  OtaVersion                 otaCurrentVersion           vehicle_state.car_version
//  OtaAvailableVersion        otaAvailableVersion         (software_update)
//  OtaStatus                  otaStatus                   software_update.status
//  OtaInstallProgress         otaInstallProgress          software_update.install_perc
//  OtaDownloadProgress        otaDownloadProgress         software_update.download_perc
//
//  canonical vehicle.LiveSession   Rivian getLiveSessionData field
//  --------------------------      -------------------------------
//  PowerKw                         power
//  CurrentA                        current
//  TotalChargedEnergy              totalChargedEnergy
//  TimeRemainingSec                timeRemaining

// tstr formats a timestamp as RFC3339Nano, or "" for the zero time (parse.go's
// parseTime treats "" as the zero time).
func tstr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// tsv builds a `{ timeStamp value }` gateway scalar wrapper.
func tsv(t time.Time, value any) map[string]any {
	return map[string]any{"timeStamp": tstr(t), "value": value}
}

// vrv builds a `{ value updatedAt }` charging value record.
func vrv(t time.Time, value any) map[string]any {
	return map[string]any{"value": value, "updatedAt": tstr(t)}
}

// vehicleStateBody builds the gateway vehicleState response body for a canonical
// state: {"data":{"vehicleState":{...}}}. Every scalar is wrapped as
// `{ timeStamp value }` (stamped with the state's LastUpdate); cloudConnection
// and gnssLocation use their nested object shapes. Empty enum strings are omitted
// (a sink that holds last-known would not have emitted them), matching the
// mock's marshal so parse.go's INVALID_SENSOR_STATES handling never trips.
func vehicleStateBody(st *vehicle.State) map[string]any {
	if st == nil {
		return map[string]any{"data": map[string]any{"vehicleState": nil}}
	}
	now := st.LastUpdate
	m := map[string]any{}

	m["cloudConnection"] = map[string]any{
		"isOnline": cloudOnlineWire(st),
		"lastSync": tstr(st.CloudLastSync),
	}
	if st.Location != nil {
		m["gnssLocation"] = map[string]any{
			"latitude":  st.Location.Latitude,
			"longitude": st.Location.Longitude,
			"timeStamp": tstr(st.Location.TimeStamp),
		}
	}

	num := func(key string, value any) { m[key] = tsv(now, value) }
	str := func(key, s string) {
		if s != "" {
			m[key] = tsv(now, s)
		}
	}

	num("gnssSpeed", st.SpeedMps)
	num("gnssBearing", st.HeadingDeg)
	num("vehicleMileage", st.OdometerMeters)
	num("distanceToEmpty", st.RangeKm)
	num("batteryLevel", st.BatteryLevelPct)
	num("batteryLimit", st.BatteryLimitPct)
	str("powerState", powerToWire(st))
	str("gearStatus", gearToWire(st.Gear))
	str("chargerState", chargerToWire(st.Charger))
	str("chargerStatus", plugToWire(st.Plug))
	str("chargePortState", chargePortToWire(st.ChargePortOpen))
	num("timeToEndOfCharge", st.TimeToEndOfChargeMin)

	num("cabinClimateInteriorTemperature", st.CabinTempC)
	num("cabinClimateDriverTemperature", st.DriverSetpointC)
	str("cabinPreconditioningStatus", st.PreconditioningStatus)
	str("defrostDefogStatus", st.DefrostStatus)
	str("seatFrontLeftHeat", st.SeatHeatFrontLeft)
	str("seatFrontRightHeat", st.SeatHeatFrontRight)
	str("seatRearLeftHeat", st.SeatHeatRearLeft)
	str("seatRearRightHeat", st.SeatHeatRearRight)
	str("steeringWheelHeat", st.SteeringWheelHeat)
	str("gearGuardVideoStatus", st.GearGuardStatus)

	str("tirePressureStatusFrontLeft", st.TpmsFrontLeft)
	str("tirePressureStatusFrontRight", st.TpmsFrontRight)
	str("tirePressureStatusRearLeft", st.TpmsRearLeft)
	str("tirePressureStatusRearRight", st.TpmsRearRight)

	str("doorFrontLeftLocked", st.DoorFrontLeftLocked)
	str("doorFrontLeftClosed", st.DoorFrontLeftClosed)
	str("doorFrontRightLocked", st.DoorFrontRightLocked)
	str("doorFrontRightClosed", st.DoorFrontRightClosed)
	str("doorRearLeftLocked", st.DoorRearLeftLocked)
	str("doorRearLeftClosed", st.DoorRearLeftClosed)
	str("doorRearRightLocked", st.DoorRearRightLocked)
	str("doorRearRightClosed", st.DoorRearRightClosed)

	str("windowFrontLeftClosed", st.WindowFrontLeftClosed)
	str("windowFrontRightClosed", st.WindowFrontRightClosed)
	str("windowRearLeftClosed", st.WindowRearLeftClosed)
	str("windowRearRightClosed", st.WindowRearRightClosed)

	str("closureFrunkClosed", st.FrunkClosed)
	str("closureLiftgateClosed", st.LiftgateClosed)
	str("closureTonneauClosed", st.TonneauClosed)
	str("closureTailgateClosed", st.TailgateClosed)

	str("otaCurrentVersion", st.OtaVersion)
	str("otaAvailableVersion", st.OtaAvailableVersion)
	str("otaStatus", st.OtaStatus)
	num("otaInstallProgress", st.OtaInstallProgress)
	num("otaDownloadProgress", st.OtaDownloadProgress)

	return map[string]any{"data": map[string]any{"vehicleState": m}}
}

// liveSessionBody builds the charging getLiveSessionData response body for a
// canonical live session. A nil session marshals to
// {"data":{"getLiveSessionData":null}} (the no-active-session shape parse.go
// reads as "no session"). Records are stamped with the state's LastUpdate so the
// loop is deterministic.
func liveSessionBody(ls *vehicle.LiveSession, now time.Time) map[string]any {
	if ls == nil {
		return map[string]any{"data": map[string]any{"getLiveSessionData": nil}}
	}
	s := map[string]any{
		"__typename":         "LiveSessionData",
		"power":              vrv(now, ls.PowerKw),
		"current":            vrv(now, ls.CurrentA),
		"timeRemaining":      vrv(now, ls.TimeRemainingSec),
		"totalChargedEnergy": vrv(now, ls.TotalChargedEnergy),
	}
	return map[string]any{"data": map[string]any{"getLiveSessionData": s}}
}

// cloudOnlineWire mirrors source_mapping.powerToCanonical's use of the cloud bit:
// offline power means the cloud reported the car unreachable.
func cloudOnlineWire(st *vehicle.State) bool {
	if st.CloudOnline {
		return true
	}
	return st.Power != vehicle.PowerOffline
}

// powerToWire is the inverse of powerToCanonical: it re-derives a Rivian
// powerState string from the canonical liveness (plus the user-present bit so
// "go" survives the loop). Sleep -> "sleep"; online -> "go" when a user is
// present else "ready"; offline/unknown -> "" (held last-known upstream).
func powerToWire(st *vehicle.State) string {
	switch st.Power {
	case vehicle.PowerSleep:
		return "sleep"
	case vehicle.PowerOnline:
		if st.UserPresent {
			return "go"
		}
		return "ready"
	default:
		return ""
	}
}

// gearToWire is the inverse of gearToCanonical.
func gearToWire(g vehicle.Gear) string {
	switch g {
	case vehicle.GearDrive:
		return "drive"
	case vehicle.GearReverse:
		return "reverse"
	case vehicle.GearNeutral:
		return "neutral"
	case vehicle.GearPark:
		return "park"
	default:
		return ""
	}
}

// chargerToWire is the inverse of chargerToCanonical. Canonical collapses several
// Rivian states into Charging/Idle, so this picks the representative wire string
// that re-canonicalizes to the same enum: charging_active for Charging,
// charging_ready for Idle, "" for Disconnect/Unknown.
func chargerToWire(c vehicle.ChargerState) string {
	switch c {
	case vehicle.ChargerCharging:
		return "charging_active"
	case vehicle.ChargerIdle:
		return "charging_ready"
	default:
		return ""
	}
}

// plugToWire re-derives a chargerStatus that re-canonicalizes to the same plug
// enum via plugToCanonical: not_connected for Disconnected, a connected sentinel
// for Connected, "" for Unknown.
func plugToWire(p vehicle.ChargePlug) string {
	switch p {
	case vehicle.PlugDisconnected:
		return "chrgr_sts_not_connected"
	case vehicle.PlugConnected:
		return "chrgr_sts_connected_charging"
	default:
		return ""
	}
}

// chargePortToWire is the inverse of the ChargePortOpen derivation (open ==
// chargePortState "open").
func chargePortToWire(open bool) string {
	if open {
		return "open"
	}
	return "closed"
}
