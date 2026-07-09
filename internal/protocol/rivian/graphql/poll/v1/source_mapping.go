package v1

import (
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// toCanonical maps a parsed Rivian VehicleState (+ optional live charging
// session) into the source-neutral vehicle.Snapshot. This is the rivian->
// canonical half of the hub: because Rivian is already metric, this hop is
// mostly field renames and enum normalization (the canonical->Tesla hop in the
// sink keeps the metric->imperial conversions). Sentinel-scrubbed empties are
// carried through as zero values; the sink's translator holds last-known.
func toCanonical(vs *VehicleState, live *LiveSessionData) vehicle.Snapshot {
	if vs == nil {
		return vehicle.Snapshot{Live: liveToCanonical(live)}
	}

	st := &vehicle.State{
		Power:         powerToCanonical(vs.PowerState, vs.CloudConnectionOnline),
		UserPresent:   vs.PowerState == "go",
		CloudOnline:   vs.CloudConnectionOnline,
		LastUpdate:    vs.LastUpdate,
		CloudLastSync: vs.CloudLastSync,

		SpeedMps:   vs.GnssSpeed,
		HeadingDeg: vs.GnssBearing,

		OdometerMeters: vs.VehicleMileage,
		RangeKm:        vs.DistanceToEmpty,

		BatteryLevelPct:      vs.BatteryLevel,
		BatteryLimitPct:      vs.BatteryLimit,
		Gear:                 gearToCanonical(vs.GearStatus),
		Charger:              chargerToCanonical(vs.ChargerState),
		Plug:                 plugToCanonical(vs.ChargerState, vs.ChargerStatus),
		ChargePortOpen:       vs.ChargePortState == "open",
		TimeToEndOfChargeMin: vs.TimeToEndOfCharge,

		CabinTempC:            vs.CabinInteriorTemp,
		DriverSetpointC:       vs.CabinClimateDriverTemperature,
		SeatHeatFrontLeft:     vs.SeatFrontLeftHeat,
		SeatHeatFrontRight:    vs.SeatFrontRightHeat,
		SeatHeatRearLeft:      vs.SeatRearLeftHeat,
		SeatHeatRearRight:     vs.SeatRearRightHeat,
		SteeringWheelHeat:     vs.SteeringWheelHeat,
		DefrostStatus:         vs.DefrostDefogStatus,
		PreconditioningStatus: vs.CabinPreconditioningStatus,

		GearGuardStatus: vs.GearGuardVideoStatus,

		TpmsFrontLeft:  vs.TirePressureStatusFrontLeft,
		TpmsFrontRight: vs.TirePressureStatusFrontRight,
		TpmsRearLeft:   vs.TirePressureStatusRearLeft,
		TpmsRearRight:  vs.TirePressureStatusRearRight,

		DoorFrontLeftLocked:  vs.DoorFrontLeftLocked,
		DoorFrontRightLocked: vs.DoorFrontRightLocked,
		DoorRearLeftLocked:   vs.DoorRearLeftLocked,
		DoorRearRightLocked:  vs.DoorRearRightLocked,

		DoorFrontLeftClosed:  vs.DoorFrontLeftClosed,
		DoorFrontRightClosed: vs.DoorFrontRightClosed,
		DoorRearLeftClosed:   vs.DoorRearLeftClosed,
		DoorRearRightClosed:  vs.DoorRearRightClosed,

		WindowFrontLeftClosed:  vs.WindowFrontLeftClosed,
		WindowFrontRightClosed: vs.WindowFrontRightClosed,
		WindowRearLeftClosed:   vs.WindowRearLeftClosed,
		WindowRearRightClosed:  vs.WindowRearRightClosed,

		FrunkClosed:    vs.ClosureFrunkClosed,
		LiftgateClosed: vs.ClosureLiftgateClosed,
		TonneauClosed:  vs.ClosureTonneauClosed,
		TailgateClosed: vs.ClosureTailgateClosed,

		OtaVersion:          vs.OtaCurrentVersion,
		OtaAvailableVersion: vs.OtaAvailableVersion,
		OtaStatus:           vs.OtaStatus,
		OtaInstallProgress:  vs.OtaInstallProgress,
		OtaDownloadProgress: vs.OtaDownloadProgress,
	}

	if vs.Location != nil {
		st.Location = &vehicle.Location{
			Latitude:  vs.Location.Latitude,
			Longitude: vs.Location.Longitude,
			TimeStamp: vs.Location.TimeStamp,
		}
	}

	return vehicle.Snapshot{State: st, Live: liveToCanonical(live)}
}

func liveToCanonical(live *LiveSessionData) *vehicle.LiveSession {
	if live == nil {
		return nil
	}
	return &vehicle.LiveSession{
		PowerKw:            live.Power,
		CurrentA:           live.Current,
		TotalChargedEnergy: live.TotalChargedEnergy,
		TimeRemainingSec:   live.TimeRemaining,
	}
}

// powerToCanonical collapses Rivian's powerState (plus the cloud-reachable bit)
// into the canonical liveness enum. This preserves the exact behavior the old
// translate.topState had: cloud-offline forces offline; sleep is asleep; go/
// ready are online; everything else (standby/vehicle_reset/unreachable/empty/
// unknown) is offline.
func powerToCanonical(powerState string, cloudOnline bool) vehicle.Power {
	if !cloudOnline {
		return vehicle.PowerOffline
	}
	switch powerState {
	case "sleep":
		return vehicle.PowerSleep
	case "go", "ready":
		return vehicle.PowerOnline
	default:
		return vehicle.PowerOffline
	}
}

func gearToCanonical(gear string) vehicle.Gear {
	switch gear {
	case "drive":
		return vehicle.GearDrive
	case "reverse":
		return vehicle.GearReverse
	case "neutral":
		return vehicle.GearNeutral
	case "park":
		return vehicle.GearPark
	default: // "" or unknown -> unknown (sink holds last-known)
		return vehicle.GearUnknown
	}
}

func chargerToCanonical(chargerState string) vehicle.ChargerState {
	switch chargerState {
	case "charging_active", "charging_connecting":
		return vehicle.ChargerCharging
	case "charging_ready":
		return vehicle.ChargerIdle
	case "":
		return vehicle.ChargerDisconnect
	default:
		return vehicle.ChargerIdle
	}
}

// plugToCanonical decides whether a cable is connected from the charger status/
// state. chargerStatus carries the plug state; an explicit not_connected wins.
func plugToCanonical(chargerState, chargerStatus string) vehicle.ChargePlug {
	if chargerStatus == "chrgr_sts_not_connected" {
		return vehicle.PlugDisconnected
	}
	switch chargerState {
	case "charging_active", "charging_connecting", "charging_ready":
		return vehicle.PlugConnected
	}
	if chargerStatus != "" {
		return vehicle.PlugConnected
	}
	return vehicle.PlugUnknown
}
