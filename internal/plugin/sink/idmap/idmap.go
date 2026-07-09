// Package idmap maps Rivian vehicle GUID strings to stable positive int64 ids.
// TeslaMate stores eid and vehicle_id as integer columns, so every Rivian GUID
// needs a deterministic synthetic integer. The id derives from an FNV-1a hash of
// the GUID (so it is stable across restarts even without the cache), and a JSON
// file persists the mapping to guarantee stability and allow inspection.
package idmap

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
)

// Map is a persisted, concurrency-safe GUID <-> int64 id map.
type Map struct {
	mu    sync.Mutex
	path  string
	byID  map[int64]string
	byKey map[string]int64
}

// New loads (or initializes) a Map backed by the JSON file at path. An empty
// path means in-memory only (no persistence).
func New(path string) (*Map, error) {
	m := &Map{
		path:  path,
		byID:  make(map[int64]string),
		byKey: make(map[string]int64),
	}
	if path == "" {
		return m, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, fmt.Errorf("idmap: read %s: %w", path, err)
	}

	var stored map[string]int64
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("idmap: parse %s: %w", path, err)
	}
	for guid, id := range stored {
		m.byKey[guid] = id
		m.byID[id] = guid
	}
	return m, nil
}

// ID returns the stable positive int64 id for a Rivian GUID, assigning and
// persisting one on first use. Collisions (rare) are resolved by linear probing.
func (m *Map) ID(guid string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id, ok := m.byKey[guid]; ok {
		return id, nil
	}

	id := hashID(guid)
	for {
		existing, taken := m.byID[id]
		if !taken || existing == guid {
			break
		}
		id++
		if id <= 0 {
			id = 1
		}
	}

	m.byKey[guid] = id
	m.byID[id] = guid
	if err := m.persist(); err != nil {
		return 0, err
	}
	return id, nil
}

// GUID returns the Rivian GUID for a previously assigned id.
func (m *Map) GUID(id int64) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	guid, ok := m.byID[id]
	return guid, ok
}

// persist writes the current mapping to disk. Caller must hold m.mu.
func (m *Map) persist() error {
	if m.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(m.byKey, "", "  ")
	if err != nil {
		return fmt.Errorf("idmap: marshal: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("idmap: write %s: %w", m.path, err)
	}
	return nil
}

// hashID deterministically maps a GUID to a positive int64 via FNV-1a, masking
// off the sign bit so the result is always > 0.
func hashID(guid string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(guid))
	id := int64(h.Sum64() & 0x7fffffffffffffff)
	if id == 0 {
		id = 1
	}
	return id
}
