// Package mock is the protocol-agnostic vehicle simulator. A single synthetic
// engine evolves physically-plausible vehicles in the CANONICAL model
// (internal/vehicle), and because every sink renders canonical data, the mock can
// impersonate ANY protocol by feeding its snapshots through that protocol's sink:
// "mock tesla-fleet-poll-v1" is the Tesla Fleet sink fed by this engine instead of the
// live poll cache. The engine implements sink.Provider directly, so the mock
// command is just engine -> sink.Open(protocol) -> serve.
//
// A scriptable scenario (idle / asleep / driving / charging / charging_ac /
// update) reshapes the vehicle; persistent state (odometer, SOC, range, location,
// version) carries across scenario switches and only moves in plausible
// directions, advancing on a simulated clock (accelerated by --time-scale).
package mock

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// Scenario names.
const (
	ScenarioIdle       = "idle"        // parked, awake, online
	ScenarioAsleep     = "asleep"      // power sleep
	ScenarioDriving    = "driving"     // gear drive, moving GPS + rising odometer
	ScenarioCharging   = "charging"    // DC fast charge (~175 kW)
	ScenarioChargingAC = "charging_ac" // AC charge (~11 kW)
	ScenarioUpdate     = "update"      // OTA install that completes (version bump)
)

// ValidScenario reports whether name is a known scenario.
func ValidScenario(name string) bool {
	switch name {
	case ScenarioIdle, ScenarioAsleep, ScenarioDriving, ScenarioCharging, ScenarioChargingAC, ScenarioUpdate:
		return true
	}
	return false
}

const (
	targetVersion       = "2026.10.1"
	updateCompleteTicks = 20
	effKwhPerKm         = 0.18 // keeps charged energy / range delta consistent
	packKwh             = 128.0
	chargeLimitPct      = 90.0                  // default charge target when a request asks for none
	fullRangeKm         = packKwh / effKwhPerKm // range at 100% SOC; range is derived from SOC so the two never drift
	dcChargeKw          = 175.0                 // default DC fast-charge power
	acChargeKw          = 11.0                  // default AC charge power

	// Driving model: a heavy-footed driver whose speed varies around a brisk
	// average, so drives trace a real path and burn energy at a realistic clip.
	driveBaseMps    = 28.0 // ~63 mph average
	driveAmpMps     = 10.0 // speed swings +/- this around the average
	drivePeriodSec  = 90.0 // seconds per speed oscillation
	driveTurnDeg    = 3.0  // heading drift amplitude (deg/s), so the GPS path curves
	heavyFootFactor = 1.3  // energy multiplier vs the nominal efficiency
	metersPerDegLat = 111320.0
)

// ScenarioOpts tunes a scenario for one SetScenarioOpts call. The zero value
// reproduces the historical behavior (charge to chargeLimitPct, drive forever).
type ScenarioOpts struct {
	// TargetSOC is the SOC the scenario drives toward, then AUTO-STOPS to idle:
	// for charging it is the charge limit; for driving it is the floor to drain
	// down to. 0 means "no target" (charge to the default limit / drive forever).
	TargetSOC float64
	// PowerKw overrides the charge power (e.g. a 35kW public charger). 0 uses the
	// scenario default (175kW DC / 11kW AC).
	PowerKw float64
}

// VehicleSpec describes a vehicle to seed into the engine.
type VehicleSpec struct {
	GUID  string
	VIN   string
	Name  string
	Model string
	Make  string
}

// vehicleSim is one simulated car: persistent continuous state plus the active
// scenario mode.
type vehicleSim struct {
	spec     VehicleSpec
	scenario string
	lastSim  time.Time

	mileageM float64 // odometer, meters (monotonic)
	battery  float64 // SOC percent (range is derived from this)
	lat, lon float64
	curVer   string

	// Driving motion (valid while ScenarioDriving): speed varies and heading
	// drifts so the snapshot reports a moving GPS path, not a fixed point.
	speedMps   float64
	headingDeg float64
	driveSec   float64 // seconds elapsed in the current drive (drives the oscillation)

	chargeTarget float64 // SOC where charging completes then auto-idles (0 = chargeLimitPct)
	driveFloor   float64 // SOC where driving auto-stops to idle (0 = no floor: drive forever)

	live         *vehicle.LiveSession
	livePowerKw  float64
	liveCurrentA float64
	updTicks     int
}

// chargeLimit is the SOC charging stops at: the requested target, or the default.
func (v *vehicleSim) chargeLimit() float64 {
	if v.chargeTarget > 0 {
		return v.chargeTarget
	}
	return chargeLimitPct
}

