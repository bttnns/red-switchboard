package wire

import "time"

// pointer helpers for building fixtures and (later) translations.
func ptrStr(s string) *string   { return &s }
func ptrInt(i int) *int         { return &i }
func ptrI64(i int64) *int64     { return &i }
func ptrF64(f float64) *float64 { return &f }
func ptrBool(b bool) *bool      { return &b }

// NewOnlineVehicleData returns a fully-populated, valid ONLINE vehicle_data
// fixture: all five sub-objects non-null, vehicle_config non-null, charge_state
// with non-null charge_energy_added and ideal_battery_range, drive_state with
// numeric timestamp/lat/lon, vehicle_state with numeric odometer. This is the
// Phase 1 static payload TeslaMate consumes to bring a car online.
func NewOnlineVehicleData(id, vehicleID int64, vin, displayName string) VehicleData {
	nowMs := time.Now().UnixMilli()

	return VehicleData{
		ID:          id,
		VehicleID:   vehicleID,
		VIN:         vin,
		DisplayName: displayName,
		State:       "online",
		OptionCodes: nil,
		APIVersion:  ptrInt(undocumentedAPIVersion),
		InService:   ptrBool(false),

		ChargeState: &ChargeState{
			BatteryLevel:         ptrInt(72),
			UsableBatteryLevel:   ptrInt(72),
			BatteryRange:         ptrF64(230.0),
			IdealBatteryRange:    ptrF64(230.0), // must be non-null
			EstBatteryRange:      ptrF64(215.0),
			ChargeLimitSoc:       ptrInt(80),
			ChargeEnergyAdded:    ptrF64(0.0), // must be non-null
			ChargingState:        ptrStr("Disconnected"),
			ChargePortDoorOpen:   ptrBool(false),
			ChargerPower:         ptrF64(0),
			ChargerActualCurrent: ptrInt(0),
			ChargerVoltage:       ptrInt(0),
			TimeToFullCharge:     ptrF64(0),
			ChargeRate:           ptrF64(0),
			FastChargerPresent:   ptrBool(false),
			Timestamp:            ptrI64(nowMs),
		},

		ClimateState: &ClimateState{
			InsideTemp:  ptrF64(21.0),
			OutsideTemp: nil,
			IsClimateOn: ptrBool(false),
			Timestamp:   ptrI64(nowMs),
		},

		DriveState: &DriveState{
			Latitude:   ptrF64(37.7749),
			Longitude:  ptrF64(-122.4194),
			Heading:    ptrInt(0),
			Speed:      nil,
			Power:      nil,
			ShiftState: nil, // parked => null/P
			Timestamp:  ptrI64(nowMs),
			GpsAsOf:    ptrI64(nowMs / 1000),
		},

		VehicleConfig: &VehicleConfig{
			CarType:     ptrStr("model3"), // cosmetic; non-null required by FSM
			TrimBadging: ptrStr("R1T"),
			Rhd:         ptrBool(false),
			Timestamp:   ptrI64(nowMs),
		},

		VehicleState: &VehicleState{
			Odometer:      ptrF64(12345.6), // numeric required
			Locked:        ptrBool(true),
			CarVersion:    ptrStr("2024.0.0"),
			VehicleName:   ptrStr(displayName),
			IsUserPresent: ptrBool(false),
			SentryMode:    ptrBool(false),
			Df:            ptrInt(0),
			Dr:            ptrInt(0),
			Pf:            ptrInt(0),
			Pr:            ptrInt(0),
			Ft:            ptrInt(0),
			Rt:            ptrInt(0),
			FdWindow:      ptrInt(0),
			FpWindow:      ptrInt(0),
			RdWindow:      ptrInt(0),
			RpWindow:      ptrInt(0),
			SoftwareUpdate: &SoftwareUpdate{
				Status:  ptrStr(""),
				Version: ptrStr(""),
			},
			Timestamp: ptrI64(nowMs),
		},
	}
}

// undocumentedAPIVersion is a plausible Fleet API version TeslaMate tolerates.
const undocumentedAPIVersion = 71
