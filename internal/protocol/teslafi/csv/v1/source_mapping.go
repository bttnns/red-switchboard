package v1

import (
	"strconv"
	"time"

	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// toSnapshot maps a TeslaFi CSV Row back into a canonical snapshot, the inverse
// of toRow. The recorded Date (parsed in loc) is preserved as the snapshot's
// timestamp; miles/mph are converted back to SI.
func (r Row) toSnapshot(loc *time.Location) vehicle.Snapshot {
	t, _ := r.parsedDate(loc)
	st := &vehicle.State{
		Power:         powerFromState(r.State),
		CloudOnline:   r.State != "offline",
		LastUpdate:    t,
		CloudLastSync: t,

		HeadingDeg:     float64(r.Heading),
		OdometerMeters: int(units.MilesToMeters(r.Odometer)),
		RangeKm:        int(units.MilesToKm(r.BatteryRange)),

		BatteryLevelPct: float64(r.BatteryLevel),
		BatteryLimitPct: float64(r.ChargeLimitSoc),
		Gear:            gearFromShift(r.ShiftState),
		Charger:         chargerFromState(r.ChargingState),
		Plug:            plugFromState(r.ChargingState),

		CabinTempC:      r.InsideTemp,
		OutsideTempC:    r.OutsideTemp,
		DriverSetpointC: r.DriverTempSetting,
		OtaVersion:      r.CarVersion,
	}
	if speed, err := strconv.ParseFloat(r.Speed, 64); err == nil {
		st.SpeedMps = units.MphToMps(speed)
	}
	if r.Latitude != 0 || r.Longitude != 0 {
		st.Location = &vehicle.Location{Latitude: r.Latitude, Longitude: r.Longitude, TimeStamp: t}
	}

	var live *vehicle.LiveSession
	if r.ChargingState == "Charging" {
		live = &vehicle.LiveSession{
			PowerKw:            float64(r.ChargerPower),
			CurrentA:           float64(r.ChargerActualCurrent),
			TotalChargedEnergy: r.ChargeEnergyAdded,
			TimeRemainingSec:   int(r.TimeToFullCharge * 3600),
		}
	}
	return vehicle.Snapshot{State: st, Live: live, FetchedAt: t}
}

func powerFromState(s string) vehicle.Power {
	switch s {
	case "asleep":
		return vehicle.PowerSleep
	case "offline":
		return vehicle.PowerOffline
	default:
		return vehicle.PowerOnline
	}
}

func gearFromShift(s string) vehicle.Gear {
	switch s {
	case "D":
		return vehicle.GearDrive
	case "R":
		return vehicle.GearReverse
	case "N":
		return vehicle.GearNeutral
	default:
		return vehicle.GearPark
	}
}

func chargerFromState(s string) vehicle.ChargerState {
	switch s {
	case "Charging":
		return vehicle.ChargerCharging
	case "Complete", "Stopped":
		return vehicle.ChargerIdle
	default:
		return vehicle.ChargerDisconnect
	}
}

func plugFromState(s string) vehicle.ChargePlug {
	if s == "Disconnected" || s == "" {
		return vehicle.PlugDisconnected
	}
	return vehicle.PlugConnected
}
