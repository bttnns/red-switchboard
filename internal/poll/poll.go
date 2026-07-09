// Package poll runs a single background goroutine that refreshes a cached
// canonical snapshot on an adaptive, deliberately CONSERVATIVE cadence and
// serves it from memory. This is the key rate-limit decoupling: stock TeslaMate
// polls the sink's API surface every ~2.5s, but those reads hit only the cache,
// never the source. The poll loop alone decides how often to actually call the
// source, based on the derived (canonical) vehicle state, with jitter, a hard
// floor, and exponential backoff on rate-limit/errors.
//
// It is vendor-agnostic: it depends only on source.Source (which returns the
// neutral vehicle.Snapshot), so the same loop drives any input plugin.
package poll

import (
	"context"
	"log"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/cenkalti/backoff/v5"
)

// Built-in fallbacks for the tunables that are otherwise configurable per
// Intervals. They match the historical hardcoded values, so behavior is
// unchanged when the config omits them (Intervals leaves the field zero and the
// Poller falls back to these).
const (
	// defaultMinInterval is the hard floor: never poll the source faster than
	// this, whatever the configured intervals say.
	defaultMinInterval = 5 * time.Second
	// defaultMaxBackoff caps the exponential backoff applied on repeated errors /
	// rate limits.
	defaultMaxBackoff = 15 * time.Minute
	// defaultJitterPct is the +-randomization (as a fraction of the interval)
	// applied to every scheduled poll, to avoid synchronized polling.
	defaultJitterPct = 0.1
	// defaultAwakeBackoffCap bounds how far an AWAKE-but-parked car's cadence may
	// stretch under change-adaptive backoff.
	defaultAwakeBackoffCap = 5 * time.Minute
	// defaultQuotaBlockFloor is the near-fixed backoff for a billing/quota block:
	// long enough that a sustained cap is not hammered, short enough to notice
	// when the operator raises the limit or the cycle resets.
	defaultQuotaBlockFloor = time.Hour
	// defaultDCThresholdKw is the delivered power (kW) at or above which a charge
	// session counts as DC for cadence when the source gives no fast-charger flag.
	// Mirrors dcChargeThresholdKw in the fleet sink mapping; kept duplicated rather
	// than coupling two unrelated packages over one constant.
	defaultDCThresholdKw = 25.0
)

// Cache is the served-snapshot writer the poll loop writes THROUGH when a stream
// service is active. internal/cache.Service (and its per-vehicle *Merger)
// implements it. nil means the Poller writes p.snapshot directly (today's
// behavior, unchanged). The edge is poll -> (interface) <- stream, so stream
// never imports poll (no cycle); serve.go does the wiring. The Poller keeps its
// OWN last-poll snapshot for cadence/stats and never reads merged content back.
// The merged snapshot is returned for tests; the Poller ignores it.
type Cache interface {
	MergePoll(id string, snap vehicle.Snapshot) vehicle.Snapshot
	// LastStreamAt reports when a streamed frame last refreshed the vehicle's live
	// fields (zero if never). The poll loop reads it to back its drive cadence off
	// while the stream is fresh. Unknown id returns zero.
	LastStreamAt(id string) time.Time
}

