package poll

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
)

// fakeSource is a controllable source.Source for the poll loop tests. It returns
// a fixed canonical snapshot (or error) and counts calls.
type fakeSource struct {
	snap  vehicle.Snapshot
	err   error
	calls int32
}

func (f *fakeSource) Name() string { return "fake" }
func (f *fakeSource) Vehicles(context.Context) ([]vehicle.Identity, error) {
	return []vehicle.Identity{{ID: "v1"}}, nil
}
func (f *fakeSource) Poll(context.Context, string) (vehicle.Snapshot, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.snap, f.err
}

// classifiedError satisfies source's unauthenticated/rateLimited interfaces so
// the poll loop's error classification can be exercised without a vendor pkg.
type classifiedError struct {
	msg    string
	unauth bool
	rate   bool
}

func (e *classifiedError) Error() string         { return e.msg }
func (e *classifiedError) Unauthenticated() bool { return e.unauth }
func (e *classifiedError) RateLimited() bool     { return e.rate }

// quotaError satisfies source's quotaBlocked/telemetryWiped interfaces so the
// poll loop's quota branch can be exercised without a vendor pkg.
type quotaError struct {
	msg   string
	wiped bool
}

func (e *quotaError) Error() string              { return e.msg }
func (e *quotaError) QuotaBlocked() bool         { return true }
func (e *quotaError) AccountDisabled() bool      { return true }
func (e *quotaError) TelemetryConfigWiped() bool { return e.wiped }

func snapOf(vs *vehicle.State, live *vehicle.LiveSession) vehicle.Snapshot {
	return vehicle.Snapshot{State: vs, Live: live}
}

