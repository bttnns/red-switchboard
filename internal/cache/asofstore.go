// asofStore persists the last-served AsOf high-water mark per vehicle so the
// monotonic AsOf clamp (Merger.commit) survives a restart. Without it a TRS
// restart mid-drive forgets the high-water mark, so a stream frame ahead of the
// next poll can drive the served AsOf backwards, which re-triggers TeslaMate's
// stale-fetch discard storm (it discards a frame older than the last accepted,
// then refetches with zero delay, a 250 req/s spin). It mirrors idmap: a JSON
// file loaded on New, persisted on advance, missing-file-is-clean.
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// asofFlushInterval throttles disk writes off the per-frame hot path. AsOf
// advances on nearly every poll/stream frame (unlike idmap, which writes only on
// a rare new-id assignment), so flushing every frame would fsync continuously. A
// crash between flushes loses at most this much advance, which only lowers the
// floor slightly; live data re-raises it on the next frame.
const asofFlushInterval = 10 * time.Second

// asofStore is a persisted, concurrency-safe map of vehicle id -> last-served
// AsOf. An empty path means in-memory only (no persistence).
type asofStore struct {
	mu          sync.Mutex
	path        string
	byID        map[string]time.Time
	lastFlushed time.Time
}

// newAsofStore loads (or initializes) a store backed by the JSON file at path. A
// missing file starts clean; a corrupt or unreadable file is a real boundary
// error (same as idmap), surfaced so a bad mount fails fast rather than silently
// dropping the high-water mark.
func newAsofStore(path string) (*asofStore, error) {
	s := &asofStore{path: path, byID: make(map[string]time.Time)}
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("asofstore: read %s: %w", path, err)
	}

	var stored map[string]time.Time
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("asofstore: parse %s: %w", path, err)
	}
	for id, t := range stored {
		s.byID[id] = t
	}
	return s, nil
}

// Get returns the persisted high-water mark for a vehicle (zero if none).
func (s *asofStore) Get(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// Advance records a new high-water mark for a vehicle (a no-op if it does not
// move the mark forward) and flushes to disk at most once per asofFlushInterval.
func (s *asofStore) Advance(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !at.After(s.byID[id]) {
		return
	}
	s.byID[id] = at
	if time.Since(s.lastFlushed) >= asofFlushInterval {
		s.persist()
	}
}

// Flush forces a write of the current marks, used on shutdown so the most recent
// advance is not lost to the throttle window.
func (s *asofStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persist()
}

// persist writes the current marks to disk atomically (temp file + rename) so a
// crash mid-write cannot leave a half-written file that newAsofStore would then
// reject as corrupt and refuse to boot from. Caller must hold s.mu. A write error
// is non-fatal: the clamp keeps working in-memory. lastFlushed advances regardless
// of outcome so a persistently failing write (read-only mount, full disk) stays
// throttled instead of re-firing on every frame.
func (s *asofStore) persist() {
	if s.path == "" {
		return
	}
	s.lastFlushed = time.Now()
	data, err := json.MarshalIndent(s.byID, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
}
