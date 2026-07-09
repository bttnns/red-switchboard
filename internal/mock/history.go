package mock

import (
	"iter"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
)

// simStep is the fixed integration granularity the synthetic clock advances by.
// It is fine-grained enough for plausible drive/charge curves yet coarse enough
// that even a multi-year window generates in seconds.
const simStep = 30 * time.Second

// GenerateHistory replays the auto-cycle across [now-since, now] on a synthetic
// clock and yields a sampled canonical snapshot stream. The result is a pull
// iterator (lazy), so an sink.Exporter consumes it without the whole window ever
// living in memory. Sampling cadence adapts to the scenario, and every scenario
// transition is captured.
func (e *Engine) GenerateHistory(since time.Duration, cfg CycleConfig) iter.Seq[sink.HistorySample] {
	return func(yield func(sink.HistorySample) bool) {
		now := time.Now()
		start := now.Add(-since)
		e.prepareForHistory(start, cfg.SocMax)

		auto := NewAutopilot(e, cfg)
		guids := e.GUIDs()
		lastEmit := make(map[string]time.Time, len(guids))
		lastScen := make(map[string]string, len(guids))

		for t := start; !t.After(now); t = t.Add(simStep) {
			e.Advance(t)
			auto.Tick(t)
			for _, g := range guids {
				scen := e.Scenario(g)
				due := scen != lastScen[g] || t.Sub(lastEmit[g]) >= sampleCadence(scen)
				if !due {
					continue
				}
				lastEmit[g], lastScen[g] = t, scen
				if !yield(sink.HistorySample{CanonID: g, Snap: e.SnapshotAt(g, t)}) {
					return
				}
			}
		}
	}
}

// prepareForHistory rewinds the engine to the generation start: every vehicle
// begins idle at startSoc with its clock anchored at start, so the first leg is
// a full drive from the charge ceiling.
func (e *Engine) prepareForHistory(start time.Time, startSoc float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range e.vehicles {
		v.scenario = ScenarioIdle
		v.lastSim = start
		v.battery = startSoc
		v.live = nil
	}
}

// sampleCadence is how often to emit a row in a given scenario: dense while
// moving, periodic while charging, sparse while parked.
func sampleCadence(scenario string) time.Duration {
	switch scenario {
	case ScenarioDriving:
		return simStep
	case ScenarioCharging, ScenarioChargingAC:
		return time.Minute
	default:
		return 10 * time.Minute
	}
}
