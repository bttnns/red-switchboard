// Package metrics adds a protocol-agnostic Prometheus /metrics surface to the
// serve pipeline, built on the official Prometheus Go client
// (github.com/prometheus/client_golang). It is deliberately decoupled from any
// sink or source plugin:
//
//   - The inbound SINK is observed with an HTTP middleware that records, per
//     (method, normalized route, status), a request counter and a duration
//     histogram. Because the serve level does not have the matched route pattern
//     (that lives inside each protocol's router), the path is NORMALIZED here:
//     numeric / synthetic-id segments collapse to "{id}", so Tesla's per-vehicle
//     REST paths and Rivian's single GraphQL path both yield stable label sets.
//   - The outbound SOURCE is observed by READING the poll loop's existing
//     per-vehicle counters at scrape time, not by counting anything here. serve
//     supplies a snapshot function (func() []VehicleMetric); a custom Collector
//     turns those into const metrics on each scrape. This avoids duplicating the
//     counting the poll Manager already does.
//
// The HTTP counter/histogram use the library's Vec types directly; the source
// and streaming views are exported as const metrics read from snapshot funcs so
// the poll loop and the streaming components stay unaware of Prometheus.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// VehicleMetric is one vehicle's source-side poll health, as the renderer needs
// it. serve builds these from the live poll stats (provider.Stats(id)) so the
// counting stays in one place (the poll loop).
type VehicleMetric struct {
	VIN                  string
	SuccessCount         int64
	ErrorCount           int64
	RateLimitedCount     int64
	ChangedCount         int64
	Backoff              time.Duration
	ConsecutiveFailures  int
	NeedsReauth          bool
	QuotaBlockedCount    int64
	QuotaBlockedUntil    time.Time
	TelemetryConfigWiped bool

	VehicleDataFetches  int64            // online polls that fetched (billed) vehicle_data
	PollsByState        map[string]int64 // successful polls per derived state
	ScheduledInterval   time.Duration    // current scheduled poll interval
	StreamBackoffActive bool             // drive poll currently backed off on a fresh stream
	DerivedState        string           // current derived state (for the state info gauge)
	LastPollAt          time.Time        // time of the last successful poll
}

// StreamSinkStats is the streaming-sink view (per-sink, not per-vehicle): how
// many consumers are connected and how many frames have been pushed/dropped.
type StreamSinkStats struct {
	Consumers     int64
	FramesPushed  int64
	FramesDropped int64
}

// StreamSourceStats is the streaming-source view: connected sessions, total
// connections (reconnect storms), frames decoded, the age of the last frame, the
// inter-frame gap histogram, and per-streamed-field frame counts.
type StreamSourceStats struct {
	Connected    int64
	Connects     int64
	Frames       int64
	LastFrameAge time.Duration

	GapBuckets  map[float64]uint64
	GapSum      float64
	GapCount    uint64
	FieldFrames map[string]int64
	Rejects     map[string]int64 // connections/frames denied at the trust boundary, by reason

	Disconnects  int64 // total sessions closed (with Connects, the connection churn)
	IdleTimeouts int64 // sessions reaped for sending no frame within idle_timeout
}

// CommandStats is the command-path view: commands sent, and per-outcome
// counters. Wakes are counted separately from other commands because they are
// the expensive billing category ($20/1k vs $1/1k), the way the plan calls out.
type CommandStats struct {
	Sent             int64
	Successes        int64
	NominalFailures  int64
	InfraErrors      int64
	Wakes            int64
	RateLimitedCount int64
}

// Registry holds the Prometheus registry plus the live snapshot functions the
// custom collectors read at scrape time. It is safe for concurrent use.
type Registry struct {
	prom *prometheus.Registry

	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	vehiclesKnown prometheus.Gauge

	mu      sync.RWMutex
	started time.Time

	// snapshot holders, updated by SetSource/SetStreamSink/SetStreamSource and
	// read by the registered collectors on each scrape.
	sourceHold      sourceHolder
	streamSinkHold  streamSinkHolder
	streamSrcHold   streamSourceHolder
	streamIntegHold streamIntegrityHolder
	commandHold     commandHolder
	sessionHold     sessionHolder
	unchangedHold   unchangedHolder
	costHold        costHolder
}

// sourceHolder carries the active source name + its poll-stats snapshot func.
type sourceHolder struct {
	name string
	fn   func() []VehicleMetric
}

type streamSinkHolder struct {
	fn func() StreamSinkStats
}

type streamSourceHolder struct {
	fn func() StreamSourceStats
}

// streamIntegrityHolder carries the cache's per-reason stream-integrity rejection
// snapshot func (reason -> total), read at scrape time.
type streamIntegrityHolder struct {
	fn func() map[string]int64
}

type commandHolder struct {
	name string
	fn   func() CommandStats
}

// sessionHolder carries the cache's drive/charge opened/closed snapshot func
// ("<kind>_<edge>" -> total), read at scrape time.
type sessionHolder struct {
	fn func() map[string]int64
}

// costHolder carries the active source name, the per-call price of a billed
// vehicle_data fetch (USD), and the source poll-stats snapshot func it sums the
// fetch count from. It reuses the source snapshot rather than wiring a second
// counter, so the cost stays a pure derivation (paid fetches x price) with no
// new counting.
type costHolder struct {
	name  string
	price float64
	fn    func() []VehicleMetric
}

