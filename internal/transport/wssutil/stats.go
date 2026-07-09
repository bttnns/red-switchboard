package wssutil

// stats.go is the shared streaming-source counter set. Every push source tracks
// the same things (currently-connected sessions, total connections opened, total
// decoded frames, the age of the last frame, the distribution of inter-frame
// gaps, and per-canonical-field frame counts), so they embed SourceCounters
// rather than each re-declaring the atomics and an identical Stats() reader.
//
// Counting stays here (out of Prometheus); the metrics collector reads a
// SourceStats SNAPSHOT at scrape time and turns it into const metrics, including
// a const histogram built from the manual gap buckets. So this package stays
// Prometheus-free, mirroring how the poll loop's counters are read at scrape.

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// gapBucketsSeconds are the upper bounds (le) of the frame-gap histogram, chosen
// to span a healthy live-drive cadence (sub-second to a few seconds) through a
// stalling stream (tens of seconds to minutes). They size a future poll backoff.
var gapBucketsSeconds = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}

// streamFieldNames maps each canonical streamed-field bit to its metric label, so
// per-field frame counters report which signals the car is actually sending.
var streamFieldNames = []struct {
	bit  vehicle.StreamField
	name string
}{
	{vehicle.StreamLoc, "location"},
	{vehicle.StreamSpeed, "speed"},
	{vehicle.StreamHeading, "heading"},
	{vehicle.StreamGear, "gear"},
	{vehicle.StreamOdometer, "odometer"},
	{vehicle.StreamSOC, "soc"},
	{vehicle.StreamRange, "range"},
	{vehicle.StreamChargePower, "charge_power"},
	{vehicle.StreamCabinTemp, "cabin_temp"},
	{vehicle.StreamOutsideTemp, "outside_temp"},
	{vehicle.StreamTpmsFl, "tpms_fl"},
	{vehicle.StreamTpmsFr, "tpms_fr"},
	{vehicle.StreamTpmsRl, "tpms_rl"},
	{vehicle.StreamTpmsRr, "tpms_rr"},
	{vehicle.StreamChargeLimit, "charge_limit_soc"},
	{vehicle.StreamTimeToFull, "time_to_full_charge"},
	{vehicle.StreamChargerVoltage, "charger_voltage"},
	{vehicle.StreamChargeCurrent, "charge_amps"},
	{vehicle.StreamChargeState, "detailed_charge_state"},
	{vehicle.StreamBatteryHeater, "battery_heater_on"},
	{vehicle.StreamLocked, "locked"},
	{vehicle.StreamSentry, "sentry_mode"},
}

// SourceStats is the streaming-source snapshot read at scrape time.
type SourceStats struct {
	Connected    int64         // currently connected sessions
	Connects     int64         // total connections opened (reconnect storms inflate this)
	Disconnects  int64         // total connections closed (with Connects, the churn rate)
	IdleTimeouts int64         // connections reaped for sending no frame within idle_timeout
	Frames       int64         // total decoded frames accepted
	LastFrameAge time.Duration // age of the most recent frame

	// Frame-gap histogram: cumulative le->count plus the sum/count needed to build
	// a Prometheus const histogram. Sizes how aggressively a poll may back off.
	GapBuckets map[float64]uint64
	GapSum     float64
	GapCount   uint64

	// FieldFrames counts frames carrying each streamed field (by label name), so
	// you can confirm RatedRange / charge power actually arrive after enabling them.
	FieldFrames map[string]int64

	// Rejects counts connections/frames denied at the trust boundary, by a bounded
	// reason (e.g. "identity", "unknown_vin"). Feeds the security dashboard; never
	// labeled by raw IP or VIN (unbounded cardinality).
	Rejects map[string]int64
}

// SourceCounters is the embeddable stat set for a streaming source. Embed it in
// the source struct (by value; the source is always used as a pointer) to get
// the Connected/Disconnected/Frame/RecordFields mutators and a promoted Stats()
// reader. The simple counters are atomics; the gap histogram and per-field map
// are guarded by a mutex (frame rate is low, so the lock is uncontended).
type SourceCounters struct {
	connected    atomic.Int64
	connects     atomic.Int64
	disconnects  atomic.Int64
	idleTimeouts atomic.Int64
	frames       atomic.Int64
	lastFrameAt  atomic.Int64 // unix nano

	mu         sync.Mutex
	gapBuckets []uint64 // parallel to gapBucketsSeconds; cumulative built in Stats
	gapSum     float64
	gapCount   uint64
	fieldFrame map[string]int64
	rejects    map[string]int64
}