// Intervals is the adaptive cadence per derived vehicle state, plus the
// cross-cutting timing tunables. Zero values for the tunables fall back to the
// built-in defaults above, so a partial config stays safe.
type Intervals struct {
	Online   time.Duration // awake but parked
	Driving  time.Duration // shift in D/R/N
	Charging time.Duration // active charge (AC / default)
	Asleep   time.Duration // power=sleep
	Default  time.Duration // fallback

	// ChargingDC is the cadence while DC fast charging; Charging is the AC/default
	// rate. DCThresholdKw is the power (kW) at or above which a session counts as DC
	// when the source supplies no fast-charger flag. Zero ChargingDC falls back to
	// Charging (no AC/DC split); zero DCThresholdKw falls back to the default.
	ChargingDC    time.Duration
	DCThresholdKw float64

	// DrivingStreaming is the (slower) drive cadence used while a telemetry stream
	// is fresh (the stream fills live position between polls). StreamFreshWithin is
	// how recently a frame must have arrived to count as fresh. Both require a
	// stream Cache wired (SetCache); zero DrivingStreaming disables the backoff
	// (always Driving). On a stall the loop snaps back to Driving within one tick.
	DrivingStreaming  time.Duration
	StreamFreshWithin time.Duration

	// MinInterval is the hard floor on the poll cadence (and the minimum error
	// backoff). Zero falls back to defaultMinInterval.
	MinInterval time.Duration
	// MaxBackoff caps the exponential error/rate-limit backoff. Zero falls back
	// to defaultMaxBackoff.
	MaxBackoff time.Duration
	// JitterPct is the +-randomization fraction applied to each scheduled poll.
	// Zero falls back to defaultJitterPct.
	JitterPct float64
	// AwakeBackoffCap bounds the change-adaptive cadence stretch for an
	// awake-but-parked car. Zero falls back to defaultAwakeBackoffCap.
	AwakeBackoffCap time.Duration
	// QuotaBlockFloor is the near-fixed backoff applied on a billing/quota block
	// (a 403 "account disabled: EXCEEDED_LIMIT"-style cap that clears only when
	// the limit is raised or the cycle resets). Unlike error/rate-limit backoff
	// this is NOT exponential: the block is sustained, so retrying on an
	// exponential-from-minInterval cadence only hammers a dead API. A server
	// Retry-After (which can be hours) still wins when present. Zero falls back
	// to defaultQuotaBlockFloor.
	QuotaBlockFloor time.Duration
}

// Stats is a point-in-time view of the loop's health, for the /status and
// /stats introspection endpoints.
type Stats struct {
	FetchedAt            time.Time     // time of the last successful snapshot
	LastSuccessAt        time.Time     // same as FetchedAt; kept for clarity
	LastError            string        // most recent error, "" when healthy
	LastErrorAt          time.Time     // when LastError occurred
	ConsecutiveFailures  int           // poll failures since the last success
	NeedsReauth          bool          // creds appear dead; re-run auth
	Backoff              time.Duration // current backoff delay (0 when healthy)
	SuccessCount         int64         // successful refreshes
	ErrorCount           int64         // failed polls
	RateLimitedCount     int64         // polls rejected with RATE_LIMIT/429
	ChangedCount         int64         // successful polls whose content actually changed
	LastChangeAt         time.Time     // when the cached content last changed
	StartedAt            time.Time     // when this loop began (for rate math)
	ConsecutiveIdle      int           // idle polls in a row (drives change-adaptive backoff)
	QuotaBlockedCount    int64         // polls rejected with a billing/quota block (403 account disabled)
	TelemetryConfigWiped bool          // last error wiped the vendor's push-telemetry config (needs re-pair)
	QuotaBlockedUntil    time.Time     // when the current quota backoff expires (zero when not blocked)

	VehicleDataFetches  int64            // online polls that fetched (billed) vehicle_data
	PollsByState        map[string]int64 // successful polls per derived state (asleep/online/driving/charging_ac/charging_dc/...)
	ScheduledInterval   time.Duration    // last scheduled (pre-jitter) poll interval
	DerivedState        string           // last derived state
	StreamBackoffActive bool             // last drive poll backed off because the stream was fresh
}

// Poller polls one vehicle (by source-native id) through a source.Source and
// caches the latest canonical snapshot. One Poller per vehicle gives each car
// independent cadence and backoff, so one car's outage never stalls another.
type Poller struct {
	src       source.Source
	id        string
	intervals Intervals
	logger    *log.Logger
	eb        *backoff.ExponentialBackOff

	// resolved timing tunables (Intervals zero values filled with defaults)
	minInterval       time.Duration
	maxBackoff        time.Duration
	jitterPct         float64
	awakeBackoffCap   time.Duration
	quotaBlockFloor   time.Duration
	dcThresholdKw     float64
	drivingStreaming  time.Duration
	streamFreshWithin time.Duration

	mu       sync.RWMutex
	snapshot vehicle.Snapshot
	cache    Cache // set via Manager.SetCache; nil = write p.snapshot directly (backward compatible)

	// pollNow wakes Run for an immediate off-schedule poll when the stream observes
	// a session boundary (drive/charge start or end). Buffered+coalesced so a burst
	// of triggers causes at most one extra poll.
	pollNow chan struct{}

	// resilience/observability counters (guarded by mu)
	lastSuccessAt        time.Time
	lastErr              error
	lastErrAt            time.Time
	consecutiveFailures  int
	needsReauth          bool
	lastBackoff          time.Duration
	successCount         int64
	errorCount           int64
	rateLimitedCount     int64
	changedCount         int64
	lastChangeAt         time.Time
	startedAt            time.Time
	consecutiveIdle      int // polls in a row with no change AND no fresh upstream sync
	quotaBlockedCount    int64
	telemetryConfigWiped bool
	quotaBlockedUntil    time.Time

	// cost/cadence observability
	vehicleDataFetches  int64            // online polls that fetched (billed) vehicle_data
	pollsByState        map[string]int64 // successful polls per derived state
	scheduledInterval   time.Duration    // last scheduled (pre-jitter) interval
	derivedState        string           // last derived state
	streamBackoffActive bool             // last poll backed off because the stream was fresh
}

