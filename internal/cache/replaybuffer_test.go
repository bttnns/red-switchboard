package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snap is a terse merged-snapshot builder for the ring tests: one streamed speed
// at a given time, enough to round-trip and to order by.
func snap(ms int64, speed float64) vehicle.Snapshot {
	return vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: speed},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.UnixMilli(ms),
	}
}

// TestReplayBufferBounds: the ring evicts the oldest entry past depth so on-disk
// growth is capped, and Replay returns the surviving window oldest-first.
func TestReplayBufferBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	b, err := newReplayBuffer(path, 3)
	require.NoError(t, err)

	for i := int64(1); i <= 5; i++ {
		b.Append("g1", snap(i*1000, float64(i)))
	}

	got := b.Replay("g1")
	require.Len(t, got, 3, "ring bounded to depth")
	assert.Equal(t, 3.0, got[0].State.SpeedMps, "oldest survivor first (1,2 evicted)")
	assert.Equal(t, 5.0, got[2].State.SpeedMps, "newest last")
}

// TestReplayBufferRoundTrip: an Append+Flush then a fresh buffer over the same file
// (a simulated restart) reloads the ring in order, so the frames buffered before
// the restart are available to replay.
func TestReplayBufferRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")

	b1, err := newReplayBuffer(path, 8)
	require.NoError(t, err)
	b1.Append("g1", snap(1000, 10))
	b1.Append("g1", snap(2000, 20))
	b1.Flush()

	b2, err := newReplayBuffer(path, 8)
	require.NoError(t, err)
	got := b2.Replay("g1")
	require.Len(t, got, 2, "ring reloaded across restart")
	assert.Equal(t, 10.0, got[0].State.SpeedMps)
	assert.Equal(t, 20.0, got[1].State.SpeedMps)
	assert.True(t, got[1].FetchedAt.Equal(time.UnixMilli(2000)), "timestamps round-trip")
}

// TestReplayBufferTrimsOnShrunkDepth: a ring loaded under a smaller depth than it
// was written with is trimmed to the new bound on load, so a config change does not
// leave it over-bounded.
func TestReplayBufferTrimsOnShrunkDepth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	b1, err := newReplayBuffer(path, 8)
	require.NoError(t, err)
	for i := int64(1); i <= 6; i++ {
		b1.Append("g1", snap(i*1000, float64(i)))
	}
	b1.Flush()

	b2, err := newReplayBuffer(path, 2)
	require.NoError(t, err)
	got := b2.Replay("g1")
	require.Len(t, got, 2, "loaded ring trimmed to the smaller depth")
	assert.Equal(t, 5.0, got[0].State.SpeedMps, "kept the newest 2")
	assert.Equal(t, 6.0, got[1].State.SpeedMps)
}

// TestReplayBufferDisabled: an empty path is a pure no-op (no capture, no replay,
// no file), the zero-overhead off state.
func TestReplayBufferDisabled(t *testing.T) {
	dir := t.TempDir()
	b, err := newReplayBuffer("", 4)
	require.NoError(t, err)
	b.Append("g1", snap(1000, 10))
	b.Flush() // must not panic, must not write
	assert.Nil(t, b.Replay("g1"), "disabled buffer never replays")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "disabled buffer writes no file")
}

// TestReplayBufferMissingFileStartsClean: no file on first run is not an error; the
// ring starts empty.
func TestReplayBufferMissingFileStartsClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	b, err := newReplayBuffer(path, 4)
	require.NoError(t, err, "missing file must not error")
	assert.Nil(t, b.Replay("g1"))
}

// TestReplayBufferCorruptFileErrors: a corrupt persistence file is a real boundary
// error surfaced from newReplayBuffer (mirrors asofStore) so a bad mount fails fast.
func TestReplayBufferCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	require.NoError(t, os.WriteFile(path, []byte("{not valid json"), 0600))

	_, err := newReplayBuffer(path, 4)
	assert.Error(t, err, "corrupt file must surface an error")
}

