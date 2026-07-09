package wssutil

import (
	"testing"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
)

// TestSourceCountersConnectsAndFields: Connected bumps both the live gauge and
// the cumulative connects counter (so a reconnect storm is visible), Disconnected
// only the gauge, and RecordFields tallies per streamed field.
func TestSourceCountersConnectsAndFields(t *testing.T) {
	var c SourceCounters
	c.Connected()
	c.Connected()
	c.Disconnected()

	c.Frame()
	c.RecordFields(vehicle.StreamLoc | vehicle.StreamSpeed)
	c.Frame()
	c.RecordFields(vehicle.StreamLoc | vehicle.StreamSOC)

	s := c.Stats()
	assert.Equal(t, int64(1), s.Connected, "live: 2 up - 1 down")
	assert.Equal(t, int64(2), s.Connects, "cumulative connections opened")
	assert.Equal(t, int64(1), s.Disconnects, "cumulative connections closed")
	assert.Equal(t, int64(2), s.Frames)
	assert.Equal(t, int64(2), s.FieldFrames["location"])
	assert.Equal(t, int64(1), s.FieldFrames["speed"])
	assert.Equal(t, int64(1), s.FieldFrames["soc"])
	assert.Zero(t, s.FieldFrames["range"])
}

// TestSourceCountersIdleTimeout: IdleTimeout is a subset of disconnects (every
// idle reap is also a close), tracked separately so a stalled-stream rate is
// distinguishable from clean churn.
func TestSourceCountersIdleTimeout(t *testing.T) {
	var c SourceCounters
	c.Connected()
	c.Connected()
	c.Disconnected() // clean close
	c.Disconnected() // idle reap
	c.IdleTimeout()

	s := c.Stats()
	assert.Equal(t, int64(2), s.Disconnects, "both closes counted")
	assert.Equal(t, int64(1), s.IdleTimeouts, "only the reaped one")
}

// TestSourceCountersRejects: Reject tallies per bounded reason and is reported in
// the snapshot; an absent reason is zero, and reasons stay separate.
func TestSourceCountersRejects(t *testing.T) {
	var c SourceCounters
	assert.Nil(t, c.Stats().Rejects, "no rejects: nil map")

	c.Reject("unknown_vin")
	c.Reject("unknown_vin")
	c.Reject("identity")

	s := c.Stats()
	assert.Equal(t, int64(2), s.Rejects["unknown_vin"])
	assert.Equal(t, int64(1), s.Rejects["identity"])
	assert.Zero(t, s.Rejects["other"])
}

// TestSourceCountersFrameGap: the first frame seeds no gap; each subsequent frame
// observes one gap into the histogram (count == frames-1).
func TestSourceCountersFrameGap(t *testing.T) {
	var c SourceCounters
	c.Frame()
	c.Frame()
	c.Frame()
	s := c.Stats()
	assert.Equal(t, int64(3), s.Frames)
	assert.Equal(t, uint64(2), s.GapCount, "gaps observed = frames - 1")
	assert.NotNil(t, s.GapBuckets)
	// Sub-second gaps land in every cumulative bucket.
	assert.Equal(t, uint64(2), s.GapBuckets[300])
}