// New constructs a Poller for one vehicle id. intervals zero values fall back
// to sane defaults.
func New(src source.Source, id string, intervals Intervals, logger *log.Logger) *Poller {
	if logger == nil {
		logger = log.Default()
	}
	if intervals.Online == 0 {
		intervals.Online = 2 * time.Minute
	}
	if intervals.Driving == 0 {
		intervals.Driving = 10 * time.Second
	}
	if intervals.Charging == 0 {
		intervals.Charging = 30 * time.Second
	}
	if intervals.ChargingDC == 0 {
		intervals.ChargingDC = intervals.Charging // no AC/DC split unless configured
	}
	if intervals.Asleep == 0 {
		intervals.Asleep = 15 * time.Minute
	}
	if intervals.Default == 0 {
		intervals.Default = 60 * time.Second
	}
	minInterval := intervals.MinInterval
	if minInterval <= 0 {
		minInterval = defaultMinInterval
	}
	maxBackoff := intervals.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	jitterPct := intervals.JitterPct
	if jitterPct <= 0 {
		jitterPct = defaultJitterPct
	}
	awakeBackoffCap := intervals.AwakeBackoffCap
	if awakeBackoffCap <= 0 {
		awakeBackoffCap = defaultAwakeBackoffCap
	}
	quotaBlockFloor := intervals.QuotaBlockFloor
	if quotaBlockFloor <= 0 {
		quotaBlockFloor = defaultQuotaBlockFloor
	}
	dcThresholdKw := intervals.DCThresholdKw
	if dcThresholdKw <= 0 {
		dcThresholdKw = defaultDCThresholdKw
	}
	// DrivingStreaming zero = backoff disabled; when set, fresh-within falls back to
	// the driving cadence's natural window if the operator left it unset.
	streamFreshWithin := intervals.StreamFreshWithin
	if intervals.DrivingStreaming > 0 && streamFreshWithin <= 0 {
		streamFreshWithin = intervals.Driving
	}
	return &Poller{
		src:               src,
		id:                id,
		intervals:         intervals,
		logger:            logger,
		eb:                newBackoff(intervals.Default, maxBackoff),
		startedAt:         time.Now(),
		minInterval:       minInterval,
		maxBackoff:        maxBackoff,
		jitterPct:         jitterPct,
		awakeBackoffCap:   awakeBackoffCap,
		quotaBlockFloor:   quotaBlockFloor,
		dcThresholdKw:     dcThresholdKw,
		drivingStreaming:  intervals.DrivingStreaming,
		streamFreshWithin: streamFreshWithin,
		pollsByState:      make(map[string]int64),
		pollNow:           make(chan struct{}, 1),
	}
}

// ID returns the vehicle this loop refreshes.
func (p *Poller) ID() string { return p.id }

// newBackoff builds the error-path backoff: exponential from the configured
// default interval, doubling, capped at maxBackoff, with +-10% randomization to
// avoid synchronized retries.
func newBackoff(initial, maxBackoff time.Duration) *backoff.ExponentialBackOff {
	if initial <= 0 {
		initial = 60 * time.Second
	}
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	eb := backoff.NewExponentialBackOff()
	eb.InitialInterval = initial
	eb.Multiplier = 2
	eb.RandomizationFactor = 0.1
	eb.MaxInterval = maxBackoff
	eb.Reset()
	return eb
}

// Manager owns one Poller per vehicle id, all sharing a single source (the
// session is account-level). Each loop runs its own goroutine with independent
// cadence and backoff, so one car's outage never stalls another.
type Manager struct {
	pollers map[string]*Poller
	order   []string
}

