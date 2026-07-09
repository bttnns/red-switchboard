// Package vehicle is the canonical, source-neutral model that every input
// (Rivian, and later Ford, Kia, ...) maps INTO and every output (Tesla, and
// later others) maps OUT OF. It is the hub's lingua franca: instead of N input
// makes times M output APIs direct translators, each side writes one mapper to
// or from this model.
//
// Units are SI throughout (meters, kilometers, m/s, degrees Celsius, kW),
// matching Rivian's already-metric API so the rivian->canonical hop is mostly
// field/enum normalization; the canonical->tesla hop then keeps the existing
// metric->imperial conversions. Load-bearing enums (power, gear, charger,
// plug) are typed so a mapper cannot emit a string the consumer's FSM does not
// understand. Numeric/string fields that arrived empty upstream are left at
// their zero value; the canonical->sink mapper is responsible for hold-last-
// known.
package vehicle

import "time"

// Power is the high-level liveness of a vehicle, derived from the source's
// power state plus whether the source's cloud reports the car reachable.
type Power int

const (
	PowerUnknown Power = iota
	PowerOffline       // not reachable / not reporting
	PowerOnline        // awake (driving, charging, or parked-but-awake)
	PowerSleep         // confirmed asleep
)

// Gear is the canonical drive selector.
type Gear int

const (
	GearUnknown Gear = iota
	GearPark
	GearDrive
	GearReverse
	GearNeutral
)

// ChargerState is the canonical charge activity.
type ChargerState int

const (
	ChargerUnknown    ChargerState = iota
	ChargerIdle                    // plugged but not charging (ready/stopped)
	ChargerCharging                // actively charging (active/connecting)
	ChargerDisconnect              // no charger reading
)

// ChargePlug is whether a cable is connected.
type ChargePlug int

const (
	PlugUnknown ChargePlug = iota
	PlugDisconnected
	PlugConnected
)

// Location is a parsed GPS fix.
type Location struct {
	Latitude  float64
	Longitude float64
	TimeStamp time.Time
}

// Identity is the stable, source-native identity of one vehicle. ID is the
// source's own stable id (e.g. a Rivian GUID); a sink that needs a different id
// type (Tesla wants int64) derives its own from ID.
type Identity struct {
	ID          string // source-native stable id (e.g. Rivian GUID)
	VIN         string
	DisplayName string
	Make        string // "Rivian", ...
	Model       string // "R1T", "R1S", ...
}

