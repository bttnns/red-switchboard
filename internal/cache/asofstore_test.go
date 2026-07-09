package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAsOfPersistsAcrossRestart: a Service advances a vehicle's served AsOf, then
// a fresh Service over the same file (a simulated restart) resumes the clamp from
// the persisted high-water mark instead of 0, so a later stream/poll time BELOW
// that mark cannot drive the served AsOf backwards (the storm trigger).
func TestAsOfPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asof.json")
	ident := []vehicle.Identity{{ID: "g1"}}

	high := time.UnixMilli(5000)
	svc1, err := NewService(ident, 0, path, "", 0, nil)
	require.NoError(t, err)
	svc1.MergePoll("g1", vehicle.Snapshot{State: &vehicle.State{Power: vehicle.PowerOnline}, FetchedAt: high})
	assert.Equal(t, high, svc1.Latest("g1").AsOf, "served AsOf advanced to the poll time")
	svc1.FlushAsOf() // persist immediately (the throttle would otherwise defer)

	// Restart: a new Service reads the same file.
	svc2, err := NewService(ident, 0, path, "", 0, nil)
	require.NoError(t, err)

	// A poll OLDER than the persisted mark arrives (e.g. a poll behind a stream
	// that ran just before the restart). The clamp must hold the served AsOf at the
	// persisted high-water mark, never regressing.
	low := time.UnixMilli(2000)
	svc2.MergePoll("g1", vehicle.Snapshot{State: &vehicle.State{Power: vehicle.PowerOnline}, FetchedAt: low})
	got := svc2.Latest("g1").AsOf
	// .Equal compares instants (the JSON round-trip relocates the time to UTC, so a
	// struct-equality check would spuriously fail on a non-UTC host).
	assert.True(t, got.Equal(high), "served AsOf resumes from the persisted mark, not the older poll")
	assert.False(t, got.Before(high), "served AsOf never regresses below the persisted mark")
}

// TestAsOfMissingFileStartsClean: no file on first run is not an error; the store
// starts empty (no high-water marks) and serves the live time normally.
func TestAsOfMissingFileStartsClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	svc, err := NewService([]vehicle.Identity{{ID: "g1"}}, 0, path, "", 0, nil)
	require.NoError(t, err, "missing file must not error")

	at := time.UnixMilli(1000)
	svc.MergePoll("g1", vehicle.Snapshot{State: &vehicle.State{Power: vehicle.PowerOnline}, FetchedAt: at})
	assert.Equal(t, at, svc.Latest("g1").AsOf, "clean start serves the live time")
}

// TestAsOfCorruptFileErrors: a corrupt persistence file is a real boundary error,
// surfaced from NewService so a bad mount fails fast rather than silently dropping
// the high-water mark.
func TestAsOfCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asof.json")
	require.NoError(t, os.WriteFile(path, []byte("{not valid json"), 0600))

	_, err := NewService([]vehicle.Identity{{ID: "g1"}}, 0, path, "", 0, nil)
	assert.Error(t, err, "corrupt file must surface an error")
}

// TestAsOfStoreAdvanceOnlyRaises: Advance is monotonic (an older time is ignored)
// and the empty-path store is a pure in-memory no-op (no panic, no file).
func TestAsOfStoreAdvanceOnlyRaises(t *testing.T) {
	s, err := newAsofStore("")
	require.NoError(t, err)

	s.Advance("g1", time.UnixMilli(3000))
	s.Advance("g1", time.UnixMilli(1000)) // older: ignored
	assert.Equal(t, time.UnixMilli(3000), s.Get("g1"))

	s.Advance("g1", time.UnixMilli(4000))
	assert.Equal(t, time.UnixMilli(4000), s.Get("g1"))

	s.Flush() // empty path: no-op, must not panic
}
