package cache

import (
	"sync/atomic"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// session.go tallies the drive/charge session boundaries the cache detects on the
// live stream path: each time a session OPENS or CLOSES. These are correctness
// signals that should balance over time (a drive that opens must eventually close),
// so a persistent opened-minus-closed skew flags a missed boundary (a phantom drive
// left open, a charge never closed). Counting stays in the cache (Prometheus-free,
// like integrityCounters); the metrics collector reads Snapshot() at scrape time.

// sessionCounters is the shared opened/closed counter set, one instance owned by
// the Service and shared across every per-vehicle Merger. The metric labels are the
// session kind (drive/charge) and the edge (opened/closed), never a VIN, so
// cardinality stays bounded.
type sessionCounters struct {
	drivesOpened  atomic.Int64
	drivesClosed  atomic.Int64
	chargesOpened atomic.Int64
	chargesClosed atomic.Int64
}

// driveBoundary classifies a fired drive boundary as an open (now in a moving gear)
// or a close, then increments the matching counter. now is the merged snapshot after
// the frame that fired the boundary.
func (c *sessionCounters) driveBoundary(now vehicle.Snapshot) {
	if c == nil {
		return
	}
	if inDrive(gearOf(now)) {
		c.drivesOpened.Add(1)
	} else {
		c.drivesClosed.Add(1)
	}
}

func (c *sessionCounters) chargeOpened() {
	if c != nil {
		c.chargesOpened.Add(1)
	}
}

func (c *sessionCounters) chargeClosed() {
	if c != nil {
		c.chargesClosed.Add(1)
	}
}

// Snapshot returns the current opened/closed totals keyed by "<kind>_<edge>" (every
// key present, 0 if never fired). A nil receiver yields a zeroed map so a
// non-streaming serve path can wire the collector unconditionally.
func (c *sessionCounters) Snapshot() map[string]int64 {
	out := map[string]int64{
		"drives_opened":  0,
		"drives_closed":  0,
		"charges_opened": 0,
		"charges_closed": 0,
	}
	if c == nil {
		return out
	}
	out["drives_opened"] = c.drivesOpened.Load()
	out["drives_closed"] = c.drivesClosed.Load()
	out["charges_opened"] = c.chargesOpened.Load()
	out["charges_closed"] = c.chargesClosed.Load()
	return out
}

// chargeStarted reports whether the frame opened a charge: the cache held no live
// charge (or 0kW) and the raw frame carries a non-zero charge power. The mirror of
// chargeEnded, so the two edges balance.
func chargeStarted(prev, raw vehicle.Snapshot) bool {
	if prev.Live != nil && prev.Live.PowerKw > 0 {
		return false
	}
	if raw.StreamPresent&vehicle.StreamChargePower == 0 {
		return false
	}
	return raw.Live != nil && raw.Live.PowerKw > 0
}
