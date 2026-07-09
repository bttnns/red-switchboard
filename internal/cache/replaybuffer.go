// replayBuffer is a bounded on-disk ring of the most recent merged snapshots per
// vehicle, so a TRS OR TeslaMate restart mid-drive does not drop the frames that
// landed during the outage: on reconnect the ring is replayed to the consumer in
// order, closing the gap between the last frame the consumer saw and the live
// state. It complements asofStore (P3): P3 restores the served-AsOf high-water
// mark so timestamps never regress; this restores the recent telemetry HISTORY so
// the positions/SOC samples between restart and first live frame are not lost.
//
// It mirrors asofStore's file-IO contract: a JSON file loaded on New, a missing
// file is a clean start, a corrupt/unreadable file is a real boundary error. The
// ring is bounded (oldest evicted past maxPerVehicle) so growth is capped, and
// writes are atomic (temp file + rename) so a crash mid-write cannot corrupt it.
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// replayFlushInterval throttles disk writes off the per-frame hot path, like
// asofStore: a drive appends a snapshot on nearly every frame, so flushing each
// one would fsync continuously. A crash between flushes loses at most this much
// tail; live frames re-fill it on reconnect.
const replayFlushInterval = 10 * time.Second

// defaultReplayDepth bounds the per-vehicle ring when the config knob is unset but
// the buffer is enabled. Sized to cover a few minutes of 1Hz drive frames so a
// routine restart replays the whole gap, while staying small on disk.
const defaultReplayDepth = 256

// replayBuffer is a persisted, concurrency-safe per-vehicle ring of merged
// snapshots. An empty path means disabled (no capture, no persistence): the zero-
// overhead off state.
type replayBuffer struct {
	mu          sync.Mutex
	path        string
	depth       int
	byID        map[string][]vehicle.Snapshot
	lastFlushed time.Time
}

// newReplayBuffer loads (or initializes) a ring backed by the JSON file at path.
// An empty path disables it. A missing file starts clean; a corrupt or unreadable
// file is a real boundary error (like asofStore) so a bad mount fails fast rather
// than silently dropping the history. depth <= 0 falls back to defaultReplayDepth.
func newReplayBuffer(path string, depth int) (*replayBuffer, error) {
	if depth <= 0 {
		depth = defaultReplayDepth
	}
	b := &replayBuffer{path: path, depth: depth, byID: make(map[string][]vehicle.Snapshot)}
	if path == "" {
		return b, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, fmt.Errorf("replaybuffer: read %s: %w", path, err)
	}

	var stored map[string][]vehicle.Snapshot
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("replaybuffer: parse %s: %w", path, err)
	}
	for id, ring := range stored {
		// Trim a ring that shrank across a config change so the loaded state already
		// obeys the current bound.
		if len(ring) > b.depth {
			ring = ring[len(ring)-b.depth:]
		}
		b.byID[id] = ring
	}
	return b, nil
}

// Append records a merged snapshot to a vehicle's ring, evicting the oldest entry
// past depth, and flushes to disk at most once per replayFlushInterval. A disabled
// buffer (empty path) is a pure no-op.
func (b *replayBuffer) Append(id string, snap vehicle.Snapshot) {
	if b == nil || b.path == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := append(b.byID[id], snap)
	if len(ring) > b.depth {
		ring = ring[len(ring)-b.depth:]
	}
	b.byID[id] = ring
	if time.Since(b.lastFlushed) >= replayFlushInterval {
		b.persist()
	}
}

// Replay returns a copy of a vehicle's buffered snapshots, oldest first, for
// re-emission to a reconnecting consumer. A disabled buffer or an unknown id
// yields nil.
func (b *replayBuffer) Replay(id string) []vehicle.Snapshot {
	if b == nil || b.path == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ring := b.byID[id]
	if len(ring) == 0 {
		return nil
	}
	out := make([]vehicle.Snapshot, len(ring))
	copy(out, ring)
	return out
}

// Flush forces a write of the current rings, used on shutdown so the most recent
// appends are not lost to the throttle window.
func (b *replayBuffer) Flush() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.persist()
}

// persist writes the rings to disk atomically (temp file + rename) so a crash
// mid-write cannot leave a half-written, corrupt file: a reader always sees either
// the old complete file or the new complete one. Caller must hold b.mu. A write
// error is non-fatal: the ring keeps working in-memory. lastFlushed advances
// regardless of outcome so a persistently failing write (read-only mount, full
// disk) stays throttled instead of re-firing on every frame.
func (b *replayBuffer) persist() {
	if b.path == "" {
		return
	}
	b.lastFlushed = time.Now()
	data, err := json.Marshal(b.byID)
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	if err := os.Rename(tmp, b.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
}
