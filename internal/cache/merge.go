// Package cache is the cache-side glue for pushed records: it implements
// streamsource.RecordSink and streamsink.CacheWatcher, merging pushed snapshots
// with polled ones per-field and fanning cache changes out to subscribers. It
// imports only internal/vehicle + stdlib (never internal/poll): the poll loop
// calls IN through poll.Cache, which *Merger satisfies.
//
// One *Merger per served vehicle gives each car its own merge mutex and
// subscriber fan-out, mirroring internal/poll's one-Poller-per-vehicle isolation:
// a slow consumer or a stalled stream on one car never blocks another.
package cache

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// subscriberBuf bounds each subscriber's change channel. A slow consumer that
// does not drain gets events dropped (the next one it drains carries the latest
// state); the cache writer is never blocked. 4 absorbs a short burst without
// dropping, while a sustained backlog drops rather than stalls.
const subscriberBuf = 4

// streamedFields lists the canonical State fields a push source owns while fresh.
// A poll never overwrites these while StreamFields is newer than the poll's
// FetchedAt; it always owns the rest (charge_state detail, config, OTA, locks,
// ...). Each entry pairs a presence bit with the field copy, so the merge updates
// only the fields a frame actually carried (push sources send field-level
// deltas, not full snapshots).
var streamedFields = []struct {
	bit  vehicle.StreamField
	copy func(dst, src *vehicle.State)
}{
	{vehicle.StreamLoc, func(d, s *vehicle.State) { d.Location = s.Location }},
	{vehicle.StreamSpeed, func(d, s *vehicle.State) { d.SpeedMps = s.SpeedMps }},
	{vehicle.StreamHeading, func(d, s *vehicle.State) { d.HeadingDeg = s.HeadingDeg }},
	{vehicle.StreamGear, func(d, s *vehicle.State) { d.Gear = s.Gear }},
	{vehicle.StreamOdometer, func(d, s *vehicle.State) { d.OdometerMeters = s.OdometerMeters }},
	{vehicle.StreamSOC, func(d, s *vehicle.State) { d.BatteryLevelPct = s.BatteryLevelPct }},
	{vehicle.StreamRange, func(d, s *vehicle.State) { d.RangeKm = s.RangeKm }},
	{vehicle.StreamCabinTemp, func(d, s *vehicle.State) { d.CabinTempC = s.CabinTempC }},
	{vehicle.StreamOutsideTemp, func(d, s *vehicle.State) { d.OutsideTempC = s.OutsideTempC }},
	{vehicle.StreamTpmsFl, func(d, s *vehicle.State) { d.TpmsPressureFlBar = s.TpmsPressureFlBar }},
	{vehicle.StreamTpmsFr, func(d, s *vehicle.State) { d.TpmsPressureFrBar = s.TpmsPressureFrBar }},
	{vehicle.StreamTpmsRl, func(d, s *vehicle.State) { d.TpmsPressureRlBar = s.TpmsPressureRlBar }},
	{vehicle.StreamTpmsRr, func(d, s *vehicle.State) { d.TpmsPressureRrBar = s.TpmsPressureRrBar }},
	{vehicle.StreamChargeLimit, func(d, s *vehicle.State) { d.BatteryLimitPct = s.BatteryLimitPct }},
	{vehicle.StreamTimeToFull, func(d, s *vehicle.State) { d.TimeToEndOfChargeMin = s.TimeToEndOfChargeMin }},
	{vehicle.StreamChargerVoltage, func(d, s *vehicle.State) { d.ChargerVoltageV = s.ChargerVoltageV }},
	{vehicle.StreamChargeState, func(d, s *vehicle.State) { d.Charger = s.Charger; d.Plug = s.Plug }},
	{vehicle.StreamBatteryHeater, func(d, s *vehicle.State) { d.BatteryHeaterOn = s.BatteryHeaterOn }},
	{vehicle.StreamLocked, func(d, s *vehicle.State) {
		d.DoorFrontLeftLocked = s.DoorFrontLeftLocked
		d.DoorFrontRightLocked = s.DoorFrontRightLocked
		d.DoorRearLeftLocked = s.DoorRearLeftLocked
		d.DoorRearRightLocked = s.DoorRearRightLocked
	}},
	{vehicle.StreamSentry, func(d, s *vehicle.State) { d.GearGuardStatus = s.GearGuardStatus }},
}