// Connected / Disconnected adjust the live session count. Connected also bumps
// the total-connections counter, so a reconnect storm is visible even though the
// live gauge only ever shows 0/1.
func (c *SourceCounters) Connected() {
	c.connected.Add(1)
	c.connects.Add(1)
}
func (c *SourceCounters) Disconnected() {
	c.connected.Add(-1)
	c.disconnects.Add(1)
}

// IdleTimeout counts a connection reaped because it sent no frame within the
// idle_timeout (a stalled/half-open stream), separate from the total close count.
func (c *SourceCounters) IdleTimeout() { c.idleTimeouts.Add(1) }

// Frame records one decoded frame: it stamps the last-frame time and observes the
// gap since the previous frame into the gap histogram.
func (c *SourceCounters) Frame() {
	now := time.Now().UnixNano()
	prev := c.lastFrameAt.Swap(now)
	c.frames.Add(1)
	if prev > 0 {
		c.observeGap(time.Duration(now - prev).Seconds())
	}
}

// RecordFields increments the per-field counters for the streamed fields a frame
// carried (present is the snapshot's StreamPresent bitmask). Called alongside
// Frame by sources that decode canonical field deltas.
func (c *SourceCounters) RecordFields(present vehicle.StreamField) {
	if present == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fieldFrame == nil {
		c.fieldFrame = make(map[string]int64, len(streamFieldNames))
	}
	for _, f := range streamFieldNames {
		if present&f.bit != 0 {
			c.fieldFrame[f.name]++
		}
	}
}

// Reject records one connection/frame denied at the trust boundary, by a bounded
// reason. The caller MUST pass a fixed reason string (never raw IP/VIN) to keep
// the label cardinality bounded.
func (c *SourceCounters) Reject(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejects == nil {
		c.rejects = make(map[string]int64)
	}
	c.rejects[reason]++
}

func (c *SourceCounters) observeGap(secs float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gapBuckets == nil {
		c.gapBuckets = make([]uint64, len(gapBucketsSeconds))
	}
	c.gapSum += secs
	c.gapCount++
	for i, le := range gapBucketsSeconds {
		if secs <= le {
			c.gapBuckets[i]++
		}
	}
}

// Stats returns the source-side snapshot for the /metrics surface.
func (c *SourceCounters) Stats() SourceStats {
	s := SourceStats{
		Connected:    c.connected.Load(),
		Connects:     c.connects.Load(),
		Disconnects:  c.disconnects.Load(),
		IdleTimeouts: c.idleTimeouts.Load(),
		Frames:       c.frames.Load(),
	}
	if ns := c.lastFrameAt.Load(); ns > 0 {
		s.LastFrameAge = time.Since(time.Unix(0, ns))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s.GapSum = c.gapSum
	s.GapCount = c.gapCount
	if c.gapBuckets != nil {
		s.GapBuckets = make(map[float64]uint64, len(gapBucketsSeconds))
		for i, le := range gapBucketsSeconds {
			s.GapBuckets[le] = c.gapBuckets[i]
		}
	}
	if len(c.fieldFrame) > 0 {
		s.FieldFrames = make(map[string]int64, len(c.fieldFrame))
		for k, v := range c.fieldFrame {
			s.FieldFrames[k] = v
		}
	}
	if len(c.rejects) > 0 {
		s.Rejects = make(map[string]int64, len(c.rejects))
		for k, v := range c.rejects {
			s.Rejects[k] = v
		}
	}
	return s
}

// SourceStatser is the optional capability serve.go reads to wire the
// streaming-source gauges. A source exposes it for free by embedding
// SourceCounters.
type SourceStatser interface {
	Stats() SourceStats
}

// SinkStatser is the optional capability serve.go reads to wire the
// streaming-sink gauges.
type SinkStatser interface {
	Stats() (consumers, pushed, dropped int64)
}