// Engine owns a set of simulated vehicles advanced on a shared simulated clock.
// It implements sink.Provider. It is safe for concurrent use.
type Engine struct {
	mu        sync.Mutex
	timeScale float64
	realStart time.Time
	simStart  time.Time
	logger    *log.Logger

	order    []string
	vehicles map[string]*vehicleSim
}

// NewEngine builds an engine seeded with the given vehicles, all starting idle
// with a plausible vehicle. timeScale accelerates the simulated clock (1 = real
// time). backfill starts the sim clock that far in the past so an accelerated run
// builds history that catches up to (and is clamped at) real-now (0 disables it).
func NewEngine(specs []VehicleSpec, timeScale float64, backfill time.Duration, logger *log.Logger) *Engine {
	if logger == nil {
		logger = log.Default()
	}
	if timeScale <= 0 {
		timeScale = 1
	}
	if backfill < 0 {
		backfill = 0
	}
	now := time.Now()
	e := &Engine{
		timeScale: timeScale,
		realStart: now,
		simStart:  now.Add(-backfill),
		logger:    logger,
		vehicles:  make(map[string]*vehicleSim, len(specs)),
	}
	for i, sp := range specs {
		if sp.Make == "" {
			sp.Make = "Rivian"
		}
		v := &vehicleSim{
			spec:     sp,
			scenario: ScenarioIdle,
			lastSim:  e.simStart,
			mileageM: 20_000_000 + float64(i)*1_000_000,
			battery:  72 - float64(i)*5,
			lat:      37.7749 + float64(i)*0.01,
			lon:      -122.4194 - float64(i)*0.01,
			curVer:   "2026.6.1",
		}
		e.vehicles[sp.GUID] = v
		e.order = append(e.order, sp.GUID)
	}
	return e
}

// Now returns the engine's current simulated time (the clock Advance/Run use), so
// a live Autopilot can tick on the same clock it serves on.
func (e *Engine) Now() time.Time { return e.simNow() }

// simNow returns the current simulated time (never future-dated).
func (e *Engine) simNow() time.Time {
	sim := e.simStart.Add(time.Duration(float64(time.Since(e.realStart)) * e.timeScale))
	if now := time.Now(); sim.After(now) {
		return now
	}
	return sim
}

// Run advances every vehicle on a 1s real ticker until stop is closed.
func (e *Engine) Run(stop <-chan struct{}) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.mu.Lock()
			now := e.simNow()
			for _, v := range e.vehicles {
				e.advanceLocked(v, now)
			}
			e.mu.Unlock()
		}
	}
}

// Advance steps every vehicle to now. It is the lock-held body of Run exposed for
// driving the engine on an external (synthetic) clock during history generation.
func (e *Engine) Advance(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range e.vehicles {
		e.advanceLocked(v, now)
	}
}

// SnapshotAt returns the canonical snapshot for one vehicle stamped at now (zero
// if unknown). Used by history generation; live serving uses Latest.
func (e *Engine) SnapshotAt(guid string, now time.Time) vehicle.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.vehicles[guid]
	if !ok {
		return vehicle.Snapshot{}
	}
	return e.buildSnapshotLocked(v, now)
}

// Scenario returns one vehicle's active scenario ("" if unknown).
func (e *Engine) Scenario(guid string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.vehicles[guid]; ok {
		return v.scenario
	}
	return ""
}

// GUIDs returns the seeded vehicle GUIDs in registration order.
func (e *Engine) GUIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.order...)
}

// SetScenario switches the scenario for one vehicle (guid=="" applies to all),
// with default tuning (charge to the default limit, drive forever).
func (e *Engine) SetScenario(name, guid string) bool {
	return e.SetScenarioOpts(name, guid, ScenarioOpts{})
}

// SetScenarioOpts is SetScenario with a target SOC + charge power. For driving,
// opts.TargetSOC is a floor the car drains to then AUTO-STOPS (idle); for
// charging it is the limit the car charges to then AUTO-STOPS, at opts.PowerKw.
// A zero opts is exactly SetScenario's historical behavior.
func (e *Engine) SetScenarioOpts(name, guid string, opts ScenarioOpts) bool {
	if !ValidScenario(name) {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	apply := func(v *vehicleSim) {
		v.scenario = name
		v.updTicks = 0
		v.live = nil
		v.chargeTarget = 0
		v.driveFloor = 0
		switch name {
		case ScenarioDriving:
			v.driveFloor = opts.TargetSOC
			v.driveSec = 0
			v.headingDeg = 45
			v.speedMps = driveBaseMps
		case ScenarioCharging:
			v.chargeTarget = opts.TargetSOC
			power := opts.PowerKw
			if power <= 0 {
				power = dcChargeKw
			}
			e.startLive(v, power, 220)
		case ScenarioChargingAC:
			v.chargeTarget = opts.TargetSOC
			power := opts.PowerKw
			if power <= 0 {
				power = acChargeKw
			}
			e.startLive(v, power, 48)
		}
	}
	if guid == "" {
		for _, v := range e.vehicles {
			apply(v)
		}
		e.logger.Printf("mock: scenario -> %s (all vehicles)", name)
		return true
	}
	v, ok := e.vehicles[guid]
	if !ok {
		return false
	}
	apply(v)
	e.logger.Printf("mock: scenario -> %s (%s)", name, guid)
	return true
}

// Scenarios returns the active scenario per vehicle GUID.
func (e *Engine) Scenarios() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.vehicles))
	for g, v := range e.vehicles {
		out[g] = v.scenario
	}
	return out
}