func TestIntervalForDerivedState(t *testing.T) {
	p := New(&fakeSource{}, "v1", Intervals{
		Online: 2 * time.Minute, Driving: 10 * time.Second,
		Charging: 30 * time.Second, Asleep: 15 * time.Minute, Default: 60 * time.Second,
	}, nil)

	cases := []struct {
		name string
		vs   *vehicle.State
		live *vehicle.LiveSession
		want time.Duration
	}{
		{"driving", &vehicle.State{Gear: vehicle.GearDrive, Power: vehicle.PowerOnline}, nil, 10 * time.Second},
		{"charging by live", &vehicle.State{Gear: vehicle.GearPark, Power: vehicle.PowerOnline}, &vehicle.LiveSession{}, 30 * time.Second},
		{"charging by state", &vehicle.State{Charger: vehicle.ChargerCharging}, nil, 30 * time.Second},
		{"asleep", &vehicle.State{Power: vehicle.PowerSleep}, nil, 15 * time.Minute},
		{"online parked", &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark}, nil, 2 * time.Minute},
		{"default", &vehicle.State{Power: vehicle.PowerOffline}, nil, 60 * time.Second},
	}
	for _, c := range cases {
		got := p.intervalFor(c.vs, c.live, 0)
		if got != c.want {
			t.Errorf("%s: interval = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestChargingACvsDC: a DC session (by fast-charger flag OR by power >= threshold)
// gets the faster ChargingDC cadence; an AC session stays at Charging. With no
// ChargingDC configured the split collapses (DC falls back to Charging).
func TestChargingACvsDC(t *testing.T) {
	p := New(&fakeSource{}, "v1", Intervals{
		Charging: 10 * time.Minute, ChargingDC: 60 * time.Second, DCThresholdKw: 25,
	}, nil)
	charging := &vehicle.State{Charger: vehicle.ChargerCharging, Power: vehicle.PowerOnline}

	assert.Equal(t, 10*time.Minute, p.intervalFor(charging, &vehicle.LiveSession{PowerKw: 7}, 0), "AC: slow cadence")
	assert.Equal(t, 60*time.Second, p.intervalFor(charging, &vehicle.LiveSession{FastCharger: true, PowerKw: 5}, 0), "DC by flag (tapered): fast cadence")
	assert.Equal(t, 60*time.Second, p.intervalFor(charging, &vehicle.LiveSession{PowerKw: 150}, 0), "DC by threshold: fast cadence")
	assert.Equal(t, 10*time.Minute, p.intervalFor(charging, &vehicle.LiveSession{PowerKw: 24.9}, 0), "just under threshold: AC cadence")

	// No split configured: DC collapses to the single Charging cadence.
	noSplit := New(&fakeSource{}, "v1", Intervals{Charging: 30 * time.Second}, nil)
	assert.Equal(t, 30*time.Second, noSplit.intervalFor(charging, &vehicle.LiveSession{PowerKw: 150}, 0), "no ChargingDC: DC uses Charging")
}

// fakeCache is a poll.Cache stub for the stream-aware backoff test: it reports a
// fixed LastStreamAt and records poll writes.
type fakeCache struct {
	lastStream time.Time
}

func (c *fakeCache) MergePoll(string, vehicle.Snapshot) vehicle.Snapshot { return vehicle.Snapshot{} }
func (c *fakeCache) LastStreamAt(string) time.Time                       { return c.lastStream }

// TestStreamAwareDrivingBackoff: while a stream frame is fresh the drive cadence
// backs off to DrivingStreaming; a stale or absent stream (and a nil cache) keeps
// the fast Driving cadence.
func TestStreamAwareDrivingBackoff(t *testing.T) {
	driving := &vehicle.State{Gear: vehicle.GearDrive, Power: vehicle.PowerOnline}
	mk := func() *Poller {
		return New(&fakeSource{}, "v1", Intervals{
			Driving: 60 * time.Second, DrivingStreaming: 2 * time.Minute, StreamFreshWithin: 60 * time.Second,
		}, nil)
	}

	// No cache wired: always the fast cadence.
	assert.Equal(t, 60*time.Second, mk().intervalFor(driving, nil, 0), "nil cache: fast Driving")

	// Fresh stream: back off.
	pFresh := mk()
	pFresh.cache = &fakeCache{lastStream: time.Now()}
	assert.Equal(t, 2*time.Minute, pFresh.intervalFor(driving, nil, 0), "fresh stream: DrivingStreaming")

	// Stale stream: snap back to fast.
	pStale := mk()
	pStale.cache = &fakeCache{lastStream: time.Now().Add(-5 * time.Minute)}
	assert.Equal(t, 60*time.Second, pStale.intervalFor(driving, nil, 0), "stale stream: fast Driving")

	// Never streamed (zero time): fast.
	pNever := mk()
	pNever.cache = &fakeCache{}
	assert.Equal(t, 60*time.Second, pNever.intervalFor(driving, nil, 0), "never streamed: fast Driving")

	// Backoff disabled (DrivingStreaming=0) ignores a fresh stream.
	pOff := New(&fakeSource{}, "v1", Intervals{Driving: 60 * time.Second}, nil)
	pOff.cache = &fakeCache{lastStream: time.Now()}
	assert.Equal(t, 60*time.Second, pOff.intervalFor(driving, nil, 0), "disabled: fast Driving")
}

// TestPollNowTriggersImmediatePoll: a session-boundary trigger wakes Run for an
// off-schedule poll even when the scheduled interval is far away.
func TestPollNowTriggersImmediatePoll(t *testing.T) {
	fs := &fakeSource{snap: snapOf(&vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark}, nil)}
	// Hour-long online cadence: no scheduled poll will fire during the test.
	p := New(fs, "v1", Intervals{Online: time.Hour, MinInterval: time.Nanosecond}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	assert.Eventually(t, func() bool { return atomic.LoadInt32(&fs.calls) >= 1 }, time.Second, 5*time.Millisecond, "startup poll")
	before := atomic.LoadInt32(&fs.calls)
	p.triggerNow()
	assert.Eventually(t, func() bool { return atomic.LoadInt32(&fs.calls) > before }, time.Second, 5*time.Millisecond, "trigger -> off-schedule poll")
}

// TestPollNowUnknownIDNoop: PollNow for an unmanaged id is a no-op, not a panic.
func TestPollNowUnknownIDNoop(t *testing.T) {
	NewManager(&fakeSource{}, []string{"v1"}, Intervals{}, nil).PollNow("nope")
}

// TestPollCostAndStateCounters: each online poll counts a billed vehicle_data
// fetch and increments the per-state counter; an asleep poll is summary-only
// (free) and counts under "asleep". The point-in-time fields track the last poll.
func TestPollCostAndStateCounters(t *testing.T) {
	f := &fakeSource{snap: snapOf(&vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive}, nil)}
	p := New(f, "v1", Intervals{Driving: time.Minute}, nil)
	p.pollOnce(context.Background(), "scheduled")
	p.pollOnce(context.Background(), "scheduled")

	s := p.Stats()
	assert.Equal(t, int64(2), s.VehicleDataFetches, "two online polls = two billed fetches")
	assert.Equal(t, int64(2), s.PollsByState["driving"])
	assert.Equal(t, "driving", s.DerivedState)
	assert.Equal(t, time.Minute, s.ScheduledInterval)
	assert.False(t, s.StreamBackoffActive, "no stream cache wired")

	// Asleep: summary-only, no billed fetch.
	f.snap = snapOf(&vehicle.State{Power: vehicle.PowerSleep}, nil)
	p.pollOnce(context.Background(), "scheduled")
	s = p.Stats()
	assert.Equal(t, int64(2), s.VehicleDataFetches, "asleep poll is free")
	assert.Equal(t, int64(1), s.PollsByState["asleep"])
	assert.Equal(t, "asleep", s.DerivedState)
}

// TestChangeAdaptiveBackoff: an awake-but-parked car's cadence stretches as it
// sits idle but is BOUNDED by awakeBackoffCap, driving/charging are never
// stretched, and a sleeping car keeps its plain asleep cadence.
func TestChangeAdaptiveBackoff(t *testing.T) {
	p := New(&fakeSource{}, "v1", Intervals{
		Online: 2 * time.Minute, Driving: 10 * time.Second,
		Charging: 30 * time.Second, Asleep: 15 * time.Minute, Default: 60 * time.Second,
	}, nil)
	parked := &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark}

	assert.Equal(t, 2*time.Minute, p.intervalFor(parked, nil, 0), "fresh: base cadence")
	assert.Equal(t, 2*time.Minute, p.intervalFor(parked, nil, idleGrace), "within grace: still base")
	assert.Greater(t, p.intervalFor(parked, nil, idleGrace+2), 2*time.Minute, "past grace: stretches")
	assert.LessOrEqual(t, p.intervalFor(parked, nil, 1000), p.awakeBackoffCap, "awake stretch is bounded by the cap")

	driving := &vehicle.State{Gear: vehicle.GearDrive, Power: vehicle.PowerOnline}
	assert.Equal(t, 10*time.Second, p.intervalFor(driving, nil, 1000), "driving is never stretched")
	charging := &vehicle.State{Charger: vehicle.ChargerCharging}
	assert.Equal(t, 30*time.Second, p.intervalFor(charging, nil, 1000), "charging is never stretched")

	asleep := &vehicle.State{Power: vehicle.PowerSleep}
	assert.Equal(t, 15*time.Minute, p.intervalFor(asleep, nil, 1000), "asleep keeps its cadence")
}

func TestSyncAdvancedResetsIdle(t *testing.T) {
	t0 := time.Now()
	old := &vehicle.State{LastUpdate: t0, CloudLastSync: t0}
	sameStale := &vehicle.State{LastUpdate: t0, CloudLastSync: t0}
	assert.False(t, syncAdvanced(old, sameStale), "no advance when timestamps are identical")

	fresher := &vehicle.State{LastUpdate: t0.Add(time.Minute), CloudLastSync: t0}
	assert.True(t, syncAdvanced(old, fresher), "newer field timestamp counts as a fresh sync")
	syncedCloud := &vehicle.State{LastUpdate: t0, CloudLastSync: t0.Add(time.Minute)}
	assert.True(t, syncAdvanced(old, syncedCloud), "newer cloud lastSync counts as a fresh sync")
}

func TestBackoffOnRateLimit(t *testing.T) {
	f := &fakeSource{err: &classifiedError{msg: "rate", rate: true}}
	p := New(f, "v1", Intervals{Default: 60 * time.Second}, nil)

	d1 := p.pollOnce(context.Background(), "scheduled")
	d2 := p.pollOnce(context.Background(), "scheduled")
	if d2 <= d1 {
		t.Errorf("backoff did not grow: d1=%v d2=%v", d1, d2)
	}
}

func TestBackoffResetsOnSuccess(t *testing.T) {
	f := &fakeSource{err: &classifiedError{msg: "rate", rate: true}}
	p := New(f, "v1", Intervals{Default: 60 * time.Second}, nil)

	_ = p.pollOnce(context.Background(), "scheduled")
	dGrown := p.pollOnce(context.Background(), "scheduled")

	f.err = nil
	f.snap = snapOf(&vehicle.State{Power: vehicle.PowerOnline}, nil)
	_ = p.pollOnce(context.Background(), "scheduled")

	f.err = &classifiedError{msg: "rate", rate: true}
	f.snap = vehicle.Snapshot{}
	dAfterReset := p.pollOnce(context.Background(), "scheduled")
	assert.Less(t, dAfterReset, dGrown, "backoff should reset to ~initial after a successful poll")
}

func TestReauthCircuitBreakerAndStats(t *testing.T) {
	f := &fakeSource{err: &classifiedError{msg: "unauth", unauth: true}}
	p := New(f, "v1", Intervals{Default: 60 * time.Second}, nil)

	p.pollOnce(context.Background(), "scheduled")
	p.pollOnce(context.Background(), "scheduled")

	st := p.Stats()
	assert.True(t, st.NeedsReauth, "NeedsReauth should be set after persistent UNAUTHENTICATED")
	assert.Equal(t, 2, st.ConsecutiveFailures)
	assert.Equal(t, int64(2), st.ErrorCount)
	assert.NotEmpty(t, st.LastError)

	f.err = nil
	f.snap = snapOf(&vehicle.State{Power: vehicle.PowerOnline}, nil)
	p.pollOnce(context.Background(), "scheduled")

	st = p.Stats()
	assert.False(t, st.NeedsReauth, "needs-reauth should clear on recovery")
	assert.Zero(t, st.ConsecutiveFailures)
	assert.Equal(t, int64(1), st.SuccessCount)
}

func TestChangedDetectsContentMoves(t *testing.T) {
	base := &vehicle.State{OdometerMeters: 1000, BatteryLevelPct: 72, Power: vehicle.PowerOnline, Gear: vehicle.GearPark}
	same := *base
	assert.False(t, changed(base, &same, nil, nil), "identical content must not count as changed")
	assert.True(t, changed(nil, base, nil, nil), "first poll must count as changed")
	moved := *base
	moved.OdometerMeters = 1020
	assert.True(t, changed(base, &moved, nil, nil), "odometer move must count")
	assert.True(t, changed(base, &same, nil, &vehicle.LiveSession{PowerKw: 100}), "live session start must count")
	wob := *base
	wob.BatteryLevelPct = 72.4
	assert.False(t, changed(base, &wob, nil, nil), "sub-integer SOC wobble must not count")
}

func TestPollerCountsChanges(t *testing.T) {
	f := &fakeSource{snap: snapOf(&vehicle.State{OdometerMeters: 1000, Power: vehicle.PowerOnline}, nil)}
	p := New(f, "v1", Intervals{}, nil)
	p.pollOnce(context.Background(), "scheduled") // first poll: change
	p.pollOnce(context.Background(), "scheduled") // identical: no change
	f.snap = snapOf(&vehicle.State{OdometerMeters: 1100, Power: vehicle.PowerOnline}, nil)
	p.pollOnce(context.Background(), "scheduled") // moved: change
	st := p.Stats()
	assert.Equal(t, int64(3), st.SuccessCount)
	assert.Equal(t, int64(2), st.ChangedCount, "2 of 3 polls changed content")
}

func TestJitterFloor(t *testing.T) {
	p := New(&fakeSource{}, "v1", Intervals{}, nil)
	for i := 0; i < 100; i++ {
		if d := p.jitter(1 * time.Second); d < p.minInterval {
			t.Fatalf("jitter produced %v below floor %v", d, p.minInterval)
		}
	}
}

func TestServingFromCacheDoesNotCallSource(t *testing.T) {
	f := &fakeSource{snap: snapOf(&vehicle.State{Power: vehicle.PowerOnline}, nil)}
	p := New(f, "v1", Intervals{}, nil)
	p.pollOnce(context.Background(), "scheduled")
	calls := atomic.LoadInt32(&f.calls)
	for i := 0; i < 50; i++ {
		_ = p.Latest()
	}
	if atomic.LoadInt32(&f.calls) != calls {
		t.Error("reading cache should not call the source")
	}
}

// TestQuotaBlockBackoff: a billing/quota block produces a near-fixed long
// backoff (the configurable floor, default 1h), NOT the exponential-from-
// minInterval cadence a transient error gets. The block increments
// QuotaBlockedCount, sets TelemetryConfigWiped, and does NOT hot-loop: repeated
// quota polls stay at the floor rather than ratcheting toward max_backoff.
func TestQuotaBlockBackoff(t *testing.T) {
	f := &fakeSource{err: &quotaError{msg: "account disabled: EXCEEDED_LIMIT", wiped: true}}
	p := New(f, "v1", Intervals{Default: 60 * time.Second}, nil)

	d1 := p.pollOnce(context.Background(), "scheduled")
	assert.InDelta(t, time.Hour, d1, float64(15*time.Second), "quota block backs off ~1h, not exponential-from-min")

	d2 := p.pollOnce(context.Background(), "scheduled")
	assert.InDelta(t, time.Hour, d2, float64(15*time.Second), "quota backoff stays ~1h (no hot-loop, no ratchet)")

	st := p.Stats()
	assert.Equal(t, int64(2), st.QuotaBlockedCount, "each quota poll increments QuotaBlockedCount")
	assert.True(t, st.TelemetryConfigWiped, "telemetry-wiped surfaces to stats")
	assert.False(t, st.QuotaBlockedUntil.IsZero(), "QuotaBlockedUntil is set")
	assert.Less(t, st.Backoff, 2*time.Hour, "backoff recorded is the floor, not the exponential max")

	// Recovery clears the quota state and resets the exponential backoff.
	f.err = nil
	f.snap = snapOf(&vehicle.State{Power: vehicle.PowerOnline}, nil)
	p.pollOnce(context.Background(), "scheduled")
	st = p.Stats()
	assert.False(t, st.TelemetryConfigWiped, "telemetry-wiped clears on recovery")
	assert.True(t, st.QuotaBlockedUntil.IsZero(), "QuotaBlockedUntil clears on recovery")
}

// TestQuotaBlockHonorsRetryAfter: a server Retry-After on a quota block (which
// can be hours) wins over the configurable floor.
func TestQuotaBlockHonorsRetryAfter(t *testing.T) {
	f := &fakeSource{err: &retryAfterQuotaError{msg: "account disabled", d: 6 * time.Hour}}
	p := New(f, "v1", Intervals{Default: 60 * time.Second}, nil)
	d := p.pollOnce(context.Background(), "scheduled")
	assert.InDelta(t, 6*time.Hour, d, float64(15*time.Second), "server Retry-After wins over the 1h floor")
}

type retryAfterQuotaError struct {
	msg string
	d   time.Duration
}

func (e *retryAfterQuotaError) Error() string                     { return e.msg }
func (e *retryAfterQuotaError) QuotaBlocked() bool                { return true }
func (e *retryAfterQuotaError) AccountDisabled() bool             { return true }
func (e *retryAfterQuotaError) TelemetryConfigWiped() bool        { return false }
func (e *retryAfterQuotaError) RetryAfter() (time.Duration, bool) { return e.d, true }
