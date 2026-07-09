package v1

import (
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/plugin/sink/idmap"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// identity carries the resolved Tesla-side identity for one served vehicle:
// the canonical (source-native) id plus the synthetic int64 ids Tesla needs
// (eid != vid), resolved once at construction via idmap.
type identity struct {
	canonID     string // source-native id (e.g. Rivian GUID)
	id          int64
	vehicleID   int64
	vin         string
	displayName string
}

// vehicleEntry is the per-vehicle served state: identity, config, the
// hold-last-known previous payload, and the last-encoded fast-path payload
// (all guarded by its own mutex).
type vehicleEntry struct {
	identity identity
	cfg      Cfg

	mu   sync.Mutex
	prev *wire.VehicleData

	// Fast path: the last full translation and the AsOf it was built from. A
	// repeat read at the same AsOf serves cachedAsOf's payload instead of
	// re-encoding all five sub-objects, so a consumer spinning on a frozen AsOf
	// (the 250 req/s stale-fetch storm) costs an AsOf compare, not a full encode.
	// Bounded: exactly one cached payload per vehicle.
	cached     *wire.VehicleData
	cachedAsOf time.Time
}

// sourceAdapter implements VehicleDataSource (+ Introspector) over the canonical
// sink.Provider. Every Products/Summary/VehicleData call reads the latest cached
// snapshot for the resolved vehicle (never the source directly), translates it
// canonical -> Tesla, and resolves the synthetic int64 id via idmap. A
// previously translated payload is retained per vehicle for hold-last-known.
type sourceAdapter struct {
	prov       sink.Provider
	staleAfter time.Duration
	now        func() time.Time // injectable clock for tests
	logger     *log.Logger

	byID  map[int64]*vehicleEntry  // Tesla id -> entry
	byVIN map[string]*vehicleEntry // VIN -> entry (Fleet vehicle_tag may be a VIN)
	order []int64                  // stable order for Products/Status

	// unchangedReads counts vehicle_data reads served from the fast path (AsOf
	// unchanged since the last translation): the spinning-consumer signal.
	unchangedReads atomic.Int64
}

// teslaWMIs are Tesla's World Manufacturer Identifiers (VIN chars 1-3): 5YJ Fremont,
// 7SA Fremont/Austin, 7G2 Austin/Nevada trucks (Cybertruck/Semi), LRW Shanghai, XP7
// Berlin, SFZ the original UK Roadster, 5XJ a rare secondary US code. Gating on these
// is what makes the char-4 line digit safe: a Rivian R1S VIN also carries 'S' at char
// 4, so without the WMI gate it would be mis-read as a Tesla Model S. Deliberately
// EXCLUDED: 3MW is BMW de Mexico's WMI (a speculative "Tesla Mexico" claim), and
// admitting it would let a BMW decode as a Tesla.
var teslaWMIs = map[string]bool{
	"5YJ": true, "5XJ": true, "7SA": true, "7G2": true,
	"LRW": true, "XP7": true, "SFZ": true,
}

// teslaLineCarType maps the VIN line digit (char 4) to the Tesla car_type. S/3/X/Y
// are the models TeslaMate renders directly; C/R/T/A (Cybertruck/Roadster/Semi/
// Cybercab) are not recognized upstream, but we still emit their real car_type so
// the sink can surface the model name as a trim badge (see vehicleConfig) instead
// of mislabeling them. Sources: Tesla NHTSA Part 565 VIN filings.
var teslaLineCarType = map[byte]string{
	'S': "models",
	'3': "model3",
	'X': "modelx",
	'Y': "modely",
	'R': "roadster",
	'C': "cybertruck",
	'T': "semi",
	'A': "cybercab",
}

// carTypeFromVIN derives the Tesla car_type from the VIN, for a Tesla WMI only. It is
// authoritative and restart-proof: the VIN never changes and needs no poll. Returns
// "" for a non-Tesla VIN or an unmapped Tesla line, so a wired resolver, the live
// vehicle_config, or the placeholder still applies.
func carTypeFromVIN(vin string) string {
	if len(vin) < 4 || !teslaWMIs[vin[:3]] {
		return ""
	}
	return teslaLineCarType[vin[3]]
}

// newSourceAdapter builds a sourceAdapter serving every vehicle the Provider
// reports. Each vehicle's id and a distinct vehicle_id are derived
// deterministically from its canonical id via idmap (eid != vid). staleAfter is
// the cache age past which a vehicle's top-level state degrades to "offline"
// (0 disables the guard). carType resolves the Tesla car_type placeholder per
// vehicle (vehicle_config must be non-null with a car_type or the FSM crashes).
func newSourceAdapter(prov sink.Provider, ids *idmap.Map, staleAfter time.Duration, carType func(vin string) string, logger *log.Logger) (*sourceAdapter, error) {
	if logger == nil {
		logger = log.Default()
	}
	s := &sourceAdapter{
		prov:       prov,
		staleAfter: staleAfter,
		now:        time.Now,
		logger:     logger,
		byID:       make(map[int64]*vehicleEntry),
		byVIN:      make(map[string]*vehicleEntry),
	}
	for _, v := range prov.Vehicles() {
		id, err := ids.ID(v.ID)
		if err != nil {
			return nil, err
		}
		vehicleID, err := ids.ID(v.ID + ":vid")
		if err != nil {
			return nil, err
		}
		// car_type must be non-null or TeslaMate's FSM crashes. Derive it from the
		// VIN (authoritative and restart-proof: the VIN never changes and needs no
		// poll), so a real Tesla shows its true model even with no vehicle_config and
		// no wired resolver. A wired resolver (Rivian) still wins; "model3" is only a
		// last resort for a genuinely unknown VIN.
		ct := carTypeFromVIN(v.VIN)
		if carType != nil {
			if c := carType(v.VIN); c != "" {
				ct = c
			}
		}
		if ct == "" {
			ct = "model3"
		}
		e := &vehicleEntry{
			identity: identity{
				canonID:     v.ID,
				id:          id,
				vehicleID:   vehicleID,
				vin:         v.VIN,
				displayName: v.DisplayName,
			},
			cfg: Cfg{CarType: ct, Model: v.Model},
		}
		s.byID[id] = e
		if v.VIN != "" {
			s.byVIN[v.VIN] = e
		}
		s.order = append(s.order, id)
	}
	return s, nil
}

// ResolveTag maps a Fleet API vehicle_tag (which may be the integer id OR the
// VIN, like the real Fleet API) to the served integer id. This lets the sink
// answer a source that addresses cars by VIN (our own tesla-fleet-poll-v1 source does)
// as well as one that uses the synthetic id (TeslaMate, from /api/1/products).
func (s *sourceAdapter) ResolveTag(tag string) (int64, bool) {
	if id, err := strconv.ParseInt(tag, 10, 64); err == nil {
		if _, ok := s.byID[id]; ok {
			return id, true
		}
	}
	if e, ok := s.byVIN[tag]; ok {
		return e.identity.id, true
	}
	return 0, false
}

// entry resolves a Tesla id to its served vehicle, or ErrVehicleNotFound.
func (s *sourceAdapter) entry(id int64) (*vehicleEntry, error) {
	if e, ok := s.byID[id]; ok {
		return e, nil
	}
	return nil, ErrVehicleNotFound
}

// isStale reports whether a cached snapshot is older than staleAfter. The age is
// measured from AsOf (the freshest of poll FetchedAt and stream StreamFields), so
// a live telemetry stream keeps a vehicle fresh even when polling has stopped --
// otherwise a dead poll would falsely degrade a vehicle that is still streaming.
// A zero AsOf (no data yet) is never stale.
func (s *sourceAdapter) isStale(asOf time.Time) bool {
	return s.staleAfter > 0 && !asOf.IsZero() && s.now().Sub(asOf) > s.staleAfter
}

// degradeIfStale degrades the top-level state when the cached snapshot is stale,
// preventing a source outage mid-drive from leaving TeslaMate logging a phantom
// infinite drive. It degrades to "offline" only when the last-known gear was a
// driving gear (so the drive-timeout guard fires); a settled car we have merely
// lost contact with degrades to "asleep", matching the cache's poll-side liveness
// rule (internal/cache classifyPollLiveness) so the two surfaces never disagree.
// Sub-objects (and their numeric timestamps) stay intact so the payload stays valid.
func (s *sourceAdapter) degradeIfStale(out wire.VehicleData, asOf time.Time) wire.VehicleData {
	if s.isStale(asOf) {
		out.State = staleLiveness(out)
	}
	return out
}

// staleLiveness picks the degraded state for a vehicle_data payload with no fresh
// data: "offline" when the last-known gear (held in drive_state.shift_state) was a
// driving gear, else "asleep". Reserving "offline" for a lost moving car is what
// keeps TeslaMate's graduated drive-timeout working while a settled car correctly
// reads asleep.
func staleLiveness(out wire.VehicleData) string {
	driving := out.DriveState != nil && out.DriveState.ShiftState != nil &&
		(*out.DriveState.ShiftState == "D" || *out.DriveState.ShiftState == "N" || *out.DriveState.ShiftState == "R")
	return lostState(driving)
}

// lostState maps "was the car driving when we lost it" to the degraded top-level
// state: a lost moving car is offline (drive-timeout guard); anything else is a
// settled car, which is asleep. Shared by the vehicle_data and lightweight surfaces
// so they never disagree.
func lostState(driving bool) string {
	if driving {
		return "offline"
	}
	return "asleep"
}

// drivingGear reports whether the canonical state's last-known gear is a moving
// gear, the canonical-side counterpart of staleLiveness's shift_state check.
func drivingGear(st *vehicle.State) bool {
	if st == nil {
		return false
	}
	switch st.Gear {
	case vehicle.GearDrive, vehicle.GearReverse, vehicle.GearNeutral:
		return true
	}
	return false
}

func (e *vehicleEntry) idsForTranslate() IDs {
	return IDs{
		ID:          e.identity.id,
		VehicleID:   e.identity.vehicleID,
		VIN:         e.identity.vin,
		DisplayName: e.identity.displayName,
	}
}

// translate produces the full payload for a vehicle from its latest snapshot,
// updating its hold-last-known prev. Returns the (possibly stale-degraded) data.
//
// Fast path: when the snapshot's AsOf has not advanced since the last
// translation, the encoded body cannot have changed, so reuse the cached
// payload instead of re-encoding all five sub-objects. A zero AsOf is never
// cached (un-merged snapshots stamp time.Now() per call, so they are never
// "unchanged"). degradeIfStale still runs on the cheap path: it keys on now(),
// not AsOf, so a frozen-AsOf car must still flip to offline once it goes stale.
func (s *sourceAdapter) translate(e *vehicleEntry) wire.VehicleData {
	snap := s.prov.Latest(e.identity.canonID)
	asOf := snap.AsOfTime()
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cached != nil && !asOf.IsZero() && asOf.Equal(e.cachedAsOf) {
		s.unchangedReads.Add(1)
		return s.degradeIfStale(*e.cached, asOf)
	}

	out := VehicleData(e.prev, snap.State, snap.Live, e.idsForTranslate(), e.cfg, asOf, s.logger)
	cp := out
	e.prev = &cp
	if !asOf.IsZero() {
		e.cached = &cp
		e.cachedAsOf = asOf
	}
	return s.degradeIfStale(out, asOf)
}

// currentState returns the top-level state for products/summary/status without
// running the full translation or advancing hold-last-known.
func (s *sourceAdapter) currentState(e *vehicleEntry) string {
	snap := s.prov.Latest(e.identity.canonID)
	if s.isStale(snap.AsOfTime()) {
		// Same rule as degradeIfStale: a lost moving car is offline, a settled one
		// asleep, so the lightweight surface agrees with vehicle_data.
		return lostState(drivingGear(snap.State))
	}
	return State(snap.State)
}

// Products returns one discovery entry per served vehicle.
func (s *sourceAdapter) Products() ([]wire.Product, error) {
	out := make([]wire.Product, 0, len(s.order))
	for _, id := range s.order {
		e := s.byID[id]
		out = append(out, wire.Product{
			ID:          e.identity.id,
			VehicleID:   e.identity.vehicleID,
			VIN:         e.identity.vin,
			DisplayName: e.identity.displayName,
			State:       s.currentState(e),
		})
	}
	return out, nil
}

// Summary returns the lightweight vehicle object for the cheap state check.
func (s *sourceAdapter) Summary(id int64) (wire.Summary, error) {
	e, err := s.entry(id)
	if err != nil {
		return wire.Summary{}, err
	}
	return wire.Summary{
		ID:          e.identity.id,
		VehicleID:   e.identity.vehicleID,
		VIN:         e.identity.vin,
		DisplayName: e.identity.displayName,
		State:       s.currentState(e),
	}, nil
}

// VehicleData translates the latest cached snapshot into a full payload.
func (s *sourceAdapter) VehicleData(id int64) (wire.VehicleData, error) {
	e, err := s.entry(id)
	if err != nil {
		return wire.VehicleData{}, err
	}
	return s.translate(e), nil
}

// --- Introspector (status/stats/extras) ------------------------------------

// Status returns one freshness/health view per served vehicle for /status.
func (s *sourceAdapter) Status() []VehicleStatus {
	out := make([]VehicleStatus, 0, len(s.order))
	for _, id := range s.order {
		e := s.byID[id]
		st := s.prov.Stats(e.identity.canonID)
		snap := s.prov.Latest(e.identity.canonID)

		// Freshness is measured from AsOf (the freshest of poll FetchedAt and stream
		// StreamFields), matching isStale's contract and currentState: a car kept live
		// by telemetry must not report Stale just because polling stopped. FetchedAt is
		// still surfaced below as the raw last-poll time.
		asOf := snap.AsOfTime()
		var age float64
		if !asOf.IsZero() {
			age = s.now().Sub(asOf).Seconds()
		}
		stale := s.isStale(asOf)

		out = append(out, VehicleStatus{
			ID:                  e.identity.id,
			VehicleID:           e.identity.vehicleID,
			VIN:                 e.identity.vin,
			DisplayName:         e.identity.displayName,
			State:               s.currentState(e),
			FetchedAt:           snap.FetchedAt,
			AgeSeconds:          age,
			Stale:               stale,
			LastError:           st.LastError,
			LastErrorAt:         st.LastErrorAt,
			BackoffSeconds:      st.Backoff.Seconds(),
			ConsecutiveFailures: st.ConsecutiveFailures,
			NeedsReauth:         st.NeedsReauth,
			PollSuccess:         st.SuccessCount,
			PollChanged:         st.ChangedCount,
			PollErrors:          st.ErrorCount,
			RateLimited:         st.RateLimitedCount,
			LastChangeAt:        st.LastChangeAt,
		})
	}
	return out
}

// Extras returns the raw canonical snapshot (the neutral State plus any live
// charging session) for the source-extras endpoint. Returns false for an
// unknown id.
func (s *sourceAdapter) Extras(id int64) (any, bool) {
	e, err := s.entry(id)
	if err != nil {
		return nil, false
	}
	snap := s.prov.Latest(e.identity.canonID)
	return map[string]any{
		"state":      snap.State,
		"live":       snap.Live,
		"fetched_at": snap.FetchedAt,
	}, true
}

// VehicleCount reports how many vehicles this source serves.
func (s *sourceAdapter) VehicleCount() int { return len(s.order) }

// UnchangedReads reports how many vehicle_data reads were served from the fast
// path (AsOf unchanged): the spinning-consumer signal exposed via /stats.
func (s *sourceAdapter) UnchangedReads() int64 { return s.unchangedReads.Load() }

// teslaID resolves a canonical (source-native) id to the synthetic Tesla int64
// id the handler routes on. An empty canonID returns the first served vehicle's
// id. It errors if there are no served vehicles or the canonID is unknown.
func (s *sourceAdapter) teslaID(canonID string) (int64, error) {
	if canonID == "" {
		if len(s.order) == 0 {
			return 0, ErrVehicleNotFound
		}
		return s.order[0], nil
	}
	for _, id := range s.order {
		if s.byID[id].identity.canonID == canonID {
			return id, nil
		}
	}
	return 0, ErrVehicleNotFound
}