func (e *Engine) startLive(v *vehicleSim, powerKw, currentA float64) {
	v.livePowerKw = powerKw
	v.liveCurrentA = currentA
	v.live = &vehicle.LiveSession{PowerKw: powerKw, CurrentA: currentA, TimeRemainingSec: 2700}
}

// advanceLocked progresses one vehicle for the simulated time since its last
// advance, so SOC/odometer/energy move at realistic rates regardless of scale.
func (e *Engine) advanceLocked(v *vehicleSim, now time.Time) {
	dt := now.Sub(v.lastSim).Seconds()
	v.lastSim = now
	if dt < 0 {
		dt = 0
	}
	switch v.scenario {
	case ScenarioDriving:
		e.driveLocked(v, dt)
		// With a floor set, stop the drive (return to idle) once drained to it, so
		// a caller can cycle drive -> charge between SOC waypoints.
		if v.driveFloor > 0 && v.battery <= v.driveFloor {
			v.battery = v.driveFloor
			v.scenario = ScenarioIdle
			e.logger.Printf("mock: drive reached %.0f%% floor on %s -> idle", v.driveFloor, v.spec.GUID)
		}
	case ScenarioCharging, ScenarioChargingAC:
		// Charge up to the limit, then stop accruing. A real charge completes at
		// the set limit; stopping lets the sink report a stopped charge so TeslaMate
		// closes the session cleanly instead of charging perpetually at the top.
		limit := v.chargeLimit()
		if v.live != nil && v.battery < limit {
			added := v.livePowerKw / 3600.0 * dt // kWh this step
			v.live.TotalChargedEnergy += added
			v.battery += added / packKwh * 100
			if v.battery >= limit {
				v.battery = limit
				// An explicit target means a caller is cycling SOC waypoints: AUTO-STOP
				// to idle so the next leg (a drive) can begin. With no target, preserve
				// the historical behavior: stay "charging" with the charger idled.
				if v.chargeTarget > 0 {
					v.scenario = ScenarioIdle
					v.live = nil
					e.logger.Printf("mock: charge reached %.0f%% on %s -> idle", limit, v.spec.GUID)
				}
			}
			if v.live != nil && v.live.TimeRemainingSec > 0 {
				v.live.TimeRemainingSec -= int(dt)
				if v.live.TimeRemainingSec < 0 {
					v.live.TimeRemainingSec = 0
				}
			}
		}
	case ScenarioUpdate:
		v.updTicks++
		if v.updTicks == updateCompleteTicks {
			v.curVer = targetVersion
			e.logger.Printf("mock: OTA install complete on %s -> %s", v.spec.GUID, v.curVer)
		}
	}
}

// driveLocked advances one driving vehicle over dt seconds: speed oscillates
// around a brisk average, heading drifts so the GPS path curves, the odometer
// grows by the distance covered, and SOC drains from the energy that distance
// costs a heavy-footed driver.
func (e *Engine) driveLocked(v *vehicleSim, dt float64) {
	v.driveSec += dt
	v.speedMps = driveBaseMps + driveAmpMps*math.Sin(v.driveSec/drivePeriodSec)
	if v.speedMps < 5 {
		v.speedMps = 5
	}
	v.headingDeg = math.Mod(v.headingDeg+driveTurnDeg*math.Sin(v.driveSec/180.0)*dt, 360)

	distM := v.speedMps * dt
	v.mileageM += distM

	// Walk the GPS fix along the current heading by distM meters.
	rad := v.headingDeg * math.Pi / 180
	v.lat += distM * math.Cos(rad) / metersPerDegLat
	v.lon += distM * math.Sin(rad) / (metersPerDegLat * math.Cos(v.lat*math.Pi/180))

	// SOC drain = energy used (kWh) as a fraction of the pack.
	kwh := (distM / 1000.0) * effKwhPerKm * heavyFootFactor
	if v.battery > 0 {
		if v.battery -= kwh / packKwh * 100; v.battery < 0 {
			v.battery = 0
		}
	}
}

// ---- sink.Provider implementation -----------------------------------------