// copyStreamed copies the streamed fields whose bits are set in present from src
// into dst. Absent fields are left untouched (hold-last-known), so a delta frame
// cannot zero a field it did not carry.
func copyStreamed(dst, src *vehicle.State, present vehicle.StreamField) {
	for _, f := range streamedFields {
		if present&f.bit != 0 {
			f.copy(dst, src)
		}
	}
}

// Merger is the single writer per vehicle that both the poll loop and the stream
// source write through. It holds the canonical SERVED snapshot and a notify
// fan-out.
//
// COPY-ON-WRITE invariant: the snapshot stored in m.snap is never mutated in
// place after it is committed. Every merge starts from a fresh *State clone, so a
// snapshot previously handed out by Latest() stays immutable while the next merge
// proceeds. This is what makes Latest() safe to return without a deep copy on the
// read path, and what keeps the streamedFields/Live writes race-free.
type Merger struct {
	id    string
	mu    sync.Mutex
	snap  vehicle.Snapshot
	subs  map[chan struct{}]struct{} // per-subscriber change channels
	drive driveDebouncer             // debounces the drive boundary against gear flap
	wake  wakeDebouncer              // debounces the stream-wake poll trigger from asleep

	// offlineCount/offlineDriving track an ongoing run of "offline" polls so the
	// settled-offline -> asleep reclassification (classifyPollLiveness) is latched at
	// episode start and debounced, rather than recomputed from the gear-cleared poll
	// snapshot each tick.
	offlineCount   int  // consecutive offline polls in the current episode (0 = reachable)
	offlineDriving bool // latched at episode start: the car was driving when it went offline

	// integrity is the shared, reason-labeled stream-integrity rejection counter
	// (one set across all vehicles; the label is the reason, never a VIN). Nil
	// disables the gate (e.g. a bare NewMerger in a test that does not exercise it).
	integrity *integrityCounters
	// session is the shared drive/charge opened/closed counter set (one across all
	// vehicles; labels are kind+edge, never a VIN). Nil skips the counting.
	session *sessionCounters
	// logger emits the single WARN line per rejected field. Nil falls back to the
	// standard logger.
	logger *log.Logger

	// onAsOf, when set, persists the served AsOf high-water mark as it advances so
	// the monotonic clamp survives a restart (see asofStore). Nil = no persistence.
	onAsOf func(id string, at time.Time)

	// gpsAt/odoAt are when the currently-served location/odometer were last set
	// (poll or stream). The stream-integrity implied-velocity checks divide a
	// field's delta by the time since its OWN last reading, so these anchor the GPS
	// and odometer checks instead of the global AsOf (which advances on every frame
	// and would shrink the divisor to the inter-frame gap, false-rejecting a drive).
	gpsAt time.Time
	odoAt time.Time
}

// NewMerger builds a Merger for one vehicle id.
func NewMerger(id string) *Merger {
	return &Merger{id: id, subs: make(map[chan struct{}]struct{})}
}

// newPersistedMerger builds a Merger seeded with the persisted AsOf high-water
// mark (so the clamp resumes there rather than from 0 after a restart) and wired
// to persist further advances. floor never moves served time backwards: the
// maxTime clamp in commit only raises the floor. integrity and logger wire the
// stream-integrity gate (shared counters, one WARN line per rejection).
func newPersistedMerger(id string, floor time.Time, onAsOf func(id string, at time.Time), integrity *integrityCounters, session *sessionCounters, logger *log.Logger) *Merger {
	m := NewMerger(id)
	m.snap.AsOf = floor
	m.onAsOf = onAsOf
	m.integrity = integrity
	m.session = session
	m.logger = logger
	return m
}

// ID returns the vehicle this Merger serves.
func (m *Merger) ID() string { return m.id }