// NewManager builds a Manager with one Poller per id.
func NewManager(src source.Source, ids []string, intervals Intervals, logger *log.Logger) *Manager {
	m := &Manager{pollers: make(map[string]*Poller, len(ids))}
	for _, id := range ids {
		m.pollers[id] = New(src, id, intervals, logger)
		m.order = append(m.order, id)
	}
	return m
}

// Run starts every vehicle's poll loop; they all stop when ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	for _, p := range m.pollers {
		go p.Run(ctx)
	}
	<-ctx.Done()
}

// SetCache wires a served-snapshot writer (internal/cache.Service) that every
// Poller writes through instead of p.snapshot. Nil-safe: a no-op when never
// called, so the non-streaming serve path is unchanged. Each Poller still keeps
// its own last-poll snapshot for cadence/stats.
func (m *Manager) SetCache(c Cache) {
	for _, p := range m.pollers {
		p.cache = c
	}
}

// IDs returns the managed vehicle ids in their original order.
func (m *Manager) IDs() []string { return m.order }

// Latest returns the cached snapshot for a vehicle (zero value if unknown).
func (m *Manager) Latest(id string) vehicle.Snapshot {
	if p, ok := m.pollers[id]; ok {
		return p.Latest()
	}
	return vehicle.Snapshot{}
}

// PollNow asks the vehicle's Poller to poll immediately (off-schedule). The stream
// path calls it on a session boundary (drive/charge start or end) so TeslaMate gets
// the terminal state without waiting for the next scheduled poll. Unknown id is a
// no-op. Safe for concurrent use.
func (m *Manager) PollNow(id string) {
	if p, ok := m.pollers[id]; ok {
		p.triggerNow()
	}
}

// Stats returns the health view for a vehicle (zero value if unknown).
func (m *Manager) Stats(id string) Stats {
	if p, ok := m.pollers[id]; ok {
		return p.Stats()
	}
	return Stats{}
}

// Latest returns the most recent cached snapshot (may be empty before the first
// successful poll).
func (p *Poller) Latest() vehicle.Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snapshot
}

// Stats returns a point-in-time health view for the introspection endpoints.
func (p *Poller) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := Stats{
		FetchedAt:            p.snapshot.FetchedAt,
		LastSuccessAt:        p.lastSuccessAt,
		LastErrorAt:          p.lastErrAt,
		ConsecutiveFailures:  p.consecutiveFailures,
		NeedsReauth:          p.needsReauth,
		Backoff:              p.lastBackoff,
		SuccessCount:         p.successCount,
		ErrorCount:           p.errorCount,
		RateLimitedCount:     p.rateLimitedCount,
		ChangedCount:         p.changedCount,
		LastChangeAt:         p.lastChangeAt,
		StartedAt:            p.startedAt,
		ConsecutiveIdle:      p.consecutiveIdle,
		QuotaBlockedCount:    p.quotaBlockedCount,
		TelemetryConfigWiped: p.telemetryConfigWiped,
		QuotaBlockedUntil:    p.quotaBlockedUntil,
		VehicleDataFetches:   p.vehicleDataFetches,
		ScheduledInterval:    p.scheduledInterval,
		DerivedState:         p.derivedState,
		StreamBackoffActive:  p.streamBackoffActive,
	}
	if len(p.pollsByState) > 0 {
		s.PollsByState = make(map[string]int64, len(p.pollsByState))
		for k, v := range p.pollsByState {
			s.PollsByState[k] = v
		}
	}
	if p.lastErr != nil {
		s.LastError = p.lastErr.Error()
	}
	return s
}

// Run drives the poll loop until ctx is cancelled. It polls once immediately,
// then schedules the next poll based on the derived state of the result.
func (p *Poller) Run(ctx context.Context) {
	trigger := "initial"
	for {
		next := p.pollOnce(ctx, trigger)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
			trigger = "scheduled"
		case <-p.pollNow:
			// stream saw a session boundary (drive/charge start/end or wake).
			trigger = "boundary"
		}
	}
}

// triggerNow requests an immediate off-schedule poll, unless a successful poll
// happened within minInterval (so a flapping stream cannot spam the source). The
// pollNow channel is buffered+coalesced, so concurrent triggers fold into one.
func (p *Poller) triggerNow() {
	p.mu.RLock()
	tooSoon := !p.lastSuccessAt.IsZero() && time.Since(p.lastSuccessAt) < p.minInterval
	p.mu.RUnlock()
	if tooSoon {
		return
	}
	select {
	case p.pollNow <- struct{}{}:
	default:
	}
}