// Vehicles returns the seeded vehicles as canonical identities.
func (e *Engine) Vehicles() []vehicle.Identity {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]vehicle.Identity, 0, len(e.order))
	for _, g := range e.order {
		v := e.vehicles[g]
		out = append(out, vehicle.Identity{
			ID:          v.spec.GUID,
			VIN:         v.spec.VIN,
			DisplayName: v.spec.Name,
			Make:        v.spec.Make,
			Model:       v.spec.Model,
		})
	}
	return out
}

// Latest returns the current canonical snapshot for a vehicle (zero if unknown).
func (e *Engine) Latest(id string) vehicle.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.vehicles[id]
	if !ok {
		return vehicle.Snapshot{}
	}
	return e.buildSnapshotLocked(v, e.simNow())
}

// Stats returns a synthetic healthy poll.Stats (the mock never errors).
func (e *Engine) Stats(id string) poll.Stats {
	now := e.simNow()
	return poll.Stats{FetchedAt: now, LastSuccessAt: now, StartedAt: e.realStart, SuccessCount: 1}
}

// buildSnapshotLocked assembles a canonical Snapshot from a vehicle's persistent
// state plus its active scenario, buttoned-up (locked/closed) and stamped with
// now (the simulated clock for live serving, or a synthetic clock when
// generating history).
func (e *Engine) buildSnapshotLocked(v *vehicleSim, now time.Time) vehicle.Snapshot {
	st := &vehicle.State{
		Power:           vehicle.PowerOnline,
		CloudOnline:     true,
		LastUpdate:      now,
		CloudLastSync:   now,
		BatteryLevelPct: v.battery,
		BatteryLimitPct: v.chargeLimit(),
		OdometerMeters:  int(v.mileageM),
		RangeKm:         int(v.battery / 100 * fullRangeKm), // derived from SOC so range never drifts

		Location:        &vehicle.Location{Latitude: v.lat, Longitude: v.lon, TimeStamp: now},
		Gear:            vehicle.GearPark,
		Charger:         vehicle.ChargerDisconnect,
		Plug:            vehicle.PlugDisconnected,
		CabinTempC:      21.0,
		DriverSetpointC: 21.0,
		OtaVersion:      v.curVer,

		// Buttoned-up defaults.
		DefrostStatus:          "Off",
		PreconditioningStatus:  "off",
		SeatHeatFrontLeft:      "Off",
		SeatHeatFrontRight:     "Off",
		GearGuardStatus:        "enabled",
		TpmsFrontLeft:          "OK",
		TpmsFrontRight:         "OK",
		TpmsRearLeft:           "OK",
		TpmsRearRight:          "OK",
		DoorFrontLeftLocked:    "locked",
		DoorFrontRightLocked:   "locked",
		DoorRearLeftLocked:     "locked",
		DoorRearRightLocked:    "locked",
		DoorFrontLeftClosed:    "closed",
		DoorFrontRightClosed:   "closed",
		DoorRearLeftClosed:     "closed",
		DoorRearRightClosed:    "closed",
		WindowFrontLeftClosed:  "closed",
		WindowFrontRightClosed: "closed",
		WindowRearLeftClosed:   "closed",
		WindowRearRightClosed:  "closed",
		FrunkClosed:            "closed",
		LiftgateClosed:         "closed",
		TonneauClosed:          "closed",
		TailgateClosed:         "closed",
	}

	var live *vehicle.LiveSession
	switch v.scenario {
	case ScenarioAsleep:
		st.Power = vehicle.PowerSleep
	case ScenarioDriving:
		st.Gear = vehicle.GearDrive
		st.UserPresent = true
		st.SpeedMps = v.speedMps
		st.HeadingDeg = v.headingDeg
	case ScenarioCharging, ScenarioChargingAC:
		st.Plug = vehicle.PlugConnected
		st.ChargePortOpen = true
		if v.battery >= v.chargeLimit() {
			// Charge complete at the limit: still plugged in, but no longer drawing
			// power, so the sink reports a stopped charge and TeslaMate closes it.
			st.Charger = vehicle.ChargerIdle
		} else {
			st.Charger = vehicle.ChargerCharging
			if v.live != nil {
				cp := *v.live
				live = &cp
				st.TimeToEndOfChargeMin = v.live.TimeRemainingSec / 60
			}
		}
	case ScenarioUpdate:
		st.OtaAvailableVersion = targetVersion
		if v.updTicks < updateCompleteTicks {
			st.OtaStatus = "installing"
			st.OtaInstallProgress = float64(v.updTicks) / float64(updateCompleteTicks) * 100
		} else {
			st.OtaStatus = "install_success"
			st.OtaInstallProgress = 100
		}
	}

	return vehicle.Snapshot{State: st, Live: live, FetchedAt: now}
}