// MergePoll is called by the poll loop's post-Poll path through poll.Cache. The
// poll loop no longer writes its served snapshot directly when a Merger is
// present; it calls here and also keeps its own copy for stats/backoff.
// StreamFields is preserved from the existing snapshot so a poll never resets
// stream provenance. While the stream is fresher than this poll, the streamed
// fields are copied back from prev so a poll cannot regress live drive data.
func (m *Merger) MergePoll(pollSnap vehicle.Snapshot, staleAfter time.Duration) vehicle.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.snap

	out := pollSnap
	out.State = cloneState(pollSnap.State) // never mutate the caller's State
	out.StreamFields = prev.StreamFields   // poll never resets stream freshness
	out.StreamPresent = prev.StreamPresent // poll never changes stream provenance
	streamHoldsLoc, streamHoldsOdo := false, false
	if !prev.StreamFields.IsZero() && prev.StreamFields.After(pollSnap.FetchedAt) && prev.State != nil && out.State != nil {
		// The stream is fresher: protect the fields it has supplied from the
		// (staler) poll by copying them back, field-by-field.
		copyStreamed(out.State, prev.State, prev.StreamPresent)
		if prev.Live != nil {
			out.Live = prev.Live // live charge session owned by stream while fresh
		}
		streamHoldsLoc = prev.StreamPresent&vehicle.StreamLoc != 0
		streamHoldsOdo = prev.StreamPresent&vehicle.StreamOdometer != 0
	}
	// Re-anchor any field the poll actually serves (the stream is not holding a
	// fresher value) to this poll's time, so the next stream frame's implied-velocity
	// check measures against the right reference instant.
	if out.State != nil {
		if !streamHoldsLoc && out.State.Location != nil {
			m.gpsAt = pollSnap.FetchedAt
		}
		if !streamHoldsOdo && out.State.OdometerMeters > 0 {
			m.odoAt = pollSnap.FetchedAt
		}
		m.classifyPollLiveness(out.State, prev)
	}
	m.commit(out, staleAfter)
	return out
}

// offlineSleepConfirmPolls is how many consecutive "offline" polls a settled car
// must show before classifyPollLiveness presents it as asleep. >1 so a single
// transient offline reading mid-charge (which TeslaMate would otherwise abort the
// charge on) cannot trip the reclassification; the second offline poll confirms a
// genuine sleep.
const offlineSleepConfirmPolls = 2

// classifyPollLiveness reclassifies a settled car that Tesla reports "offline" as
// asleep. Tesla's Fleet API labels a deeply-sleeping (often plugged-in) car
// "offline", never "asleep", but across TeslaMate's FSM "offline" means a transient
// dropout to wait out -- e.g. it sits in :charging forever logging "Vehicle went
// offline while charging" -- while "asleep" means settled, so close the session and
// rest. "offline" is kept ONLY for a car lost while DRIVING, where the graduated
// drive-timeout (phantom-drive) guard needs it. Debounced one extra poll so a lone
// transient offline reading cannot abort a live session. Caller holds m.mu; now is
// the merged State; prev is the last committed snapshot, whose gear is the
// last-known one (the bare offline poll carries no gear, so it is read here, before
// commit clobbers it).
func (m *Merger) classifyPollLiveness(now *vehicle.State, prev vehicle.Snapshot) {
	if now.Power != vehicle.PowerOffline {
		m.offlineCount = 0
		m.offlineDriving = false
		return
	}
	if m.offlineCount == 0 {
		// Episode start: latch whether we lost a moving car. Only a known driving gear
		// holds "offline"; unknown/parked is settled and becomes asleep.
		m.offlineDriving = inDrive(gearOf(prev))
	}
	m.offlineCount++
	if !m.offlineDriving && m.offlineCount >= offlineSleepConfirmPolls {
		now.Power = vehicle.PowerSleep
	}
}

// MergeStream is called by streamsource.RecordSink.Put. streamSnap carries only
// the streamed fields meaningfully; non-streamed fields are zero and must not
// overwrite the poll-owned ones. The merge starts from a clone of prev.State.
func (m *Merger) MergeStream(streamSnap vehicle.Snapshot, staleAfter time.Duration) vehicle.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mergeStreamLocked(streamSnap, staleAfter)
}