// State is the neutral telemetry snapshot: every field the canonical->sink
// mappers read, with source-neutral names/enums and SI units. It carries the
// union of what current sources can report; a field a source cannot supply is
// left at its zero value.
type State struct {
	// Liveness / freshness.
	Power         Power
	UserPresent   bool      // a person is actively using the car (drives is_user_present)
	CloudOnline   bool      // raw cloud-reachable bit (Power derives from it + power state)
	LastUpdate    time.Time // newest field timestamp seen upstream
	CloudLastSync time.Time // when the car last pushed to the source's cloud

	// Motion / location.
	Location   *Location
	SpeedMps   float64 // meters/second
	HeadingDeg float64

	// DrivePowerKw is the instantaneous drive power in kW (negative for regen).
	// Poll-only: Tesla's vehicle_data drive_state.power carries it; Fleet
	// Telemetry has no equivalent. Distinct from LiveSession.PowerKw (charge
	// power). nil = absent (not driving, or source does not report it).
	DrivePowerKw *float64

	// Odometer / range.
	OdometerMeters int // meters
	RangeKm        int // km to empty

	// Battery / charging.
	BatteryLevelPct      float64 // 0-100
	BatteryLimitPct      float64 // 0-100 (target SOC)
	Gear                 Gear
	Charger              ChargerState
	Plug                 ChargePlug
	ChargePortOpen       bool
	TimeToEndOfChargeMin int // minutes (from vehicle state, not the live session)

	// Charger voltage in volts. Poll fills it via the AC/DC inference (synthesized
	// 240 V AC, unset for DC), but Fleet Telemetry streams a real reading, so a
	// streaming car carries the measured voltage. Zero = unreported.
	ChargerVoltageV int
	// BatteryHeaterOn is the battery thermal-management heater state. Poll-owned via
	// battery_heater_on; Fleet Telemetry also streams it for a parked-but-awake car.
	BatteryHeaterOn bool

	// Climate.
	CabinTempC            float64 // Celsius (inside_temp)
	OutsideTempC          float64 // Celsius (ambient/outside_temp)
	DriverSetpointC       float64 // Celsius (0 = unreported)
	SeatHeatFrontLeft     string  // "Off"/"Level_1".."Level_3" (source-native level string)
	SeatHeatFrontRight    string
	SeatHeatRearLeft      string
	SeatHeatRearRight     string
	SteeringWheelHeat     string // "Off"/"Level_x"
	DefrostStatus         string // "Off"/...
	PreconditioningStatus string // "off"/"active"/"complete_maintain"/"initiate"/...

	// Hardware identity the source reports per-snapshot (Tesla's vehicle_config).
	// Source-native strings passed straight to the sink's vehicle_config so a real
	// car shows its true model/trim. Empty when the source does not report them
	// (e.g. a cheap summary poll, or Rivian); the sink then holds last-known or
	// falls back to its configured placeholder.
	CarType     string // e.g. Tesla "models2" / "model3"
	TrimBadging string // e.g. Tesla "p90d"

	// Security.
	GearGuardStatus string // "disabled"/"enabled"/"engaged"

	// Tire pressure status strings (the pollable health signal).
	TpmsFrontLeft  string
	TpmsFrontRight string
	TpmsRearLeft   string
	TpmsRearRight  string

	// Numeric tire pressures in bar. Subscription-only via REST poll (left zero
	// there), but Fleet Telemetry streams them, so a streaming car fills these.
	TpmsPressureFlBar float64
	TpmsPressureFrBar float64
	TpmsPressureRlBar float64
	TpmsPressureRrBar float64

	// Locks (locked/unlocked).
	DoorFrontLeftLocked  string
	DoorFrontRightLocked string
	DoorRearLeftLocked   string
	DoorRearRightLocked  string

	// Doors open/closed ("open"/"closed").
	DoorFrontLeftClosed  string
	DoorFrontRightClosed string
	DoorRearLeftClosed   string
	DoorRearRightClosed  string

	// Windows open/closed ("open"/"closed").
	WindowFrontLeftClosed  string
	WindowFrontRightClosed string
	WindowRearLeftClosed   string
	WindowRearRightClosed  string

	// Closures open/closed ("open"/"closed").
	FrunkClosed    string
	LiftgateClosed string
	TonneauClosed  string
	TailgateClosed string

	// OTA / software update.
	OtaVersion          string  // currently installed
	OtaAvailableVersion string  // available update version
	OtaStatus           string  // source-native status ("installing"/"ready_to_install"/...)
	OtaInstallProgress  float64 // percent
	OtaDownloadProgress int     // percent
}

// LiveSession is the canonical active charging session (nil when no session is
// active). Energy/power numbers are SI; the sink decides AC/DC inference.
type LiveSession struct {
	PowerKw            float64 // kW
	CurrentA           float64 // amps
	TotalChargedEnergy float64 // kWh, cumulative for the session
	TimeRemainingSec   int     // seconds remaining
	// FastCharger is the source's own DC-fast-charge flag (Tesla
	// fast_charger_present), the authoritative AC/DC signal when present. The poll
	// cadence prefers it over the power-threshold fallback, since a DC session that
	// has tapered below the threshold is still DC. Streamed sessions leave it false
	// (Fleet Telemetry carries no such flag); the threshold covers those.
	FastCharger bool
}