// pollOnce performs a single poll and returns the delay until the next one. The
// source decides internally whether to fetch a live charging session, so this
// is one call returning the full canonical snapshot.
func (p *Poller) pollOnce(ctx context.Context, trigger string) time.Duration {
	snap, err := p.src.Poll(ctx, p.id)
	if err != nil {
		return p.handleError(err)
	}
	vs := snap.State
	live := snap.Live

	// Healthy poll: reset backoff, cache the snapshot, clear failure state.
	p.eb.Reset()
	now := time.Now()
	if snap.FetchedAt.IsZero() {
		snap.FetchedAt = now
	}
	p.mu.Lock()
	// Did the upstream content actually change (vs just a fresh fetch of identical
	// data)? This quantifies the rate decoupling: data typically changes far less
	// often than either we poll the source or TeslaMate reads us.
	prev := p.snapshot.State
	contentChanged := changed(prev, vs, p.snapshot.Live, live)
	if contentChanged {
		p.changedCount++
		p.lastChangeAt = now
	}
	// Change-adaptive backoff: a poll is "idle" only when our derived content did
	// NOT change AND the car is not pushing fresh data to the cloud (lastSync /
	// newest field timestamp not advancing). Any change or fresh sync resets the
	// streak, snapping the cadence back to base (see intervalFor).
	if contentChanged || syncAdvanced(prev, vs) {
		p.consecutiveIdle = 0
	} else {
		p.consecutiveIdle++
	}
	idle := p.consecutiveIdle
	p.snapshot = snap
	p.lastSuccessAt = now
	p.lastErr = nil
	p.lastBackoff = 0
	p.consecutiveFailures = 0
	p.telemetryConfigWiped = false
	p.quotaBlockedUntil = time.Time{}
	p.successCount++
	recovered := p.needsReauth
	p.needsReauth = false
	// Cost/cadence observability: classify the poll, count it per state, and count
	// the (billed) vehicle_data fetch. The source fetches vehicle_data IFF the
	// summary reported the car online, so an online-derived snapshot == one billed
	// call; asleep/offline polls are summary-only and free.
	state := p.deriveState(vs, live)
	backoff := isDriving(vs) && p.streamFresh()
	interval := p.intervalFor(vs, live, idle)
	p.pollsByState[state]++
	if vs != nil && vs.Power == vehicle.PowerOnline {
		p.vehicleDataFetches++
	}
	prevState := p.derivedState
	p.derivedState = state
	p.scheduledInterval = interval
	p.streamBackoffActive = backoff
	p.mu.Unlock()
	if recovered {
		p.logger.Printf("poll: re-authenticated; polling recovered.")
	}
	// Structured decision trail (P15): one line per poll, plus one per derived-state
	// transition, so an operator can reconstruct why the cadence is what it is. Polls
	// are minutes apart so this is low volume (never per stream frame).
	slog.Info("poll",
		"vehicle", p.id, "state", state, "interval", interval, "trigger", trigger,
		"changed", contentChanged, "stream_backoff", backoff)
	if prevState != "" && prevState != state {
		slog.Info("state transition", "vehicle", p.id, "from", prevState, "to", state)
	}
	// When a stream service is active, the SERVED snapshot is the merged one
	// (stream fields + poll gap-fill) owned by the cache; write it through. The
	// Poller keeps its own raw poll record above for cadence/stats and never reads
	// merged content back. nil cache = p.snapshot is the served cache (today).
	if p.cache != nil {
		p.cache.MergePoll(p.id, snap)
	}

	return p.jitter(interval)
}