// mergeStreamLocked is the body of MergeStream; the caller must already hold m.mu.
// Split out so MergeStreamDetect can run the merge and the session/wake detection
// under a SINGLE lock acquisition: otherwise two concurrent frames for one vehicle
// (Tesla's ~120s connection recycle briefly overlaps two read loops) interleave and
// feed mismatched prev/now pairs to the boundary detectors, skewing the counters.
func (m *Merger) mergeStreamLocked(streamSnap vehicle.Snapshot, staleAfter time.Duration) vehicle.Snapshot {
	prev := m.snap
	// Cross-check the frame's numerics against the last trusted values; a rejected
	// field's presence bit is cleared so copyStreamed holds last-known. prev.AsOf is
	// the freshest trusted time (poll+stream) and is the divisor base for the
	// implied-velocity checks. A nil prev.State (first frame) only trips the absolute
	// range checks (SOC/speed), never a regression it has no baseline for.
	present := m.gate(prev.State, streamSnap.State, streamSnap.StreamPresent, m.gpsAt, m.odoAt, streamSnap.FetchedAt)
	// Re-anchor each implied-velocity check to the time its own field was last
	// accepted: a rejected (bit-cleared) field holds last-known and keeps its older
	// anchor, so the divisor is the true window between readings, never the gap to
	// the last unrelated frame.
	if present&vehicle.StreamLoc != 0 {
		m.gpsAt = streamSnap.FetchedAt
	}
	if present&vehicle.StreamOdometer != 0 {
		m.odoAt = streamSnap.FetchedAt
	}
	if prev.State == nil {
		// First-ever data for this vehicle came from the stream before any poll:
		// take it whole, but copy only the gated-present fields onto a zero State so a
		// rejected field is left unknown rather than serving the impossible value (no
		// prior value exists to hold). The poll loop fills the rest shortly.
		out := streamSnap
		out.State = &vehicle.State{}
		if streamSnap.State != nil {
			out.State.Power = streamSnap.State.Power
			copyStreamed(out.State, streamSnap.State, present)
		}
		out.StreamPresent = present
		out.StreamFields = streamSnap.FetchedAt
		m.commit(out, staleAfter)
		return out
	}
	out := prev
	out.State = cloneState(prev.State)
	out.StreamFields = streamSnap.FetchedAt
	// Accumulate presence: once the stream has supplied a field it owns that field
	// (hold-last-known) until the stream stalls. Only the fields this frame
	// carried (and passed the integrity gate) are copied, so a delta frame cannot
	// zero a field it omitted and a rejected field holds last-known.
	out.StreamPresent = prev.StreamPresent | present
	if streamSnap.State != nil {
		copyStreamed(out.State, streamSnap.State, present)
		// Drive-derived liveness: a stream frame implies the car is online.
		if out.State.Power == vehicle.PowerUnknown || out.State.Power == vehicle.PowerOffline {
			out.State.Power = vehicle.PowerOnline
		}
	}
	// Live: the stream's charge frames carry PowerKw (Fleet Telemetry
	// AC/DCChargingPower), CurrentA (ChargeAmps, J1c), and TotalChargedEnergy
	// (AC/DCChargingEnergyIn); the poll owns the rest of the session (time-remaining,
	// fast-charger flag). Update only the streamed fields so a sparse frame cannot
	// zero the poll-owned fields TeslaMate needs -- doing so blanks "Remaining Time"
	// (time_to_full_charge). Only adopt the stream's Live wholesale when the poll has
	// none.
	hasPower := present&vehicle.StreamChargePower != 0 && streamSnap.Live != nil && streamSnap.Live.PowerKw != 0
	hasCurrent := present&vehicle.StreamChargeCurrent != 0 && streamSnap.Live != nil
	hasEnergy := present&vehicle.StreamChargeEnergyIn != 0 && streamSnap.Live != nil
	if hasPower || hasCurrent || hasEnergy {
		if out.Live == nil {
			out.Live = streamSnap.Live
		} else {
			merged := *out.Live
			if hasPower {
				merged.PowerKw = streamSnap.Live.PowerKw
			}
			if hasCurrent {
				merged.CurrentA = streamSnap.Live.CurrentA
			}
			if hasEnergy {
				merged.TotalChargedEnergy = streamSnap.Live.TotalChargedEnergy
			}
			out.Live = &merged
		}
	}
	m.commit(out, staleAfter)
	return out
}

