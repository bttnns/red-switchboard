package v1

import (
	"math"
	"strconv"
	"time"

	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

const (
	dcThresholdKw = 25.0 // at/above this a session is treated as DC fast charging
	acVoltage     = 240  // nominal single-phase AC voltage for current back-calc
)

// toRow maps a canonical snapshot into one TeslaFi CSV Row. The Date is the
// snapshot's FetchedAt rendered naive-local in loc (TeslaMate is told the same
// zone on import).
func toRow(snap vehicle.Snapshot, vehicleID int, ident vehicleIdent, loc *time.Location) Row {
	st := snap.State
	r := Row{
		Date:        snap.FetchedAt.In(loc).Format(dateLayout),
		ID:          vehicleID,
		VehicleID:   vehicleID,
		DisplayName: ident.name,
		VehicleName: ident.name,
		VIN:         ident.vin,
		State:       topState(st.Power),

		Odometer:           round2(units.MetersToMiles(float64(st.OdometerMeters))),
		BatteryLevel:       int(math.Round(st.BatteryLevelPct)),
		UsableBatteryLevel: int(math.Round(st.BatteryLevelPct)),
		ChargeLimitSoc:     int(math.Round(st.BatteryLimitPct)),

		InsideTemp:        st.CabinTempC,
		OutsideTemp:       outsideTempOrDefault(st.OutsideTempC),
		IsClimateOn:       st.CabinTempC != 0 && st.PreconditioningStatus == "active",
		DriverTempSetting: st.DriverSetpointC,

		CarVersion: st.OtaVersion,
	}

	// Range fields (miles), all derived from the same SOC-backed range so the
	// rated/ideal/est readings never disagree.
	rangeMi := round2(units.KmToMiles(float64(st.RangeKm)))
	r.BatteryRange = rangeMi
	r.EstBatteryRange = rangeMi
	r.IdealBatteryRange = rangeMi

	// Location.
	if st.Location != nil {
		r.Latitude = st.Location.Latitude
		r.Longitude = st.Location.Longitude
	}
	r.Heading = int(math.Round(st.HeadingDeg))

	// Drive: shift_state + speed only while in gear; TeslaMate detects drives
	// from shift_state in {D,R,N}.
	if st.Gear == vehicle.GearDrive || st.Gear == vehicle.GearReverse || st.Gear == vehicle.GearNeutral {
		r.ShiftState = shiftState(st.Gear)
		r.Speed = strconv.Itoa(int(math.Round(units.MpsToMph(st.SpeedMps))))
	}

	// Charge.
	r.ChargingState = chargingState(st.Charger, st.Plug)
	if snap.Live != nil {
		live := snap.Live
		r.ChargeEnergyAdded = round2(live.TotalChargedEnergy)
		r.ChargerPower = int(math.Round(live.PowerKw))
		r.TimeToFullCharge = round2(float64(live.TimeRemainingSec) / 3600.0)
		if live.PowerKw >= dcThresholdKw {
			// DC fast charge: like the real Tesla API, report it via charger_power
			// and leave voltage/current at 0. (TeslaMate multiplies voltage*current
			// as smallints, which overflows at real DC magnitudes, e.g. 400V*500A.)
			r.FastChargerPresent = true
			r.FastChargerType = "Tesla"
			r.ChargerPhases = ""
		} else {
			r.FastChargerType = "<invalid>"
			r.ChargerVoltage = acVoltage
			r.ChargerActualCurrent = ampsFrom(live.PowerKw, acVoltage)
			r.ChargerPhases = "1"
		}
	}
	return r
}

func topState(p vehicle.Power) string {
	switch p {
	case vehicle.PowerSleep:
		return "asleep"
	case vehicle.PowerOffline:
		return "offline"
	default:
		return "online"
	}
}

func shiftState(g vehicle.Gear) string {
	switch g {
	case vehicle.GearDrive:
		return "D"
	case vehicle.GearReverse:
		return "R"
	case vehicle.GearNeutral:
		return "N"
	default:
		return "P"
	}
}

// chargingState maps the canonical charger/plug enums to TeslaFi's string: a
// drawing charger is Charging; plugged-but-idle reads Complete; otherwise
// Disconnected.
func chargingState(c vehicle.ChargerState, plug vehicle.ChargePlug) string {
	if plug == vehicle.PlugDisconnected {
		return "Disconnected"
	}
	switch c {
	case vehicle.ChargerCharging:
		return "Charging"
	case vehicle.ChargerIdle:
		return "Complete"
	default:
		return "Disconnected"
	}
}

func ampsFrom(powerKw float64, volts int) int {
	if volts <= 0 {
		return 0
	}
	return int(math.Round(powerKw * 1000.0 / float64(volts)))
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// The TeslaFi CSV field is a plain float; when the source reports no ambient temp
// (canonical 0), a mild constant reads more realistically than a literal 0 °C.
func outsideTempOrDefault(c float64) float64 {
	if c == 0 {
		return 20.0
	}
	return c
}