// handleError classifies the failure, records it for observability, and returns
// the next delay from the exponential backoff. It NEVER drops the cached
// snapshot: a source failure only affects freshness, not availability.
func (p *Poller) handleError(err error) time.Duration {
	// A billing/quota block is sustained (hours to days), not transient, so it gets
	// a near-fixed backoff independent of the exponential error backoff: retrying
	// on an exponential-from-minInterval cadence would hammer a dead API. A server
	// Retry-After (which can be hours) still wins when present.
	if source.IsQuotaBlocked(err) {
		return p.handleQuotaBlock(err)
	}

	d := p.eb.NextBackOff()
	if d < p.minInterval {
		d = p.minInterval
	}
	// A server-specified backoff (Retry-After / RateLimit-Reset) wins over our own
	// exponential guess: obey the API rather than hammer it (the Owner API returns
	// multi-hour lockouts that we must not retry through). It is NOT capped at
	// maxBackoff: a multi-hour Retry-After must be honored in full.
	if ra, ok := source.RetryAfter(err); ok && ra > d {
		d = ra
	}

	p.mu.Lock()
	p.errorCount++
	p.consecutiveFailures++
	p.lastErr = err
	p.lastErrAt = time.Now()
	p.lastBackoff = d
	switch {
	case source.IsUnauthenticated(err):
		// The source already retried its own re-auth once; a persistent
		// UNAUTHENTICATED means the stored session is dead. Log the actionable
		// message ONCE (circuit-breaker style), not on every backed-off poll, and
		// keep serving last-known cache. The backoff cadence throttles re-attempts.
		firstTime := !p.needsReauth
		p.needsReauth = true
		p.mu.Unlock()
		if firstTime {
			p.logger.Printf("poll: UNAUTHENTICATED after source retry; mint fresh creds with the login tool (e.g. rivian_auth). Serving last-known cache.")
		}
		return d
	case source.IsRateLimited(err):
		p.rateLimitedCount++
		p.mu.Unlock()
		p.logger.Printf("poll: rate limited by source; backing off %s.", d.Round(time.Second))
		return d
	default:
		p.mu.Unlock()
		p.logger.Printf("poll: poll error (backing off %s): %v", d.Round(time.Second), err)
		return d
	}
}

// handleQuotaBlock records a billing/quota block and returns the near-fixed
// backoff. The exponential error backoff is reset so a sustained block does not
// ratchet it toward max (and so recovery starts clean); the quota delay is
// computed independently of p.eb.
func (p *Poller) handleQuotaBlock(err error) time.Duration {
	d := p.quotaBlockFloor
	if ra, ok := source.RetryAfter(err); ok && ra > d {
		d = ra
	}
	p.mu.Lock()
	p.errorCount++
	p.consecutiveFailures++
	p.quotaBlockedCount++
	p.telemetryConfigWiped = source.TelemetryConfigWiped(err)
	p.quotaBlockedUntil = time.Now().Add(d)
	p.lastErr = err
	p.lastErrAt = time.Now()
	p.lastBackoff = d
	p.mu.Unlock()
	p.eb.Reset()
	p.logger.Printf("poll: QUOTA BLOCKED by source (%v); backing off %s. "+
		"Telemetry config likely wiped; re-pair/reconfigure Fleet Telemetry after the cap is raised.",
		err, d.Round(time.Second))
	return d
}

// idleGrace is how many consecutive idle polls to tolerate at the base cadence
// before the change-adaptive backoff starts stretching the interval.
const idleGrace = 3

// intervalFor picks the cadence for the next poll from the derived (canonical)
// state. Awake-but-parked states get change-adaptive backoff (capped). Driving
// and charging are NEVER stretched; a confirmed-asleep car keeps its asleep
// cadence.
func (p *Poller) intervalFor(vs *vehicle.State, live *vehicle.LiveSession, idle int) time.Duration {
	switch {
	case isDriving(vs):
		// Back off while a telemetry stream is fresh: it fills live position between
		// polls, so the poll only needs to refresh poll-owned fields. A stalled
		// stream falls through to the fast Driving cadence within one tick.
		if p.streamFresh() {
			return p.drivingStreaming
		}
		return p.intervals.Driving
	case isCharging(vs, live):
		if isDC(live, p.dcThresholdKw) {
			return p.intervals.ChargingDC
		}
		return p.intervals.Charging
	case vs != nil && vs.Power == vehicle.PowerSleep:
		return p.intervals.Asleep
	case vs != nil && vs.Power == vehicle.PowerOnline:
		return stretch(p.intervals.Online, idle, p.awakeBackoffCap)
	default:
		return stretch(p.intervals.Default, idle, p.awakeBackoffCap)
	}
}

// stretch lengthens a base poll interval the longer a vehicle sits idle. For
// each idle poll beyond idleGrace the interval doubles, clamped to ceil.
func stretch(base time.Duration, idle int, ceil time.Duration) time.Duration {
	if ceil < base {
		ceil = base
	}
	if idle <= idleGrace {
		return base
	}
	steps := idle - idleGrace
	if steps > 20 { // guard against shift overflow on a long-idle car
		steps = 20
	}
	d := base << steps
	if d <= 0 || d > ceil {
		d = ceil
	}
	return d
}