// MergeStreamDetect merges a stream frame and, under the SAME lock, detects whether
// the frame crossed a drive/charge session boundary or confirmed a wake, so two
// concurrent frames for one vehicle can never hand a mismatched prev/now pair to the
// detectors. It returns the merged snapshot and whether the caller should fire an
// immediate poll. Session counters are bumped here (they are independently
// synchronized and nil-safe), mirroring SessionBoundary.
func (m *Merger) MergeStreamDetect(streamSnap vehicle.Snapshot, staleAfter time.Duration) (vehicle.Snapshot, bool) {
	m.mu.Lock()
	prev := m.snap
	merged := m.mergeStreamLocked(streamSnap, staleAfter)
	drive := m.drive.boundary(prev, merged, streamSnap.FetchedAt)
	endCharge := chargeEnded(prev, streamSnap)
	started := chargeStarted(prev, streamSnap)
	boundary := drive || endCharge
	fireWake := false
	if !boundary {
		// Only advance the wake run when the frame did not already cross a session
		// boundary, matching the original Put ordering (SessionBoundary, then a
		// WakeBoundary check only if it did not fire).
		fireWake = m.wake.frame(asleep(prev), streamSnap.FetchedAt)
	}
	m.mu.Unlock()

	if drive {
		m.session.driveBoundary(merged)
	}
	switch {
	case endCharge:
		m.session.chargeClosed()
	case started:
		m.session.chargeOpened()
	}
	return merged, boundary || fireWake
}

// gate runs the stream-integrity cross-check and records each rejection (counter
// + one WARN line, no secret values, vehicle id only). It returns the presence
// bitmask with rejected fields cleared. When no counters are wired (a bare test
// Merger) the gate is disabled and the frame passes unchanged.
func (m *Merger) gate(prev, next *vehicle.State, present vehicle.StreamField, gpsAt, odoAt, now time.Time) vehicle.StreamField {
	if m.integrity == nil {
		return present
	}
	gated, reasons := gateStream(prev, next, present, gpsAt, odoAt, now)
	for _, r := range reasons {
		m.integrity.inc(r)
		m.warnf("stream-integrity: rejected %s field for vehicle %s (held last-known)", r, m.id)
	}
	return gated
}

func (m *Merger) warnf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// commit stores the merged snapshot, applies stale_after degradation for a
// stalled stream, and non-blocking-notifies subscribers. Returns whether content
// changed.
func (m *Merger) commit(snap vehicle.Snapshot, staleAfter time.Duration) bool {
	// Stamp the one canonical emitted time: the freshest of this snapshot's poll
	// and stream times, clamped so it never regresses below what we last served.
	// Both sinks emit snap.AsOf, so a stream frame and a (staler) poll can no longer
	// hand TeslaMate crossing timestamps -- the cause of the stale-fetch discard
	// storm and the end_date < start_date state-machine crash.
	snap.AsOf = maxTime(m.snap.AsOf, snap.FetchedAt, snap.StreamFields)
	if m.onAsOf != nil {
		m.onAsOf(m.id, snap.AsOf)
	}
	if staleAfter > 0 && !snap.StreamFields.IsZero() {
		if age := time.Since(snap.StreamFields); age > staleAfter {
			// Stream stalled: reset presence so a resumed stream re-establishes field
			// ownership rather than protecting stale pre-stall values.
			snap.StreamPresent = 0
			// Degrade liveness to offline ONLY when the poll is also stale. A fresh
			// poll is the authoritative liveness signal (the car is demonstrably
			// online), so a silent stream must not override it: a car that streamed
			// earlier then lost telemetry while still awake would otherwise flip to
			// offline on every poll and fragment the drive. When the poll is stale
			// too, both signals agree the car went silent.
			pollStale := snap.FetchedAt.IsZero() || time.Since(snap.FetchedAt) > staleAfter
			if pollStale && snap.State != nil && snap.State.Power == vehicle.PowerOnline {
				snap.State.Power = vehicle.PowerOffline
			}
		}
	}
	// Structured decision trail (P15): one line when the cache's liveness actually
	// transitions (asleep/offline <-> online), the stream-driven counterpart of the
	// poll loop's derived-state log. Gated on a real Power change so a steady stream
	// of drive frames (Power held online) never logs per frame.
	logPowerTransition(m.id, powerOf(m.snap), powerOf(snap))
	changed := streamChanged(m.snap, snap)
	m.snap = snap
	if changed {
		for ch := range m.subs {
			select {
			case ch <- struct{}{}:
			default:
				// subscriber's buffer is full: drop this event. The next one it
				// drains will carry the latest state. Never block the writer.
			}
		}
	}
	return changed
}

