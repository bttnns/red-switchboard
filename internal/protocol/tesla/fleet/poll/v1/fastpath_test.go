package v1

import (
	"sync"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink/idmap"
	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mutableProvider serves one vehicle whose snapshot the test mutates between
// reads, so the fast path can be driven across unchanged and advancing AsOf.
type mutableProvider struct {
	id  string
	vin string

	mu   sync.Mutex
	snap vehicle.Snapshot
}

func (p *mutableProvider) Vehicles() []vehicle.Identity {
	return []vehicle.Identity{{ID: p.id, VIN: p.vin, DisplayName: "Truck", Model: "R1T"}}
}

func (p *mutableProvider) Latest(string) vehicle.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap
}

func (p *mutableProvider) Stats(string) poll.Stats { return poll.Stats{} }

func (p *mutableProvider) set(s vehicle.Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snap = s
}

func newFastPathAdapter(t *testing.T, prov *mutableProvider) (*sourceAdapter, int64) {
	t.Helper()
	ids, err := idmap.New("")
	require.NoError(t, err)
	s, err := newSourceAdapter(prov, ids, 30*time.Minute, func(string) string { return "model3" }, nil)
	require.NoError(t, err)
	id, err := s.teslaID(prov.id)
	require.NoError(t, err)
	return s, id
}

func snapAt(asOf time.Time, level float64) vehicle.Snapshot {
	return vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, BatteryLevelPct: level},
		FetchedAt: asOf,
		AsOf:      asOf,
	}
}

// TestFastPathUnchangedAsOf: repeated reads at an unchanged AsOf are served from
// the cache (the spinning-consumer signal increments) and return the identical
// body a fresh translation would, while an advancing AsOf re-encodes and does NOT
// count as unchanged. This is the P7 anti-spin guarantee.
func TestFastPathUnchangedAsOf(t *testing.T) {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	prov := &mutableProvider{id: "guid-a", vin: "VIN_A"}
	prov.set(snapAt(base, 50))
	s, id := newFastPathAdapter(t, prov)
	s.now = func() time.Time { return base } // fresh: never stale-degraded

	first, err := s.VehicleData(id)
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.UnchangedReads(), "the first read populates the cache, it is not a fast-path hit")
	require.NotNil(t, first.ChargeState)
	require.NotNil(t, first.ChargeState.BatteryLevel)
	assert.Equal(t, 50, *first.ChargeState.BatteryLevel)

	// Spin at the SAME AsOf: every repeat is a fast-path hit and byte-identical.
	const spin = 250
	for i := 0; i < spin; i++ {
		got, err := s.VehicleData(id)
		require.NoError(t, err)
		assert.Equal(t, first, got, "an unchanged AsOf must serve the identical body")
	}
	assert.Equal(t, int64(spin), s.UnchangedReads(), "every repeat read at unchanged AsOf is a fast-path hit")

	// Advance AsOf with new data: a fresh translation, not a fast-path hit.
	prov.set(snapAt(base.Add(time.Minute), 60))
	fresh, err := s.VehicleData(id)
	require.NoError(t, err)
	require.NotNil(t, fresh.ChargeState.BatteryLevel)
	assert.Equal(t, 60, *fresh.ChargeState.BatteryLevel, "an advancing AsOf must produce a fresh response")
	assert.Equal(t, int64(spin), s.UnchangedReads(), "advancing AsOf must not count as a fast-path hit")
}

// TestFastPathZeroAsOfNeverCached: an un-merged snapshot (zero AsOf) stamps a
// fresh time per call, so it must never be served from the fast path, otherwise a
// pre-merge vehicle would freeze on its first body.
func TestFastPathZeroAsOfNeverCached(t *testing.T) {
	prov := &mutableProvider{id: "guid-a", vin: "VIN_A"}
	prov.set(vehicle.Snapshot{State: &vehicle.State{Power: vehicle.PowerOnline}}) // zero AsOf
	s, id := newFastPathAdapter(t, prov)

	for i := 0; i < 5; i++ {
		_, err := s.VehicleData(id)
		require.NoError(t, err)
	}
	assert.Equal(t, int64(0), s.UnchangedReads(), "a zero AsOf must never be served from the fast path")
}

