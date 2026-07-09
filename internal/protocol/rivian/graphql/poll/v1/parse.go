package v1

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// flexFloat unmarshals from either a JSON number (1.5) or a numeric string
// ("1.5"). The Rivian live-session endpoint is inconsistent: the charging schema
// types timeElapsed/currentPrice as Float, but captured responses send some of
// them as quoted strings, which a bare float64 field would fail to decode (and
// error the whole getLiveSessionData response). Tolerating both is safest.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return nil // tolerate junk rather than failing the whole decode
	}
	*f = flexFloat(v)
	return nil
}

// invalidSensorStates mirrors HA's INVALID_SENSOR_STATES: values the API uses
// to signal "no reading". A wrapped field reporting one of these (compared
// case-insensitively) is treated as absent (left at the Go zero value) so the
// translator can hold last-known.
var invalidSensorStates = map[string]struct{}{
	"fault":                {},
	"signal_not_available": {},
	"undefined":            {},
}

// tsValue is the raw `{ timeStamp value }` wrapper. value is kept as a
// RawMessage because the API types it as String, Int, Float or Bool depending
// on the field.
type tsValue struct {
	TimeStamp string          `json:"timeStamp"`
	Value     json.RawMessage `json:"value"`
}

// valid reports whether the wrapper carries a usable reading (present and not
// an INVALID_SENSOR_STATES sentinel).
func (t *tsValue) valid() bool {
	if t == nil || len(t.Value) == 0 || string(t.Value) == "null" {
		return false
	}
	if s, ok := t.asString(); ok {
		if _, bad := invalidSensorStates[strings.ToLower(s)]; bad {
			return false
		}
	}
	return true
}

func (t *tsValue) asString() (string, bool) {
	if t == nil || len(t.Value) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(t.Value, &s); err == nil {
		return s, true
	}
	return "", false
}

// str returns the string value, or "" if absent/invalid.
func (t *tsValue) str() string {
	if !t.valid() {
		return ""
	}
	s, _ := t.asString()
	return s
}

// f64 returns the value as float64, accepting either a JSON number or a numeric
// string. Returns 0 if absent/invalid/non-numeric.
func (t *tsValue) f64() float64 {
	if !t.valid() {
		return 0
	}
	var f float64
	if err := json.Unmarshal(t.Value, &f); err == nil {
		return f
	}
	if s, ok := t.asString(); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return 0
}

// i returns the value as int (rounded from float), 0 if absent/invalid.
func (t *tsValue) i() int {
	return int(t.f64())
}

// ts parses the wrapper's timeStamp into a time.Time (RFC3339), zero if empty.
func (t *tsValue) ts() time.Time {
	if t == nil || t.TimeStamp == "" {
		return time.Time{}
	}
	return parseTime(t.TimeStamp)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm
		}
	}
	return time.Time{}
}

// rawVehicleStateResponse is the gateway GetVehicleState response envelope.
type rawVehicleStateResponse struct {
	Data struct {
		VehicleState *rawVehicleState `json:"vehicleState"`
	} `json:"data"`
}

