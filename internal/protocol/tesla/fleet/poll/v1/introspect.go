package v1

import (
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// VehicleStatus is the per-vehicle health view served by GET /status: cache
// freshness, the derived/translated Tesla state, and the poll loop's resilience
// counters. It is what `redswitchboard status` renders.
type VehicleStatus struct {
	ID                  int64     `json:"id"`
	VehicleID           int64     `json:"vehicle_id"`
	VIN                 string    `json:"vin"`
	DisplayName         string    `json:"display_name"`
	State               string    `json:"state"`
	FetchedAt           time.Time `json:"fetched_at"`
	AgeSeconds          float64   `json:"age_seconds"`
	Stale               bool      `json:"stale"`
	LastError           string    `json:"last_error,omitempty"`
	LastErrorAt         time.Time `json:"last_error_at,omitempty"`
	BackoffSeconds      float64   `json:"backoff_seconds"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NeedsReauth         bool      `json:"needs_reauth"`
	PollSuccess         int64     `json:"poll_success"`
	PollChanged         int64     `json:"poll_changed"` // polls whose content actually changed
	PollErrors          int64     `json:"poll_errors"`
	RateLimited         int64     `json:"rate_limited"`
	LastChangeAt        time.Time `json:"last_change_at,omitempty"`
}

// Introspector is the optional capability a data source exposes so the server
// can serve the /status, /stats, and /rivian_extras endpoints. A source that
// does not implement it (e.g. a static test source) simply has those endpoints
// left unregistered, so the core Tesla surface is unaffected.
type Introspector interface {
	// Status returns one entry per known vehicle.
	Status() []VehicleStatus
	// Extras returns the raw Rivian-only snapshot for a vehicle (nil/false if the
	// id is unknown). Marshaled verbatim by /rivian_extras.
	Extras(id int64) (any, bool)
	// VehicleCount is the number of vehicles known to the source.
	VehicleCount() int
	// UnchangedReads is the count of vehicle_data reads served from the fast path
	// (AsOf unchanged): the spinning-consumer signal.
	UnchangedReads() int64
}

// requestCounters tallies served requests per route pattern, for /stats.
type requestCounters struct {
	mu sync.Mutex
	m  map[string]*int64
}

func newRequestCounters() *requestCounters { return &requestCounters{m: map[string]*int64{}} }

func (c *requestCounters) inc(pattern string) {
	if pattern == "" {
		return
	}
	c.mu.Lock()
	p, ok := c.m[pattern]
	if !ok {
		p = new(int64)
		c.m[pattern] = p
	}
	c.mu.Unlock()
	atomic.AddInt64(p, 1)
}

func (c *requestCounters) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = atomic.LoadInt64(v)
	}
	return out
}

// handleStatus serves GET /status: the per-vehicle freshness/health view.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"vehicles": s.intro.Status()})
}

// handleExtras serves GET /api/1/vehicles/{id}/rivian_extras: the raw Rivian
// snapshot (all Rivian-only data) that has no Tesla Fleet API equivalent.
func (s *Server) handleExtras(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	extras, ok := s.intro.Extras(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"response": extras})
}

// handleStats serves GET /stats: process metrics (uptime, goroutines, heap),
// per-route request counts, vehicles known, and aggregate poller counters. This
// is the headline rate-decoupling view (cache reads vs actual Rivian polls).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	var pollSuccess, pollChanged, pollErrors, rateLimited int64
	for _, v := range s.intro.Status() {
		pollSuccess += v.PollSuccess
		pollChanged += v.PollChanged
		pollErrors += v.PollErrors
		rateLimited += v.RateLimited
	}

	counts := s.counters.snapshot()
	reads := counts["/api/1/vehicles/{id}/vehicle_data"]
	unchangedReads := s.intro.UnchangedReads()
	uptimeMin := time.Since(s.startedAt).Minutes()

	// The rate-decoupling story in numbers:
	//   reads_per_poll   = consumer reads served per actual Rivian poll (cache wins)
	//   reads_per_change = consumer reads per actual DATA change (the real headline)
	//   change_ratio     = fraction of Rivian polls that returned new data
	rate := func(n int64) float64 {
		if uptimeMin <= 0 {
			return 0
		}
		return float64(n) / uptimeMin
	}
	div := func(a, b int64) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds":  time.Since(s.startedAt).Seconds(),
		"vehicles_known":  s.intro.VehicleCount(),
		"requests":        counts,
		"rivian_polls":    pollSuccess,
		"rivian_changes":  pollChanged,
		"rivian_errors":   pollErrors,
		"rate_limited":    rateLimited,
		"reads":           reads,
		"unchanged_reads": unchangedReads, // reads served from the AsOf fast path (P7)
		"rates_per_min": map[string]any{
			"consumer_reads": rate(reads),
			"rivian_polls":   rate(pollSuccess),
			"data_changes":   rate(pollChanged),
		},
		"reads_per_poll":   div(reads, pollSuccess),
		"reads_per_change": div(reads, pollChanged),
		"change_ratio":     div(pollChanged, pollSuccess),
		"cache_hit_ratio":  div(reads, pollSuccess),
		"resources": map[string]any{
			"goroutines": runtime.NumGoroutine(),
			"heap_alloc": mem.HeapAlloc,
			"sys":        mem.Sys,
			"num_gc":     mem.NumGC,
			"go_version": runtime.Version(),
			"num_cpu":    runtime.NumCPU(),
		},
	})
}