// syncAdvanced reports whether the car pushed fresh data to the cloud between
// two snapshots: either the newest field timestamp or the cloud lastSync moved
// forward.
func syncAdvanced(old, new *vehicle.State) bool {
	if old == nil || new == nil {
		return old != new
	}
	return new.LastUpdate.After(old.LastUpdate) || new.CloudLastSync.After(old.CloudLastSync)
}

func isDriving(vs *vehicle.State) bool {
	if vs == nil {
		return false
	}
	switch vs.Gear {
	case vehicle.GearDrive, vehicle.GearReverse, vehicle.GearNeutral:
		return true
	}
	return false
}

// streamFresh reports whether a telemetry stream refreshed this vehicle within
// StreamFreshWithin (so the drive cadence may back off). False when no stream
// cache is wired or the backoff is disabled.
func (p *Poller) streamFresh() bool {
	if p.drivingStreaming <= 0 || p.cache == nil {
		return false
	}
	last := p.cache.LastStreamAt(p.id)
	return !last.IsZero() && time.Since(last) < p.streamFreshWithin
}

// deriveState classifies a snapshot into the cadence/cost state used for the
// per-state poll counters and the derived-state gauge. It mirrors intervalFor's
// branches and splits charging into AC/DC.
func (p *Poller) deriveState(vs *vehicle.State, live *vehicle.LiveSession) string {
	switch {
	case isDriving(vs):
		return "driving"
	case isCharging(vs, live):
		if isDC(live, p.dcThresholdKw) {
			return "charging_dc"
		}
		return "charging_ac"
	case vs != nil && vs.Power == vehicle.PowerSleep:
		return "asleep"
	case vs != nil && vs.Power == vehicle.PowerOnline:
		return "online"
	case vs != nil && vs.Power == vehicle.PowerOffline:
		return "offline"
	default:
		return "unknown"
	}
}

func isCharging(vs *vehicle.State, live *vehicle.LiveSession) bool {
	if live != nil {
		return true
	}
	return vs != nil && vs.Charger == vehicle.ChargerCharging
}

// isDC reports whether an active charge session is DC fast charging, for cadence.
// The source's fast-charger flag is authoritative (a tapered DC session stays DC);
// absent it, delivered power at or above thresholdKw is the fallback (covers
// stream-derived sessions, which carry power but no flag).
func isDC(live *vehicle.LiveSession, thresholdKw float64) bool {
	if live == nil {
		return false
	}
	return live.FastCharger || (thresholdKw > 0 && live.PowerKw >= thresholdKw)
}

// changed reports whether the meaningful content of the snapshot moved between
// two polls (not just the fetch time). It compares the fields a consumer acts
// on: odometer, SOC, the derived-state enums, charge port, location, and whether
// a live charging session is present. A nil-to-non-nil transition counts. The
// first successful poll (old == nil) is always a change.
func changed(old, new *vehicle.State, oldLive, newLive *vehicle.LiveSession) bool {
	if old == nil || new == nil {
		return old != new
	}
	if old.OdometerMeters != new.OdometerMeters ||
		int(old.BatteryLevelPct) != int(new.BatteryLevelPct) ||
		old.Power != new.Power ||
		old.Gear != new.Gear ||
		old.Charger != new.Charger ||
		old.Plug != new.Plug ||
		old.ChargePortOpen != new.ChargePortOpen ||
		old.RangeKm != new.RangeKm {
		return true
	}
	if (oldLive == nil) != (newLive == nil) {
		return true
	}
	if old.Location == nil || new.Location == nil {
		return old.Location != new.Location
	}
	return old.Location.Latitude != new.Location.Latitude ||
		old.Location.Longitude != new.Location.Longitude
}

// jitter applies +-jitterPct randomness and enforces the hard minimum floor.
func (p *Poller) jitter(d time.Duration) time.Duration {
	if d < p.minInterval {
		d = p.minInterval
	}
	delta := float64(d) * p.jitterPct
	j := time.Duration((rand.Float64()*2 - 1) * delta)
	out := d + j
	if out < p.minInterval {
		out = p.minInterval
	}
	return out
}