type rawVehicleState struct {
	CloudConnection *struct {
		IsOnline bool   `json:"isOnline"`
		LastSync string `json:"lastSync"`
	} `json:"cloudConnection"`
	GnssLocation *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		TimeStamp string  `json:"timeStamp"`
	} `json:"gnssLocation"`

	GnssSpeed       *tsValue `json:"gnssSpeed"`
	GnssBearing     *tsValue `json:"gnssBearing"`
	GnssAltitude    *tsValue `json:"gnssAltitude"`
	VehicleMileage  *tsValue `json:"vehicleMileage"`
	DistanceToEmpty *tsValue `json:"distanceToEmpty"`
	BatteryLevel    *tsValue `json:"batteryLevel"`
	BatteryLimit    *tsValue `json:"batteryLimit"`
	BatteryCapacity *tsValue `json:"batteryCapacity"`
	PowerState      *tsValue `json:"powerState"`
	GearStatus      *tsValue `json:"gearStatus"`
	DriveMode       *tsValue `json:"driveMode"`
	ChargerState    *tsValue `json:"chargerState"`
	ChargerStatus   *tsValue `json:"chargerStatus"`
	ChargePortState *tsValue `json:"chargePortState"`

	TimeToEndOfCharge               *tsValue `json:"timeToEndOfCharge"`
	CabinClimateInteriorTemperature *tsValue `json:"cabinClimateInteriorTemperature"`
	CabinClimateDriverTemperature   *tsValue `json:"cabinClimateDriverTemperature"`
	CabinPreconditioningStatus      *tsValue `json:"cabinPreconditioningStatus"`
	DefrostDefogStatus              *tsValue `json:"defrostDefogStatus"`

	SeatFrontLeftHeat  *tsValue `json:"seatFrontLeftHeat"`
	SeatFrontRightHeat *tsValue `json:"seatFrontRightHeat"`
	SeatRearLeftHeat   *tsValue `json:"seatRearLeftHeat"`
	SeatRearRightHeat  *tsValue `json:"seatRearRightHeat"`
	SteeringWheelHeat  *tsValue `json:"steeringWheelHeat"`

	GearGuardVideoStatus *tsValue `json:"gearGuardVideoStatus"`

	TirePressureStatusFrontLeft  *tsValue `json:"tirePressureStatusFrontLeft"`
	TirePressureStatusFrontRight *tsValue `json:"tirePressureStatusFrontRight"`
	TirePressureStatusRearLeft   *tsValue `json:"tirePressureStatusRearLeft"`
	TirePressureStatusRearRight  *tsValue `json:"tirePressureStatusRearRight"`

	DoorFrontLeftLocked  *tsValue `json:"doorFrontLeftLocked"`
	DoorFrontLeftClosed  *tsValue `json:"doorFrontLeftClosed"`
	DoorFrontRightLocked *tsValue `json:"doorFrontRightLocked"`
	DoorFrontRightClosed *tsValue `json:"doorFrontRightClosed"`
	DoorRearLeftLocked   *tsValue `json:"doorRearLeftLocked"`
	DoorRearLeftClosed   *tsValue `json:"doorRearLeftClosed"`
	DoorRearRightLocked  *tsValue `json:"doorRearRightLocked"`
	DoorRearRightClosed  *tsValue `json:"doorRearRightClosed"`

	WindowFrontLeftClosed  *tsValue `json:"windowFrontLeftClosed"`
	WindowFrontRightClosed *tsValue `json:"windowFrontRightClosed"`
	WindowRearLeftClosed   *tsValue `json:"windowRearLeftClosed"`
	WindowRearRightClosed  *tsValue `json:"windowRearRightClosed"`

	ClosureFrunkLocked    *tsValue `json:"closureFrunkLocked"`
	ClosureFrunkClosed    *tsValue `json:"closureFrunkClosed"`
	ClosureLiftgateLocked *tsValue `json:"closureLiftgateLocked"`
	ClosureLiftgateClosed *tsValue `json:"closureLiftgateClosed"`
	ClosureTonneauLocked  *tsValue `json:"closureTonneauLocked"`
	ClosureTonneauClosed  *tsValue `json:"closureTonneauClosed"`
	ClosureTailgateLocked *tsValue `json:"closureTailgateLocked"`
	ClosureTailgateClosed *tsValue `json:"closureTailgateClosed"`
	GearGuardLocked       *tsValue `json:"gearGuardLocked"`

	OtaCurrentVersion   *tsValue `json:"otaCurrentVersion"`
	OtaAvailableVersion *tsValue `json:"otaAvailableVersion"`
	OtaStatus           *tsValue `json:"otaStatus"`
	OtaInstallProgress  *tsValue `json:"otaInstallProgress"`
	OtaDownloadProgress *tsValue `json:"otaDownloadProgress"`
}