// powerOf returns the snapshot's liveness, or PowerUnknown for a nil State.
func powerOf(s vehicle.Snapshot) vehicle.Power {
	if s.State == nil {
		return vehicle.PowerUnknown
	}
	return s.State.Power
}

// powerName maps a liveness enum to the operator-facing word used in the decision log.
func powerName(p vehicle.Power) string {
	switch p {
	case vehicle.PowerOffline:
		return "offline"
	case vehicle.PowerOnline:
		return "online"
	case vehicle.PowerSleep:
		return "asleep"
	default:
		return "unknown"
	}
}

// logPowerTransition emits one decision line when the cache's liveness changes. It
// no-ops on no change so a steady stream of same-power frames never logs per frame.
func logPowerTransition(id string, from, to vehicle.Power) {
	if from == to {
		return
	}
	slog.Info("state transition", "vehicle", id, "from", powerName(from), "to", powerName(to), "via", "cache")
}

// Latest returns the current cached snapshot (by value; the embedded *State is
// never mutated after commit thanks to copy-on-write, so no deep copy is needed).
func (m *Merger) Latest() vehicle.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

// LastStreamAt returns when a streamed frame last refreshed this vehicle's live
// fields (zero if no stream has ever fed it).
func (m *Merger) LastStreamAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap.StreamFields
}

// Subscribe returns a buffered change channel and removes it when ctx ends. Each
// event signals the subscriber should re-read Latest(); it carries no payload so
// a dropped event loses nothing but a redundant notification.
func (m *Merger) Subscribe(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, subscriberBuf)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
		close(ch)
	}()
	return ch
}

// streamChanged is the STREAMING-grade change test that gates a subscriber notify.
// It is deliberately FINER than internal/poll's changed(): poll.changed() ignores
// SpeedMps/HeadingDeg and int-truncates SOC because those are noise for the poll
// cadence, but they are exactly the fields a live drive frame must carry. A push
// must fire whenever any data:update column moves at full resolution.
func streamChanged(a, b vehicle.Snapshot) bool {
	sa, sb := a.State, b.State
	if (sa == nil) != (sb == nil) {
		return true
	}
	if sa == nil {
		return false
	}
	if sa.Power != sb.Power || sa.Gear != sb.Gear ||
		sa.SpeedMps != sb.SpeedMps || sa.HeadingDeg != sb.HeadingDeg ||
		sa.OdometerMeters != sb.OdometerMeters || sa.RangeKm != sb.RangeKm ||
		sa.BatteryLevelPct != sb.BatteryLevelPct {
		return true
	}
	if locChanged(sa.Location, sb.Location) {
		return true
	}
	return liveChanged(a.Live, b.Live)
}

// cloneState returns a shallow copy of *s with Location cloned too, or nil. It is
// the one allocation per write that buys the copy-on-write invariant.
func cloneState(s *vehicle.State) *vehicle.State {
	if s == nil {
		return nil
	}
	c := *s
	if s.Location != nil {
		loc := *s.Location
		c.Location = &loc
	}
	return &c
}

func locChanged(a, b *vehicle.Location) bool {
	if (a == nil) != (b == nil) {
		return true
	}
	if a == nil {
		return false
	}
	return a.Latitude != b.Latitude || a.Longitude != b.Longitude
}

// maxTime returns the latest of the given times (zero if all are zero).
func maxTime(ts ...time.Time) time.Time {
	var latest time.Time
	for _, t := range ts {
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

func liveChanged(a, b *vehicle.LiveSession) bool {
	if (a == nil) != (b == nil) {
		return true
	}
	if a == nil {
		return false
	}
	return a.PowerKw != b.PowerKw || a.TotalChargedEnergy != b.TotalChargedEnergy
}
