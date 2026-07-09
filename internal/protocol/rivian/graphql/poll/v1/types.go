package v1

import "time"

// VehicleState is the flattened, typed snapshot of a Rivian vehicle, parsed
// from the gateway `vehicleState(id:)` GraphQL query. The raw API wraps each
// scalar as `{ timeStamp value }`; this struct flattens those into plain Go
// values. Fields that the API reported with an invalid sensor sentinel
// ("fault", "signal_not_available", "undefined") are left at their zero value
// (the translator should hold last-known for those).
//
// Units (verified against the Rivian python clients and HA):
//   - VehicleMileage:        meters (int)
//   - DistanceToEmpty:       kilometers (int)
//   - GnssSpeed:             meters/second (float)
//   - GnssBearing:           degrees (float)
//   - GnssAltitude:          meters (float)
//   - BatteryLevel:          percent 0-100 (float)
//   - BatteryLimit:          percent 0-100 (float, charge target SOC)
//   - BatteryCapacity:       kWh gross (float, dynamic)
//   - CabinInteriorTemp:     degrees Celsius (float)
//   - TimeToEndOfCharge:     minutes (int)
//   - OtaInstallProgress:    percent 0-100 (float)
//
// Enum string fields are returned raw/lowercase as Rivian sends them, e.g.
// PowerState {go,ready,sleep,standby,vehicle_reset}; GearStatus
// {drive,neutral,park,reverse}; ChargerState {charging_active,
// charging_connecting,charging_ready}; locks {locked,unlocked}; doors/windows/
// closures {open,closed}.
type VehicleState struct {
	// Cloud connection / freshness.
	CloudConnectionOnline bool
	CloudLastSync         time.Time

	// Location and motion.
	Location     *GnssLocation
	GnssSpeed    float64 // m/s
	GnssBearing  float64 // degrees
	GnssAltitude float64 // meters

	// Odometer and range.
	VehicleMileage  int // meters
	DistanceToEmpty int // km

	// Battery / charging.
	BatteryLevel      float64 // percent
	BatteryLimit      float64 // percent (target SOC)
	BatteryCapacity   float64 // kWh gross
	PowerState        string
	GearStatus        string
	DriveMode         string
	ChargerState      string
	ChargerStatus     string
	ChargePortState   string
	TimeToEndOfCharge int // minutes

	// Climate.
	CabinInteriorTemp float64 // Celsius

	// Climate controls (poll-safe). Seat/steering heat are "Off"/"Level_1..3".
	SeatFrontLeftHeat             string
	SeatFrontRightHeat            string
	SeatRearLeftHeat              string
	SeatRearRightHeat             string
	SteeringWheelHeat             string  // "Off"/"Level_1"
	CabinPreconditioningStatus    string  // e.g. "off"/"active"/"complete_maintain"/"initiate"
	CabinClimateDriverTemperature float64 // Celsius (setpoint)
	DefrostDefogStatus            string  // "Off"/...

	// Security.
	GearGuardVideoStatus string // "disabled"/"enabled"/"engaged"

	// Tire pressure status (string status, e.g. "OK"). Numeric pressures are
	// subscription-only and intentionally not requested here.
	TirePressureStatusFrontLeft  string
	TirePressureStatusFrontRight string
	TirePressureStatusRearLeft   string
	TirePressureStatusRearRight  string

	// Locks (locked/unlocked).
	DoorFrontLeftLocked  string
	DoorFrontRightLocked string
	DoorRearLeftLocked   string
	DoorRearRightLocked  string
	GearGuardLocked      string

	// Doors closed (open/closed).
	DoorFrontLeftClosed  string
	DoorFrontRightClosed string
	DoorRearLeftClosed   string
	DoorRearRightClosed  string

	// Windows closed (open/closed).
	WindowFrontLeftClosed  string
	WindowFrontRightClosed string
	WindowRearLeftClosed   string
	WindowRearRightClosed  string

	// Closures closed (open/closed).
	ClosureFrunkClosed    string
	ClosureLiftgateClosed string
	ClosureTonneauClosed  string
	ClosureTailgateClosed string

	// OTA / software update.
	OtaCurrentVersion   string
	OtaAvailableVersion string
	OtaStatus           string
	OtaInstallProgress  float64 // percent
	OtaDownloadProgress int     // percent

	// LastUpdate is the most recent timeStamp seen across the flattened
	// scalar fields, useful as a snapshot time when gnssLocation is absent.
	LastUpdate time.Time
}

// GnssLocation is a parsed GPS fix.
type GnssLocation struct {
	Latitude  float64
	Longitude float64
	TimeStamp time.Time
}

// LiveSessionData is the flattened live charging session, parsed from the
// chrg `getLiveSessionData(vehicleId:)` query. A nil *LiveSessionData (with no
// error) means there is no active charging session; callers should fall back
// to VehicleState.ChargerState / ChargerStatus.
//
// Units:
//   - Power:                  kW (float)
//   - Current:                amps (float)
//   - KilometersChargedPerHr:  km/h (float)
//   - RangeAddedThisSession:   km (float)
//   - Soc:                     percent 0-100 (float)
//   - TimeRemaining:           seconds (int; API sends it as a String)
//   - TotalChargedEnergy:      kWh, cumulative for the session (float)
//   - CurrentPrice:            money in CurrentCurrency (float)
//   - TimeElapsed:             seconds (int)
type LiveSessionData struct {
	Power                  float64
	Current                float64
	KilometersChargedPerHr float64
	RangeAddedThisSession  float64
	Soc                    float64
	TimeRemaining          int
	TotalChargedEnergy     float64
	VehicleChargerState    string
	CurrentPrice           float64
	CurrentCurrency        string
	TimeElapsed            int
}