// flatten converts the raw response into the clean typed VehicleState. A nil
// vehicleState (or nil envelope) yields a zero-value struct.
func (r *rawVehicleStateResponse) flatten() *VehicleState {
	out := &VehicleState{}
	v := r.Data.VehicleState
	if v == nil {
		return out
	}

	var latest time.Time
	track := func(w *tsValue) {
		if t := w.ts(); t.After(latest) {
			latest = t
		}
	}

	if v.CloudConnection != nil {
		out.CloudConnectionOnline = v.CloudConnection.IsOnline
		out.CloudLastSync = parseTime(v.CloudConnection.LastSync)
	}
	if v.GnssLocation != nil {
		out.Location = &GnssLocation{
			Latitude:  v.GnssLocation.Latitude,
			Longitude: v.GnssLocation.Longitude,
			TimeStamp: parseTime(v.GnssLocation.TimeStamp),
		}
		if out.Location.TimeStamp.After(latest) {
			latest = out.Location.TimeStamp
		}
	}

	out.GnssSpeed = v.GnssSpeed.f64()
	out.GnssBearing = v.GnssBearing.f64()
	out.GnssAltitude = v.GnssAltitude.f64()
	out.VehicleMileage = v.VehicleMileage.i()
	out.DistanceToEmpty = v.DistanceToEmpty.i()
	out.BatteryLevel = v.BatteryLevel.f64()
	out.BatteryLimit = v.BatteryLimit.f64()
	out.BatteryCapacity = v.BatteryCapacity.f64()
	out.PowerState = v.PowerState.str()
	out.GearStatus = v.GearStatus.str()
	out.DriveMode = v.DriveMode.str()
	out.ChargerState = v.ChargerState.str()
	out.ChargerStatus = v.ChargerStatus.str()
	out.ChargePortState = v.ChargePortState.str()
	out.TimeToEndOfCharge = v.TimeToEndOfCharge.i()
	out.CabinInteriorTemp = v.CabinClimateInteriorTemperature.f64()
	out.CabinClimateDriverTemperature = v.CabinClimateDriverTemperature.f64()
	out.CabinPreconditioningStatus = v.CabinPreconditioningStatus.str()
	out.DefrostDefogStatus = v.DefrostDefogStatus.str()

	out.SeatFrontLeftHeat = v.SeatFrontLeftHeat.str()
	out.SeatFrontRightHeat = v.SeatFrontRightHeat.str()
	out.SeatRearLeftHeat = v.SeatRearLeftHeat.str()
	out.SeatRearRightHeat = v.SeatRearRightHeat.str()
	out.SteeringWheelHeat = v.SteeringWheelHeat.str()

	out.GearGuardVideoStatus = v.GearGuardVideoStatus.str()

	out.TirePressureStatusFrontLeft = v.TirePressureStatusFrontLeft.str()
	out.TirePressureStatusFrontRight = v.TirePressureStatusFrontRight.str()
	out.TirePressureStatusRearLeft = v.TirePressureStatusRearLeft.str()
	out.TirePressureStatusRearRight = v.TirePressureStatusRearRight.str()

	out.DoorFrontLeftLocked = v.DoorFrontLeftLocked.str()
	out.DoorFrontLeftClosed = v.DoorFrontLeftClosed.str()
	out.DoorFrontRightLocked = v.DoorFrontRightLocked.str()
	out.DoorFrontRightClosed = v.DoorFrontRightClosed.str()
	out.DoorRearLeftLocked = v.DoorRearLeftLocked.str()
	out.DoorRearLeftClosed = v.DoorRearLeftClosed.str()
	out.DoorRearRightLocked = v.DoorRearRightLocked.str()
	out.DoorRearRightClosed = v.DoorRearRightClosed.str()

	out.WindowFrontLeftClosed = v.WindowFrontLeftClosed.str()
	out.WindowFrontRightClosed = v.WindowFrontRightClosed.str()
	out.WindowRearLeftClosed = v.WindowRearLeftClosed.str()
	out.WindowRearRightClosed = v.WindowRearRightClosed.str()

	out.ClosureFrunkClosed = v.ClosureFrunkClosed.str()
	out.ClosureLiftgateClosed = v.ClosureLiftgateClosed.str()
	out.ClosureTonneauClosed = v.ClosureTonneauClosed.str()
	out.ClosureTailgateClosed = v.ClosureTailgateClosed.str()
	out.GearGuardLocked = v.GearGuardLocked.str()

	out.OtaCurrentVersion = v.OtaCurrentVersion.str()
	out.OtaAvailableVersion = v.OtaAvailableVersion.str()
	out.OtaStatus = v.OtaStatus.str()
	out.OtaInstallProgress = v.OtaInstallProgress.f64()
	out.OtaDownloadProgress = v.OtaDownloadProgress.i()

	for _, w := range []*tsValue{
		v.GnssSpeed, v.GnssBearing, v.GnssAltitude, v.VehicleMileage,
		v.DistanceToEmpty, v.BatteryLevel, v.BatteryLimit, v.BatteryCapacity,
		v.PowerState, v.GearStatus, v.DriveMode, v.ChargerState, v.ChargerStatus,
		v.ChargePortState, v.TimeToEndOfCharge, v.CabinClimateInteriorTemperature,
		v.OtaStatus, v.OtaCurrentVersion,
	} {
		track(w)
	}
	out.LastUpdate = latest

	return out
}