// unchangedHolder carries the active source name + the process-wide unchanged-reads
// snapshot func (the P7 spinning-consumer signal: vehicle_data reads served from the
// AsOf fast path). The counter is process-wide on the sink, so it carries no vin.
type unchangedHolder struct {
	name string
	fn   func() int64
}

// New constructs a Registry with the standard Go + process collectors and the
// app's HTTP/source/streaming metric instruments registered.
func New() *Registry {
	reg := &Registry{
		prom:    prometheus.NewRegistry(),
		started: time.Now(),
	}

	reg.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "redswitchboard_http_requests_total",
		Help: "Total HTTP requests served, by method, normalized route, and status.",
	}, []string{"method", "route", "status"})

	reg.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "redswitchboard_http_request_duration_seconds",
		Help:    "HTTP request handling time in seconds, by method and normalized route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	reg.vehiclesKnown = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "redswitchboard_vehicles_known",
		Help: "Number of vehicles the hub is serving.",
	})

	uptime := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "redswitchboard_uptime_seconds",
		Help: "Seconds since the serve process started.",
	}, func() float64 { return time.Since(reg.started).Seconds() })

	reg.prom.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		reg.httpRequests,
		reg.httpDuration,
		reg.vehiclesKnown,
		uptime,
		newSourceCollector(&reg.sourceHold),
		newStreamSinkCollector(&reg.streamSinkHold),
		newStreamSourceCollector(&reg.streamSrcHold),
		newStreamIntegrityCollector(&reg.streamIntegHold),
		newCommandCollector(&reg.commandHold),
		newSessionCollector(&reg.sessionHold),
		newUnchangedCollector(&reg.unchangedHold),
		newCostCollector(&reg.costHold),
	)
	return reg
}

// SetSource wires the source-side view: name is the active source plugin, snap
// returns the current per-vehicle poll stats. Pass a nil snap to clear it.
func (reg *Registry) SetSource(name string, snap func() []VehicleMetric) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sourceHold.name = name
	reg.sourceHold.fn = snap
}

// SetStreamSink wires the streaming-sink gauges. Pass a nil snap to clear it.
func (reg *Registry) SetStreamSink(snap func() StreamSinkStats) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.streamSinkHold.fn = snap
}

// SetStreamSource wires the streaming-source gauges. Pass a nil snap to clear it.
func (reg *Registry) SetStreamSource(snap func() StreamSourceStats) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.streamSrcHold.fn = snap
}

// SetStreamIntegrity wires the cache's stream-integrity rejection counters
// (reason -> total). Pass a nil snap to clear it.
func (reg *Registry) SetStreamIntegrity(snap func() map[string]int64) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.streamIntegHold.fn = snap
}

// SetCommand wires the command-path counters. name is the active commander plugin
// (e.g. "tesla-command-v1"); snap returns the current counters. Pass a nil snap
// to clear it (commands disabled).
func (reg *Registry) SetCommand(name string, snap func() CommandStats) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.commandHold.name = name
	reg.commandHold.fn = snap
}

// SetSession wires the cache's drive/charge opened/closed counters ("<kind>_<edge>"
// -> total). Pass a nil snap to clear it.
func (reg *Registry) SetSession(snap func() map[string]int64) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sessionHold.fn = snap
}

// SetSourceUnchanged wires the P7 unchanged-reads counter: name is the active
// source plugin, snap returns the process-wide count of vehicle_data reads served
// from the AsOf fast path (the spinning-consumer signal). Pass a nil snap to clear.
func (reg *Registry) SetSourceUnchanged(name string, snap func() int64) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.unchangedHold.name = name
	reg.unchangedHold.fn = snap
}

// SetSourceCost wires the cost estimate: name is the active source plugin, price
// is the per-call USD price of a billed vehicle_data fetch, and snap returns the
// per-vehicle poll stats (the same snapshot SetSource uses) whose VehicleDataFetches
// are summed. A zero or negative price, or a nil snap, clears it.
func (reg *Registry) SetSourceCost(name string, price float64, snap func() []VehicleMetric) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.costHold.name = name
	reg.costHold.price = price
	reg.costHold.fn = snap
}

// Middleware records each request's method, normalized route, status, and
// duration, then delegates to next.
func (reg *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		reg.observe(r.Method, NormalizePath(r.URL.Path), rec.status, time.Since(start))
	})
}

func (reg *Registry) observe(method, route string, status int, d time.Duration) {
	reg.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	reg.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// statusRecorder captures the response status code for the middleware.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer if it supports flushing, so streaming
// sink handlers keep working behind the middleware.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Handler returns the Prometheus exposition handler. vehiclesKnown is set as a
// static gauge (serve knows the identity count; the per-vehicle source stats
// come from the SetSource snapshot, read fresh on each scrape).
func (reg *Registry) Handler(vehiclesKnown int) http.Handler {
	reg.vehiclesKnown.Set(float64(vehiclesKnown))
	return promhttp.HandlerFor(reg.prom, promhttp.HandlerOpts{})
}
