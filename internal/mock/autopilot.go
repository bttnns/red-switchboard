package mock

import (
	"time"
)

// CycleConfig tunes the Autopilot's day-in-the-life loop.
type CycleConfig struct {
	SocMin, SocMax float64       // drive down to SocMin, charge up to SocMax
	DcKw, AcKw     float64       // charge powers; the loop alternates DC and AC
	Idle           time.Duration // rest between legs
	Sleep          time.Duration // duration of an occasional sleep
	Update         time.Duration // duration of an occasional OTA install
	SleepEvery     int           // sleep on every Nth cycle (0 = never)
	UpdateEvery    int           // OTA update on every Nth cycle (0 = never)
}

// DefaultCycle is the out-of-the-box loop: drive max->min, rest, charge min->max,
// rest, repeat; with an occasional sleep and OTA update so those states appear.
func DefaultCycle(socMin, socMax, dcKw, acKw float64) CycleConfig {
	return CycleConfig{
		SocMin: socMin, SocMax: socMax, DcKw: dcKw, AcKw: acKw,
		Idle:        5 * time.Minute,
		Sleep:       45 * time.Minute,
		Update:      15 * time.Minute,
		SleepEvery:  3,
		UpdateEvery: 4,
	}
}

// phase is where a vehicle is in the cycle.
type phase int

const (
	phaseInit phase = iota
	phaseDrive
	phaseRestAfterDrive
	phaseCharge
	phaseRestAfterCharge
	phaseSleep
	phaseUpdate
)

// carCycle is one vehicle's progress through the loop.
type carCycle struct {
	phase  phase
	since  time.Time // sim time the current phase began
	cycles int       // completed drive+charge cycles
	useAC  bool      // alternate DC/AC charging
}

// Autopilot cycles an engine's vehicles through CycleConfig, on whatever clock
// Tick is called with: the wall clock for live serving, or a synthetic clock for
// history generation. It relies on the engine auto-stopping drives/charges at the
// SOC waypoints (it watches for the return to idle), so it only nudges scenarios
// at transitions.
type Autopilot struct {
	eng  *Engine
	cfg  CycleConfig
	cars map[string]*carCycle
}

// NewAutopilot builds an Autopilot for every vehicle currently in eng.
func NewAutopilot(eng *Engine, cfg CycleConfig) *Autopilot {
	a := &Autopilot{eng: eng, cfg: cfg, cars: map[string]*carCycle{}}
	for _, guid := range eng.GUIDs() {
		a.cars[guid] = &carCycle{phase: phaseInit}
	}
	return a
}

// Tick advances every vehicle's cycle to now.
func (a *Autopilot) Tick(now time.Time) {
	for guid, c := range a.cars {
		a.step(guid, c, now)
	}
}

// step runs one vehicle's state machine. Transitions that begin a leg also reset
// the phase clock so the rest/sleep/update timers measure from the right moment.
func (a *Autopilot) step(guid string, c *carCycle, now time.Time) {
	idle := a.eng.Scenario(guid) == ScenarioIdle

	switch c.phase {
	case phaseInit:
		a.startDrive(guid, c, now)

	case phaseDrive:
		if idle { // engine hit the SOC floor
			a.enter(c, phaseRestAfterDrive, now)
		}

	case phaseRestAfterDrive:
		if now.Sub(c.since) >= a.cfg.Idle {
			a.startCharge(guid, c, now)
		}

	case phaseCharge:
		if idle { // engine hit the charge limit
			a.enter(c, phaseRestAfterCharge, now)
		}

	case phaseRestAfterCharge:
		if now.Sub(c.since) >= a.cfg.Idle {
			a.nextCycle(guid, c, now)
		}

	case phaseSleep:
		if now.Sub(c.since) >= a.cfg.Sleep {
			a.startDrive(guid, c, now)
		}

	case phaseUpdate:
		if now.Sub(c.since) >= a.cfg.Update {
			a.startDrive(guid, c, now)
		}
	}
}

// nextCycle decides what follows a completed drive+charge: an occasional sleep or
// OTA update, otherwise the next drive.
func (a *Autopilot) nextCycle(guid string, c *carCycle, now time.Time) {
	c.cycles++
	switch {
	case a.cfg.UpdateEvery > 0 && c.cycles%a.cfg.UpdateEvery == 0:
		a.eng.SetScenario(ScenarioUpdate, guid)
		a.enter(c, phaseUpdate, now)
	case a.cfg.SleepEvery > 0 && c.cycles%a.cfg.SleepEvery == 0:
		a.eng.SetScenario(ScenarioAsleep, guid)
		a.enter(c, phaseSleep, now)
	default:
		a.startDrive(guid, c, now)
	}
}

func (a *Autopilot) startDrive(guid string, c *carCycle, now time.Time) {
	a.eng.SetScenarioOpts(ScenarioDriving, guid, ScenarioOpts{TargetSOC: a.cfg.SocMin})
	a.enter(c, phaseDrive, now)
}

// startCharge alternates DC fast and AC slow charging each cycle.
func (a *Autopilot) startCharge(guid string, c *carCycle, now time.Time) {
	scen, power := ScenarioCharging, a.cfg.DcKw
	if c.useAC {
		scen, power = ScenarioChargingAC, a.cfg.AcKw
	}
	c.useAC = !c.useAC
	a.eng.SetScenarioOpts(scen, guid, ScenarioOpts{TargetSOC: a.cfg.SocMax, PowerKw: power})
	a.enter(c, phaseCharge, now)
}

func (a *Autopilot) enter(c *carCycle, p phase, now time.Time) {
	c.phase = p
	c.since = now
}