// vrValue is the live-session `{ value updatedAt }` wrapper.
type vrValue struct {
	Value     json.RawMessage `json:"value"`
	UpdatedAt string          `json:"updatedAt"`
}

func (w *vrValue) valid() bool {
	if w == nil || len(w.Value) == 0 || string(w.Value) == "null" {
		return false
	}
	return true
}

func (w *vrValue) f64() float64 {
	if !w.valid() {
		return 0
	}
	var f float64
	if err := json.Unmarshal(w.Value, &f); err == nil {
		return f
	}
	var s string
	if err := json.Unmarshal(w.Value, &s); err == nil {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return 0
}

func (w *vrValue) i() int { return int(w.f64()) }

func (w *vrValue) str() string {
	if !w.valid() {
		return ""
	}
	var s string
	if err := json.Unmarshal(w.Value, &s); err == nil {
		return s
	}
	return ""
}

// rawLiveSessionResponse is the chrg getLiveSessionData response envelope. The
// session object is a pointer so a null object is distinguishable from an empty
// one (null => no active session).
type rawLiveSessionResponse struct {
	Data struct {
		Session *rawLiveSession `json:"getLiveSessionData"`
	} `json:"data"`
}

type rawLiveSession struct {
	VehicleChargerState      *vrValue  `json:"vehicleChargerState"`
	Power                    *vrValue  `json:"power"`
	Current                  *vrValue  `json:"current"`
	KilometersChargedPerHour *vrValue  `json:"kilometersChargedPerHour"`
	RangeAddedThisSession    *vrValue  `json:"rangeAddedThisSession"`
	Soc                      *vrValue  `json:"soc"`
	TimeRemaining            *vrValue  `json:"timeRemaining"`
	TotalChargedEnergy       *vrValue  `json:"totalChargedEnergy"`
	TimeElapsed              flexFloat `json:"timeElapsed"`
	CurrentPrice             flexFloat `json:"currentPrice"`
	CurrentCurrency          string    `json:"currentCurrency"`
}

func (s *rawLiveSession) flatten() *LiveSessionData {
	return &LiveSessionData{
		Power:                  s.Power.f64(),
		Current:                s.Current.f64(),
		KilometersChargedPerHr: s.KilometersChargedPerHour.f64(),
		RangeAddedThisSession:  s.RangeAddedThisSession.f64(),
		Soc:                    s.Soc.f64(),
		TimeRemaining:          s.TimeRemaining.i(),
		TotalChargedEnergy:     s.TotalChargedEnergy.f64(),
		VehicleChargerState:    s.VehicleChargerState.str(),
		CurrentPrice:           float64(s.CurrentPrice),
		CurrentCurrency:        s.CurrentCurrency,
		TimeElapsed:            int(s.TimeElapsed),
	}
}