// Snapshot is the cached result of one poll: the neutral state, the optional
// live charging session, and when it was fetched. A zero Snapshot (nil State)
// means no successful poll yet.
type Snapshot struct {
	State     *State
	Live      *LiveSession
	FetchedAt time.Time

	// StreamFields, when non-zero, is the time the streamed live fields
	// (Location/Speed/Heading/Gear/Odometer/SOC/Range/charge-power) were last
	// refreshed by a push source. Zero means no stream has ever fed this vehicle.
	// The cache writer uses it to decide whether a poll should overwrite a
	// streamed field (it should not, while StreamFields is fresher than the poll's
	// FetchedAt) and to age a stalled stream out (see internal/cache Merger).
	StreamFields time.Time

	// StreamPresent is a bitmask of the streamed fields a push frame actually
	// carried (StreamLoc|StreamSpeed|...). Push sources send field-level deltas,
	// not synchronized snapshots, so the merge must update only the fields a frame
	// carries and hold the rest last-known: copying an absent field's zero value
	// would clobber a good one. Zero means the frame carried no streamed field (a
	// keepalive) and is dropped before the merge. The cache accumulates the union
	// across frames so the poll-regression guard protects every field the stream
	// has supplied, and resets it when the stream stalls (see internal/cache).
	StreamPresent StreamField

	// AsOf is the ONE canonical emitted timestamp for this snapshot: the freshest
	// of FetchedAt and StreamFields, clamped monotonic non-decreasing per vehicle
	// by the cache Merger. EVERY consumer-facing surface emits this single value --
	// the REST vehicle_data sub-object timestamps AND the streaming data:update
	// prefix -- so the poll and stream paths can never report different "now"s for
	// the same car. They used to: the REST path stamped a poll-owned LastUpdate that
	// froze between polls during a drive while the stream stamped a fresh per-frame
	// time, so TeslaMate discarded every poll as stale mid-drive and, when a drive
	// transition mixed the two clocks, wrote an end_date < start_date that its
	// positive_duration constraint rejected -- crashing the vehicle state machine.
	// Zero only on an un-merged snapshot (direct construction in tests, or the very
	// first frame before commit); AsOfTime falls back to the component max then.
	AsOf time.Time
}

// AsOfTime returns the canonical emitted timestamp: the monotonic AsOf the cache
// Merger stamped, or, for an un-merged snapshot, the freshest of FetchedAt and
// StreamFields. Both sinks emit this so the poll and stream surfaces never disagree.
func (s Snapshot) AsOfTime() time.Time {
	if !s.AsOf.IsZero() {
		return s.AsOf
	}
	if s.StreamFields.After(s.FetchedAt) {
		return s.StreamFields
	}
	return s.FetchedAt
}

// StreamField is a bitmask flag identifying one streamed live field a push frame
// carried. Only streaming sources set these; the canonical model otherwise
// ignores them.
// uint32 (not uint16): an in-memory bitmask only, with no wire encoding, so the
// width is free to widen. J1c pushed the streamed-field count past 16 bits.
type StreamField uint32

const (
	StreamLoc StreamField = 1 << iota
	StreamSpeed
	StreamHeading
	StreamGear
	StreamOdometer
	StreamSOC
	StreamRange
	StreamChargePower
	StreamCabinTemp
	StreamOutsideTemp
	StreamTpmsFl
	StreamTpmsFr
	StreamTpmsRl
	StreamTpmsRr
	StreamChargeLimit
	StreamTimeToFull
	StreamChargerVoltage
	StreamChargeCurrent
	StreamChargeState
	StreamBatteryHeater
	StreamLocked
	StreamSentry
	StreamChargeEnergyIn
)

// IsZeroish reports whether a decoded State carries no usable telemetry: nil
// receiver, or every live field a push frame carries at its zero value. A push
// source calls it to drop an empty/keepalive frame before it reaches the cache,
// so a content-free frame never trips a spurious subscriber notify.
func (s *State) IsZeroish() bool {
	if s == nil {
		return true
	}
	return s.Power == PowerUnknown &&
		s.Gear == GearUnknown &&
		s.Location == nil &&
		s.SpeedMps == 0 &&
		s.HeadingDeg == 0 &&
		s.OdometerMeters == 0 &&
		s.RangeKm == 0 &&
		s.BatteryLevelPct == 0
}