// TestServiceCapturesAndReplays: the Service captures every merged poll/stream
// snapshot into the ring, and Service.Replay returns them oldest-first for a
// reconnecting consumer.
func TestServiceCapturesAndReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	ident := []vehicle.Identity{{ID: "g1"}}
	svc, err := NewService(ident, 0, "", path, 16, nil)
	require.NoError(t, err)

	svc.MergePoll("g1", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline},
		FetchedAt: time.UnixMilli(1000),
	})
	_ = svc.Put(context.Background(), "g1", vehicle.Snapshot{
		State:         &vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive, SpeedMps: 15},
		StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear,
		FetchedAt:     time.UnixMilli(2000),
	})

	got := svc.Replay("g1")
	require.Len(t, got, 2, "both the poll and the stream merge were captured")
	assert.Equal(t, vehicle.PowerOnline, got[0].State.Power, "poll snapshot first")
	assert.Equal(t, 15.0, got[1].State.SpeedMps, "stream snapshot second")
}

// TestServiceReplayDisabledByDefault: with no replay_file the Service captures
// nothing and Replay is empty (zero overhead off-by-default).
func TestServiceReplayDisabledByDefault(t *testing.T) {
	svc := newTestService(t, []vehicle.Identity{{ID: "g1"}}, 0)
	svc.MergePoll("g1", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline},
		FetchedAt: time.UnixMilli(1000),
	})
	assert.Nil(t, svc.Replay("g1"), "no replay_file means no capture")
}

// TestServiceRestartMidDriveReplays is the J2 acceptance scenario: a drive streams
// frames into the ring, TRS is killed mid-drive (a fresh Service over the same
// file), and on restart the buffered drive frames replay in order so a reconnecting
// consumer sees no gap in positions.
func TestServiceRestartMidDriveReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	ident := []vehicle.Identity{{ID: "g1"}}

	// Drive in progress: a poll opens the car online, then stream frames carry the
	// moving positions/speed that a restart must not drop.
	svc1, err := NewService(ident, 0, "", path, 64, nil)
	require.NoError(t, err)
	svc1.MergePoll("g1", vehicle.Snapshot{
		State:     &vehicle.State{Power: vehicle.PowerOnline},
		FetchedAt: time.UnixMilli(1000),
	})
	// Plausible 1Hz drive: ~11 m steps per second (0.0001 deg) so the integrity gate
	// does not reject them as a GPS teleport.
	const baseLat = 37.5
	for i := int64(1); i <= 4; i++ {
		_ = svc1.Put(context.Background(), "g1", vehicle.Snapshot{
			State: &vehicle.State{
				Power:    vehicle.PowerOnline,
				Gear:     vehicle.GearDrive,
				SpeedMps: 11,
				Location: &vehicle.Location{Latitude: baseLat + float64(i)*0.0001, Longitude: -122.2},
			},
			StreamPresent: vehicle.StreamSpeed | vehicle.StreamGear | vehicle.StreamLoc,
			FetchedAt:     time.UnixMilli(1000 + i*1000),
		})
	}
	svc1.FlushReplay() // simulate the shutdown flush before the kill

	// Restart mid-drive: a fresh Service over the same file replays the buffered
	// frames, in order, so the consumer reconnecting after the restart re-receives
	// the in-flight drive history.
	svc2, err := NewService(ident, 0, "", path, 64, nil)
	require.NoError(t, err)
	got := svc2.Replay("g1")
	require.Len(t, got, 5, "poll + 4 drive frames survived the restart")

	var lats []float64
	for _, s := range got {
		if s.State.Location != nil {
			lats = append(lats, s.State.Location.Latitude)
		}
	}
	want := []float64{baseLat + 0.0001, baseLat + 0.0002, baseLat + 0.0003, baseLat + 0.0004}
	assert.Equal(t, want, lats, "drive positions replay in order, no gap")
}
