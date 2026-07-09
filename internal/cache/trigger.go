package cache

import (
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// driveFlapWindow debounces the drive boundary: a parked<->driving flip that
// reverses within this window of the last fired boundary is treated as a gear
// flap (a P->R->D parking shuffle, a momentary selector blip) and does NOT fork a
// new boundary. Sized to cover a parking shuffle (a few seconds) while staying
// well under any genuine park-then-redrive, so real boundaries still fire.
const driveFlapWindow = 5 * time.Second

// wakeFrameThreshold is how many stream frames must arrive while the car is
// asleep before a wake PollNow fires. >1 so a single stray/buffered frame cannot
// trip a poll, which would defeat TeslaMate sleep; small so a real wake is still
// confirmed within seconds.
const wakeFrameThreshold = 3

// wakeSustainWindow is the minimum span the sleeping-state frames must cover
// before a wake PollNow fires. A burst of buffered frames delivered in the same
// instant is not a real wake; requiring the run to span at least this long means
// the car is genuinely streaming, not flushing a stale buffer.
const wakeSustainWindow = 3 * time.Second

// sessionBoundaryCrossed reports whether a stream frame revealed the start or end
// of a drive or charge session -- the moments TeslaMate needs a fresh poll to open
// or close the session. prev/now are the merged snapshots before/after the frame;
// raw is the frame itself (the charge-stop 0kW reading is dropped by the merge, so
// it is read from raw).
func sessionBoundaryCrossed(prev, raw, now vehicle.Snapshot) bool {
	return driveBoundary(prev, now) || chargeEnded(prev, raw)
}

// SessionBoundary is the debounced counterpart of sessionBoundaryCrossed used on
// the live Put path: the drive boundary is run through this Merger's per-vehicle
// flap debounce (so a parking shuffle does not fork the drive), while charge
// start/end stays undebounced. t is the frame time used to age the debounce window.
// Each fired boundary increments the matching opened/closed session counter (the
// correctness signal) before returning whether a poll should fire.
func (m *Merger) SessionBoundary(prev, raw, now vehicle.Snapshot, t time.Time) bool {
	m.mu.Lock()
	drive := m.drive.boundary(prev, now, t)
	m.mu.Unlock()
	if drive {
		m.session.driveBoundary(now)
	}
	endCharge := chargeEnded(prev, raw)
	switch {
	case endCharge:
		m.session.chargeClosed()
	case chargeStarted(prev, raw):
		m.session.chargeOpened()
	}
	return drive || endCharge
}

// WakeBoundary reports whether sustained stream frames have just confirmed a wake
// from a confirmed-asleep state, so the caller should fire a PollNow that lets the
// poll authoritatively flip the car online and pull the poll-only fields
// (charge_limit, climate, locks). prev is the merged snapshot BEFORE this frame
// (its Power tells us the car was asleep); t is the frame time. The merge
// deliberately keeps an asleep car asleep on a stream frame (so a stray frame
// cannot block TeslaMate sleep), so this trigger, not the merge, drives the wake
// poll. It is edge-triggered: exactly one PollNow per wake, not one per frame.
func (m *Merger) WakeBoundary(prev vehicle.Snapshot, t time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wake.frame(asleep(prev), t)
}

// asleep reports whether the snapshot's liveness is confirmed-asleep.
func asleep(s vehicle.Snapshot) bool {
	return s.State != nil && s.State.Power == vehicle.PowerSleep
}

// wakeDebouncer is the per-vehicle state behind the stream-wake poll trigger: it
// counts the consecutive frames seen while the car is asleep and fires once the run
// reaches wakeFrameThreshold frames spanning wakeSustainWindow. It latches after
// firing so a still-streaming awake car cannot re-fire, and re-arms only when the
// car is observed asleep again. Guarded by the owning Merger's mutex.
type wakeDebouncer struct {
	count int       // consecutive asleep-state frames in the current run
	first time.Time // frame time that opened the current run
	fired bool      // already triggered for this wake; suppresses re-fire
}

// frame advances the wake run with one frame. sleeping is whether the car was
// asleep BEFORE this frame; t is the frame time. It returns true exactly once per
// wake: when the run of asleep-state frames first reaches wakeFrameThreshold
// frames spanning at least wakeSustainWindow. A frame seen while not-asleep resets
// the run and clears the latch, re-arming for the next sleep->wake cycle.
func (w *wakeDebouncer) frame(sleeping bool, t time.Time) bool {
	if !sleeping {
		w.count = 0
		w.fired = false
		return false
	}
	if w.count == 0 {
		w.first = t
	}
	w.count++
	if w.fired || w.count < wakeFrameThreshold || t.Sub(w.first) < wakeSustainWindow {
		return false
	}
	w.fired = true
	return true
}

// driveDebouncer is the per-vehicle state behind the drive-boundary debounce: it
// remembers the last fired parked-vs-driving classification and when, so a flip
// that reverses within driveFlapWindow can be swallowed instead of forking the
// drive into multiple boundaries. Guarded by the owning Merger's mutex.
type driveDebouncer struct {
	class bool      // last fired classification: inDrive(gear)
	at    time.Time // when that classification last fired
	set   bool
}

// boundary reports whether the drive classification settled into a new state,
// applying the flap debounce. prev/now are the merged snapshots before/after the
// frame; t is the frame time. It seeds lazily from prev so the first frame matches
// the undebounced predicate, then suppresses a flip that reverses the last fired
// boundary within driveFlapWindow.
func (d *driveDebouncer) boundary(prev, now vehicle.Snapshot, t time.Time) bool {
	if !d.set {
		// Seed the baseline classification from prev but leave d.at zero: the window
		// measures time since the last FIRED boundary, and none has fired yet, so the
		// first real flip must never be mistaken for a quick reversal.
		d.class = inDrive(gearOf(prev))
		d.set = true
	}
	nc := inDrive(gearOf(now))
	if nc == d.class {
		return false
	}
	// A flip reversing the last fired boundary within the window is a gear flap;
	// swallow it without advancing state so the window stays anchored to the last
	// real boundary (a sustained re-entry past the window still fires).
	if t.Sub(d.at) < driveFlapWindow {
		return false
	}
	d.class = nc
	d.at = t
	return true
}

func gearOf(s vehicle.Snapshot) vehicle.Gear {
	if s.State == nil {
		return vehicle.GearUnknown
	}
	return s.State.Gear
}

// inDrive reports whether the gear is a moving selector (D/R/N), matching how
// TeslaMate treats a drive.
func inDrive(g vehicle.Gear) bool {
	return g == vehicle.GearDrive || g == vehicle.GearReverse || g == vehicle.GearNeutral
}

// driveBoundary fires when the gear crosses between parked/unknown and a moving
// selector in either direction (drive start or drive end).
func driveBoundary(prev, now vehicle.Snapshot) bool {
	p, n := gearOf(prev), gearOf(now)
	return p != n && inDrive(p) != inDrive(n)
}

// chargeEnded fires when the cache was holding a live charge (power > 0) and the
// frame reports charge power dropped to 0. The 0kW reading is taken from the raw
// frame because the merge deliberately keeps the last non-zero power (see Merger),
// and the decode now surfaces a 0 reading rather than dropping it.
func chargeEnded(prev, raw vehicle.Snapshot) bool {
	if prev.Live == nil || prev.Live.PowerKw <= 0 {
		return false
	}
	if raw.StreamPresent&vehicle.StreamChargePower == 0 {
		return false
	}
	return raw.Live == nil || raw.Live.PowerKw == 0
}
