package metrics

// render.go holds the custom prometheus.Collectors that turn the live snapshot
// functions (poll stats, streaming sink/source state) into const metrics at
// scrape time, plus a text-exposition helper used by tests. Production serves
// /metrics through promhttp.HandlerFor (see metrics.go); the const-metric
// collectors are what keep the counting in the poll loop / streaming components
// and out of Prometheus, mirroring an exporter.

import (
	"bytes"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// sourceCollector emits the per-vehicle source poll gauges/counters as const
// metrics read from the snapshot func on each scrape. Counting stays in the poll
// loop; this only reads. When the snapshot func is nil (no source wired), it
// emits nothing.
type sourceCollector struct {
	hold *sourceHolder
}

func newSourceCollector(h *sourceHolder) *sourceCollector {
	return &sourceCollector{hold: h}
}

// sourceMetricDef describes one emitted source metric.
type sourceMetricDef struct {
	name      string
	help      string
	valueType prometheus.ValueType
	extract   func(v VehicleMetric) float64
}

var sourceMetricDefs = []sourceMetricDef{
	{"redswitchboard_source_polls_total", "Successful source polls, by source and vehicle.", prometheus.CounterValue, func(v VehicleMetric) float64 { return float64(v.SuccessCount) }},
	{"redswitchboard_source_poll_errors_total", "Failed source polls, by source and vehicle.", prometheus.CounterValue, func(v VehicleMetric) float64 { return float64(v.ErrorCount) }},
	{"redswitchboard_source_poll_changes_total", "Source polls whose cached content actually changed, by source and vehicle.", prometheus.CounterValue, func(v VehicleMetric) float64 { return float64(v.ChangedCount) }},
	{"redswitchboard_source_rate_limited_total", "Source polls rejected with a rate limit, by source and vehicle.", prometheus.CounterValue, func(v VehicleMetric) float64 { return float64(v.RateLimitedCount) }},
	{"redswitchboard_source_poll_backoff_seconds", "Current poll backoff delay in seconds (0 when healthy), by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return v.Backoff.Seconds() }},
	{"redswitchboard_source_consecutive_failures", "Consecutive poll failures since the last success, by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return float64(v.ConsecutiveFailures) }},
	{"redswitchboard_source_needs_reauth", "1 when the source creds appear dead and need re-auth, else 0, by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return boolGauge(v.NeedsReauth) }},
	{"redswitchboard_source_quota_blocked_total", "Source polls rejected with a billing/quota block (403 account disabled), by source and vehicle.", prometheus.CounterValue, func(v VehicleMetric) float64 { return float64(v.QuotaBlockedCount) }},
	{"redswitchboard_source_quota_blocked", "1 when the source is currently in a quota-block backoff, else 0, by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 {
		return boolGauge(!v.QuotaBlockedUntil.IsZero() && time.Now().Before(v.QuotaBlockedUntil))
	}},
	{"redswitchboard_source_telemetry_config_wiped", "1 when the last source error wiped the vendor push-telemetry config (needs re-pair), else 0, by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return boolGauge(v.TelemetryConfigWiped) }},
	{"redswitchboard_source_vehicle_data_fetches_total", "Online polls that fetched (billed) vehicle_data, by source and vehicle. This is the Tesla cost driver; free summary polls are excluded.", prometheus.CounterValue, func(v VehicleMetric) float64 { return float64(v.VehicleDataFetches) }},
	{"redswitchboard_source_scheduled_interval_seconds", "Current scheduled (pre-jitter) poll interval in seconds, by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return v.ScheduledInterval.Seconds() }},
	{"redswitchboard_source_stream_backoff_active", "1 when the drive poll is currently backed off because a telemetry stream is fresh, else 0, by source and vehicle.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return boolGauge(v.StreamBackoffActive) }},
	{"redswitchboard_source_last_poll_timestamp_seconds", "Unix time of the last successful source poll (0 if none yet), by source and vehicle. With scheduled_interval_seconds this gives the next-poll ETA.", prometheus.GaugeValue, func(v VehicleMetric) float64 { return unixOrZero(v.LastPollAt) }},
}

// sourceStateDesc is the current-derived-state info gauge: it emits a single
// series (value 1) labeled with the live state, so "what is the car doing right
// now" is a direct query. Old state series go stale on the next scrape.
var sourceStateDesc = prometheus.NewDesc(
	"redswitchboard_source_state",
	"Current derived vehicle state (value always 1), by source, vehicle, and state.",
	[]string{"source", "vin", "state"}, nil,
)

// sourceStatePollsDesc is the per-derived-state poll counter (extra "state"
// label), emitted separately from sourceMetricDefs because of its extra label.
var sourceStatePollsDesc = prometheus.NewDesc(
	"redswitchboard_source_state_polls_total",
	"Successful source polls per derived vehicle state, by source, vehicle, and state (asleep/online/driving/charging_ac/charging_dc/offline).",
	[]string{"source", "vin", "state"}, nil,
)

// sourceDescs is the source metric descriptors, built once (immutable metadata)
// and reused by every Describe/Collect rather than rebuilt per scrape.
var sourceDescs = func() []*prometheus.Desc {
	descs := make([]*prometheus.Desc, len(sourceMetricDefs))
	for i, d := range sourceMetricDefs {
		descs[i] = prometheus.NewDesc(d.name, d.help, []string{"source", "vin"}, nil)
	}
	return descs
}()

// Describe sends the descriptors for every source metric so the registry knows
// the family metadata even before the first scrape (and when no source is set).
func (c *sourceCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range sourceDescs {
		ch <- d
	}
	ch <- sourceStatePollsDesc
	ch <- sourceStateDesc
}

func (c *sourceCollector) Collect(ch chan<- prometheus.Metric) {
	hold := *c.hold
	if hold.fn == nil {
		return
	}
	for _, v := range hold.fn() {
		for i, d := range sourceMetricDefs {
			ch <- prometheus.MustNewConstMetric(sourceDescs[i], d.valueType, d.extract(v), hold.name, v.VIN)
		}
		for state, n := range v.PollsByState {
			ch <- prometheus.MustNewConstMetric(sourceStatePollsDesc, prometheus.CounterValue, float64(n), hold.name, v.VIN, state)
		}
		if v.DerivedState != "" {
			ch <- prometheus.MustNewConstMetric(sourceStateDesc, prometheus.GaugeValue, 1, hold.name, v.VIN, v.DerivedState)
		}
	}
}

// streamSinkCollector emits the per-sink streaming gauges/counters as const
// metrics read from the snapshot func on each scrape.
type streamSinkCollector struct {
	hold *streamSinkHolder
}

func newStreamSinkCollector(h *streamSinkHolder) *streamSinkCollector {
	return &streamSinkCollector{hold: h}
}

// streamSinkDescs is built once and reused by Describe/Collect.
var streamSinkDescs = []*prometheus.Desc{
	prometheus.NewDesc("redswitchboard_stream_sink_consumers", "Number of currently connected streaming-sink consumers.", nil, nil),
	prometheus.NewDesc("redswitchboard_stream_sink_frames_pushed_total", "Total data:update frames pushed to consumers.", nil, nil),
	prometheus.NewDesc("redswitchboard_stream_sink_frames_dropped_total", "Total frames dropped (slow consumer / send buffer full).", nil, nil),
}

func (c *streamSinkCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range streamSinkDescs {
		ch <- d
	}
}

func (c *streamSinkCollector) Collect(ch chan<- prometheus.Metric) {
	if c.hold.fn == nil {
		return
	}
	s := c.hold.fn()
	ch <- prometheus.MustNewConstMetric(streamSinkDescs[0], prometheus.GaugeValue, float64(s.Consumers))
	ch <- prometheus.MustNewConstMetric(streamSinkDescs[1], prometheus.CounterValue, float64(s.FramesPushed))
	ch <- prometheus.MustNewConstMetric(streamSinkDescs[2], prometheus.CounterValue, float64(s.FramesDropped))
}

// streamSourceCollector emits the streaming-source gauges/counters as const
// metrics read from the snapshot func on each scrape.
type streamSourceCollector struct {
	hold *streamSourceHolder
}

func newStreamSourceCollector(h *streamSourceHolder) *streamSourceCollector {
	return &streamSourceCollector{hold: h}
}

// streamSourceDescs is built once and reused by Describe/Collect.
var (
	streamSrcConnectedDesc = prometheus.NewDesc("redswitchboard_stream_source_connected", "Currently connected streaming-source sessions (e.g. Fleet Telemetry vehicles).", nil, nil)
	streamSrcConnectsDesc  = prometheus.NewDesc("redswitchboard_stream_source_connects_total", "Total streaming-source connections opened (a reconnect storm inflates this).", nil, nil)
	streamSrcFramesDesc    = prometheus.NewDesc("redswitchboard_stream_source_frames_total", "Total frames decoded and accepted from the streaming source.", nil, nil)
	streamSrcLastAgeDesc   = prometheus.NewDesc("redswitchboard_stream_source_last_frame_age_seconds", "Seconds since the last decoded frame.", nil, nil)
	streamSrcGapDesc       = prometheus.NewDesc("redswitchboard_stream_source_frame_gap_seconds", "Histogram of the gap between consecutive decoded stream frames.", nil, nil)
	streamSrcFieldDesc     = prometheus.NewDesc("redswitchboard_stream_source_field_frames_total", "Frames carrying each streamed canonical field, by field.", []string{"field"}, nil)
	streamSrcRejectsDesc   = prometheus.NewDesc("redswitchboard_stream_source_rejects_total", "Connections/frames denied at the streaming-source trust boundary, by reason (identity/unknown_vin).", []string{"reason"}, nil)
	streamSrcDisconnDesc   = prometheus.NewDesc("redswitchboard_stream_source_disconnects_total", "Total streaming-source sessions closed (with connects_total, the connection churn rate).", nil, nil)
	streamSrcIdleDesc      = prometheus.NewDesc("redswitchboard_stream_source_idle_timeouts_total", "Streaming-source sessions reaped for sending no frame within idle_timeout (a stalled/half-open stream).", nil, nil)
)

func (c *streamSourceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- streamSrcConnectedDesc
	ch <- streamSrcConnectsDesc
	ch <- streamSrcFramesDesc
	ch <- streamSrcLastAgeDesc
	ch <- streamSrcGapDesc
	ch <- streamSrcFieldDesc
	ch <- streamSrcRejectsDesc
	ch <- streamSrcDisconnDesc
	ch <- streamSrcIdleDesc
}

func (c *streamSourceCollector) Collect(ch chan<- prometheus.Metric) {
	if c.hold.fn == nil {
		return
	}
	s := c.hold.fn()
	ch <- prometheus.MustNewConstMetric(streamSrcConnectedDesc, prometheus.GaugeValue, float64(s.Connected))
	ch <- prometheus.MustNewConstMetric(streamSrcConnectsDesc, prometheus.CounterValue, float64(s.Connects))
	ch <- prometheus.MustNewConstMetric(streamSrcFramesDesc, prometheus.CounterValue, float64(s.Frames))
	ch <- prometheus.MustNewConstMetric(streamSrcLastAgeDesc, prometheus.GaugeValue, s.LastFrameAge.Seconds())
	if s.GapCount > 0 {
		ch <- prometheus.MustNewConstHistogram(streamSrcGapDesc, s.GapCount, s.GapSum, s.GapBuckets)
	}
	for field, n := range s.FieldFrames {
		ch <- prometheus.MustNewConstMetric(streamSrcFieldDesc, prometheus.CounterValue, float64(n), field)
	}
	for reason, n := range s.Rejects {
		ch <- prometheus.MustNewConstMetric(streamSrcRejectsDesc, prometheus.CounterValue, float64(n), reason)
	}
	ch <- prometheus.MustNewConstMetric(streamSrcDisconnDesc, prometheus.CounterValue, float64(s.Disconnects))
	ch <- prometheus.MustNewConstMetric(streamSrcIdleDesc, prometheus.CounterValue, float64(s.IdleTimeouts))
}

// streamIntegrityCollector emits the cache's stream-integrity rejection counter,
// labeled by reason, read from the snapshot func on each scrape. Counting stays
// in the cache (the merge gate); this only reads, mirroring the other collectors.
type streamIntegrityCollector struct {
	hold *streamIntegrityHolder
}

func newStreamIntegrityCollector(h *streamIntegrityHolder) *streamIntegrityCollector {
	return &streamIntegrityCollector{hold: h}
}

var streamIntegrityDesc = prometheus.NewDesc(
	"redswitchboard_stream_integrity_rejections_total",
	"Streamed numeric fields rejected by the merge-side integrity gate (held last-known), by reason (odometer_regress/gps_teleport/soc_range/speed_range).",
	[]string{"reason"}, nil,
)

func (c *streamIntegrityCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- streamIntegrityDesc
}

func (c *streamIntegrityCollector) Collect(ch chan<- prometheus.Metric) {
	if c.hold.fn == nil {
		return
	}
	for reason, n := range c.hold.fn() {
		ch <- prometheus.MustNewConstMetric(streamIntegrityDesc, prometheus.CounterValue, float64(n), reason)
	}
}

// sessionCollector emits the cache's drive/charge session opened/closed counters,
// labeled by kind (drives/charges) and edge (opened/closed), read from the snapshot
// func on each scrape. A persistent opened-minus-closed skew flags a missed boundary
// (a phantom drive left open, a charge never closed). Counting stays in the cache.
type sessionCollector struct {
	hold *sessionHolder
}

func newSessionCollector(h *sessionHolder) *sessionCollector {
	return &sessionCollector{hold: h}
}

var sessionDesc = prometheus.NewDesc(
	"redswitchboard_cache_sessions_total",
	"Drive/charge sessions the cache detected on the stream path, by kind (drives/charges) and edge (opened/closed). Opened and closed should balance over time.",
	[]string{"kind", "edge"}, nil,
)

func (c *sessionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- sessionDesc
}

func (c *sessionCollector) Collect(ch chan<- prometheus.Metric) {
	if c.hold.fn == nil {
		return
	}
	for key, n := range c.hold.fn() {
		kind, edge, ok := strings.Cut(key, "_")
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(sessionDesc, prometheus.CounterValue, float64(n), kind, edge)
	}
}

// unchangedCollector emits the P7 unchanged-reads counter (vehicle_data reads served
// from the AsOf fast path: the spinning-consumer signal), read from the snapshot func
// on each scrape. Process-wide on the sink, so it carries only the source label.
type unchangedCollector struct {
	hold *unchangedHolder
}

func newUnchangedCollector(h *unchangedHolder) *unchangedCollector {
	return &unchangedCollector{hold: h}
}

var unchangedDesc = prometheus.NewDesc(
	"redswitchboard_source_unchanged_reads_total",
	"vehicle_data reads served from the AsOf fast path because the source did not advance since the last translation (the spinning-consumer signal, ties to P7), by source.",
	[]string{"source"}, nil,
)

func (c *unchangedCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- unchangedDesc
}

func (c *unchangedCollector) Collect(ch chan<- prometheus.Metric) {
	hold := *c.hold
	if hold.fn == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(unchangedDesc, prometheus.CounterValue, float64(hold.fn()), hold.name)
}

// costCollector emits the Tesla $/day cost proxy: the per-source price of a billed
// vehicle_data fetch, and the running estimated spend (sum of paid fetches x price).
// It reads the same source snapshot the sourceCollector reads, so the fetch count
// is never counted twice; rate() over the cost counter in Grafana gives $/day.
type costCollector struct {
	hold *costHolder
}

func newCostCollector(h *costHolder) *costCollector {
	return &costCollector{hold: h}
}

var (
	costPriceDesc = prometheus.NewDesc(
		"redswitchboard_source_vehicle_data_price_usd",
		"Configured per-call USD price of a billed vehicle_data fetch (the cost-estimate multiplier), by source.",
		[]string{"source"}, nil,
	)
	costSpendDesc = prometheus.NewDesc(
		"redswitchboard_source_estimated_cost_usd_total",
		"Estimated cumulative USD spent on billed vehicle_data fetches (sum of fetches x price), by source. rate() gives the $/day burn.",
		[]string{"source"}, nil,
	)
)

func (c *costCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- costPriceDesc
	ch <- costSpendDesc
}

func (c *costCollector) Collect(ch chan<- prometheus.Metric) {
	hold := *c.hold
	if hold.fn == nil || hold.price <= 0 {
		return
	}
	var fetches int64
	for _, v := range hold.fn() {
		fetches += v.VehicleDataFetches
	}
	ch <- prometheus.MustNewConstMetric(costPriceDesc, prometheus.GaugeValue, hold.price, hold.name)
	ch <- prometheus.MustNewConstMetric(costSpendDesc, prometheus.CounterValue, float64(fetches)*hold.price, hold.name)
}

// commandCollector emits the command-path counters as const metrics read from
// the snapshot func on each scrape. Counting stays in the commander (it records
// each call's outcome); this only reads, mirroring the source/stream collectors.
type commandCollector struct {
	hold *commandHolder
}

func newCommandCollector(h *commandHolder) *commandCollector {
	return &commandCollector{hold: h}
}

type commandMetricDef struct {
	name      string
	help      string
	valueType prometheus.ValueType
	extract   func(s CommandStats) float64
}

var commandMetricDefs = []commandMetricDef{
	{"redswitchboard_commands_total", "Commands submitted to the commander, by commander plugin.", prometheus.CounterValue, func(s CommandStats) float64 { return float64(s.Sent) }},
	{"redswitchboard_command_successes_total", "Commands that succeeded (Ack{Result:true}), by commander plugin.", prometheus.CounterValue, func(s CommandStats) float64 { return float64(s.Successes) }},
	{"redswitchboard_command_nominal_failures_total", "Commands the vehicle rejected for a known reason (Ack{Result:false}, 200), by commander plugin.", prometheus.CounterValue, func(s CommandStats) float64 { return float64(s.NominalFailures) }},
	{"redswitchboard_command_infra_errors_total", "Commands that failed with an infrastructure error (5xx), by commander plugin.", prometheus.CounterValue, func(s CommandStats) float64 { return float64(s.InfraErrors) }},
	{"redswitchboard_command_wakes_total", "Wake commands sent (the expensive billing category), by commander plugin.", prometheus.CounterValue, func(s CommandStats) float64 { return float64(s.Wakes) }},
	{"redswitchboard_command_rate_limited_total", "Commands rejected with a rate limit, by commander plugin.", prometheus.CounterValue, func(s CommandStats) float64 { return float64(s.RateLimitedCount) }},
}

// commandDescs is built once and reused by Describe/Collect.
var commandDescs = func() []*prometheus.Desc {
	descs := make([]*prometheus.Desc, len(commandMetricDefs))
	for i, d := range commandMetricDefs {
		descs[i] = prometheus.NewDesc(d.name, d.help, []string{"commander"}, nil)
	}
	return descs
}()

func (c *commandCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range commandDescs {
		ch <- d
	}
}

func (c *commandCollector) Collect(ch chan<- prometheus.Metric) {
	hold := *c.hold
	if hold.fn == nil {
		return
	}
	s := hold.fn()
	for i, def := range commandMetricDefs {
		ch <- prometheus.MustNewConstMetric(commandDescs[i], def.valueType, def.extract(s), hold.name)
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// unixOrZero returns t as unix seconds, or 0 for the zero time.
func unixOrZero(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}

// render produces the full Prometheus text exposition (used by tests). It sets
// the vehicles_known gauge (the one piece of state the production Handler
// receives as an argument) and gathers + encodes the registry.
func (reg *Registry) render(vehiclesKnown int) string {
	reg.vehiclesKnown.Set(float64(vehiclesKnown))
	families, err := reg.prom.Gather()
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range families {
		_ = enc.Encode(f)
	}
	return buf.String()
}