// TestFastPathStillDegradesStale: the fast path keys on AsOf, but staleness keys
// on now(), so a car spinning on a frozen AsOf must still degrade once it ages past
// staleAfter (the phantom-drive guard must survive the fast path). A car last seen
// DRIVING degrades to offline so TeslaMate's drive-timeout fires.
func TestFastPathStillDegradesStale(t *testing.T) {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	wall := base
	prov := &mutableProvider{id: "guid-a", vin: "VIN_A"}
	prov.set(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, BatteryLevelPct: 50, Gear: vehicle.GearDrive},
		FetchedAt: base,
		AsOf:      base,
	})
	s, id := newFastPathAdapter(t, prov)
	s.now = func() time.Time { return wall }

	fresh, err := s.VehicleData(id)
	require.NoError(t, err)
	assert.Equal(t, "online", fresh.State)

	// Same AsOf, but wall clock advances past staleAfter: a fast-path hit that
	// must still degrade. A driving car -> offline (drive-timeout guard).
	wall = base.Add(45 * time.Minute)
	stale, err := s.VehicleData(id)
	require.NoError(t, err)
	assert.Equal(t, int64(1), s.UnchangedReads(), "the stale read still went through the fast path")
	assert.Equal(t, "offline", stale.State, "a stale driving car must degrade to offline")
	require.NotNil(t, stale.ChargeState, "sub-objects must survive the degrade")
}

// TestStaleDegradeSettledCarAsleep: a settled (parked/charging) car that goes stale
// degrades to asleep, not offline -- "offline" is reserved for a car lost while
// driving. Presenting asleep is what lets TeslaMate's :charging FSM complete the
// charge instead of looping "Vehicle went offline while charging" forever.
func TestStaleDegradeSettledCarAsleep(t *testing.T) {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	wall := base
	prov := &mutableProvider{id: "guid-a", vin: "VIN_A"}
	prov.set(snapAt(base, 50)) // parked: no gear
	s, id := newFastPathAdapter(t, prov)
	s.now = func() time.Time { return wall }

	wall = base.Add(45 * time.Minute)
	stale, err := s.VehicleData(id)
	require.NoError(t, err)
	assert.Equal(t, "asleep", stale.State, "a stale settled car degrades to asleep, not offline")
}

// TestSummaryStaleAgreesWithVehicleData: the lightweight Summary/currentState
// surface must degrade by the same rule as vehicle_data, so TeslaMate sees one
// liveness whichever endpoint it polls -- driving ⇒ offline, settled ⇒ asleep.
func TestSummaryStaleAgreesWithVehicleData(t *testing.T) {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	wall := base.Add(45 * time.Minute)

	driving := &mutableProvider{id: "guid-d", vin: "VIN_D"}
	driving.set(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive},
		FetchedAt: base, AsOf: base,
	})
	sd, idd := newFastPathAdapter(t, driving)
	sd.now = func() time.Time { return wall }
	sum, err := sd.Summary(idd)
	require.NoError(t, err)
	assert.Equal(t, "offline", sum.State, "stale driving car ⇒ offline on the lightweight surface")

	parked := &mutableProvider{id: "guid-p", vin: "VIN_P"}
	parked.set(vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark},
		FetchedAt: base, AsOf: base,
	})
	sp, idp := newFastPathAdapter(t, parked)
	sp.now = func() time.Time { return wall }
	sum, err = sp.Summary(idp)
	require.NoError(t, err)
	assert.Equal(t, "asleep", sum.State, "stale settled car ⇒ asleep on the lightweight surface")
}

// TestFastPathConcurrentRace hammers one vehicle from many goroutines at a fixed
// AsOf to prove the fast path is race-clean (run under -race).
func TestFastPathConcurrentRace(t *testing.T) {
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	prov := &mutableProvider{id: "guid-a", vin: "VIN_A"}
	prov.set(snapAt(base, 50))
	s, id := newFastPathAdapter(t, prov)
	s.now = func() time.Time { return base }

	// Prime the cache once.
	_, err := s.VehicleData(id)
	require.NoError(t, err)

	const workers, perWorker = 16, 64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, err := s.VehicleData(id)
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(workers*perWorker), s.UnchangedReads(), "every concurrent repeat read is a fast-path hit")
}
