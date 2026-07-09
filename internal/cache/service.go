// Service is the cache wiring between the poll loop, streaming sources, and
// streaming sinks: it owns one *Merger per served vehicle and exposes the three
// read/write views the rest of the hub needs. It implements:
//   - streamsource.RecordSink: a streaming source's Put -> MergeStream.
//   - streamsink.CacheWatcher: Latest/Subscribe/Vehicles over the Merger set.
//   - poll.Cache: the poll loop writes its served snapshot through MergePoll.
//   - a sink.Provider via Provider(): Latest from the Merger (merged content),
//     Stats delegated to the poll Manager (poll health is poll-owned).
//
// The poll loop keeps its OWN last-poll snapshot for cadence/stats and writes the
// served snapshot through here; it never reads merged content back. The Service
// is created only when streaming is configured, so the non-streaming serve path
// is unchanged.
package cache

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// Service owns one Merger per served vehicle plus the shared stale_after and the
// poll Manager (for Stats delegation in the Provider adapter).
type Service struct {
	mu         sync.RWMutex
	mergers    map[string]*Merger
	order      []string
	identities []vehicle.Identity
	staleAfter time.Duration
	mgr        *poll.Manager      // for Stats delegation in the Provider adapter
	asof       *asofStore         // persisted served-AsOf high-water mark (clamp survives restart)
	replay     *replayBuffer      // bounded on-disk ring of recent merged snapshots (restart-safe replay)
	integrity  *integrityCounters // shared stream-integrity rejection counters (reason-labeled)
	session    *sessionCounters   // shared drive/charge opened/closed counters (kind+edge-labeled)
	logger     *log.Logger

	// pollTrigger, when set, is called with a vehicle id when a stream frame reveals
	// a session boundary (drive/charge start or end), so the poll loop can fetch the
	// terminal state promptly. Set once at startup (serve wires it to Manager.PollNow).
	pollTrigger func(id string)

	// streamFreshWithin gates the disconnect-poll: a disconnect whose last stream
	// frame is newer than this is a routine recycle (cache still current), so it must
	// not manufacture a billed poll. Zero disables the gate (always poll). Set from
	// the configured poll.stream_fresh_within; independent of driving_streaming, so
	// the gate can suppress even when the poller's drive-cadence backoff is disabled.
	streamFreshWithin time.Duration
}

// NewService builds one Merger per served identity. staleAfter is the configured
// poll.stale_after, reused to age stalled streams out (see Merger.commit).
// asofFile persists the served-AsOf high-water mark per vehicle (empty = in-memory
// only); a corrupt/unreadable file is a real boundary error. Each Merger is seeded
// with its persisted mark so the monotonic AsOf clamp resumes there after a
// restart instead of from 0. replayFile + replayDepth back the bounded on-disk
// ring that survives a restart and is replayed to a reconnecting consumer (empty
// replayFile = disabled, zero overhead).
func NewService(identities []vehicle.Identity, staleAfter time.Duration, asofFile, replayFile string, replayDepth int, logger *log.Logger) (*Service, error) {
	if logger == nil {
		logger = log.Default()
	}
	store, err := newAsofStore(asofFile)
	if err != nil {
		return nil, err
	}
	ring, err := newReplayBuffer(replayFile, replayDepth)
	if err != nil {
		return nil, err
	}
	s := &Service{
		mergers:    make(map[string]*Merger, len(identities)),
		identities: identities,
		staleAfter: staleAfter,
		asof:       store,
		replay:     ring,
		integrity:  &integrityCounters{},
		session:    &sessionCounters{},
		logger:     logger,
	}
	for _, id := range identities {
		s.mergers[id.ID] = newPersistedMerger(id.ID, store.Get(id.ID), store.Advance, s.integrity, s.session, logger)
		s.order = append(s.order, id.ID)
	}
	return s, nil
}

// IntegrityStats returns the current stream-integrity rejection totals per reason
// (every reason present, 0 if never tripped). serve reads it at scrape time to
// emit the redswitchboard_stream_integrity_rejections_total counter, keeping the
// counting in the cache (Prometheus-free) like the rest of internal/cache.
func (s *Service) IntegrityStats() map[string]int64 { return s.integrity.Snapshot() }

// SessionStats returns the current drive/charge opened/closed totals keyed by
// "<kind>_<edge>" (every key present, 0 if never fired). serve reads it at scrape
// time to emit the redswitchboard_cache_*_total session counters, keeping the
// counting in the cache (Prometheus-free) like the rest of internal/cache.
func (s *Service) SessionStats() map[string]int64 { return s.session.Snapshot() }

// FlushAsOf forces the persisted AsOf marks to disk (the throttle defers most
// writes off the hot path). serve calls it on shutdown so the most recent advance
// is not lost to the throttle window.
func (s *Service) FlushAsOf() { s.asof.Flush() }

// FlushReplay forces the replay ring to disk (the throttle defers most writes off
// the hot path). serve calls it on shutdown so the tail of an in-flight drive is
// not lost to the throttle window.
func (s *Service) FlushReplay() { s.replay.Flush() }

// Replay returns the buffered merged snapshots for a vehicle, oldest first, so a
// reconnecting consumer re-receives the frames that landed during a TRS/consumer
// restart instead of seeing only the live state with a gap. Nil when the buffer is
// disabled or the vehicle has no history. Implements streamsink.CacheReplayer.
func (s *Service) Replay(id string) []vehicle.Snapshot { return s.replay.Replay(id) }

// SetManager wires the poll Manager for Stats delegation. Called by serve after
// NewService and before Run.
func (s *Service) SetManager(mgr *poll.Manager) { s.mgr = mgr }

// SetPollTrigger wires the callback invoked on a session boundary (see Put). Called
// once at startup before any stream frame arrives, so no lock is needed.
func (s *Service) SetPollTrigger(fn func(id string)) { s.pollTrigger = fn }

// SetStreamFreshWithin sets the recycle-suppression window for OnStreamDisconnect.
// Zero (the default) disables the gate so every disconnect polls.
func (s *Service) SetStreamFreshWithin(d time.Duration) { s.streamFreshWithin = d }

func (s *Service) merger(id string) *Merger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mergers[id]
}

// MergePoll implements poll.Cache: the poll loop writes its served snapshot
// through the vehicle's Merger. Unknown id is a no-op (the poll loop only polls
// served ids). Returns the merged (served) snapshot; the poll loop ignores it and
// keeps its own raw poll record for cadence/stats.
func (s *Service) MergePoll(id string, snap vehicle.Snapshot) vehicle.Snapshot {
	m := s.merger(id)
	if m == nil {
		return snap
	}
	merged := m.MergePoll(snap, s.staleAfter)
	s.replay.Append(id, merged)
	return merged
}

// LastStreamAt implements poll.Cache: when a streamed frame last refreshed the
// vehicle's live fields (zero for an unknown id or one never streamed).
func (s *Service) LastStreamAt(id string) time.Time {
	if m := s.merger(id); m != nil {
		return m.LastStreamAt()
	}
	return time.Time{}
}

// Put implements streamsource.RecordSink: merge a pushed snapshot for one
// vehicle. An unknown id is rejected (deny-by-default) so a rogue vehicle cannot
// fill the cache; a frame carrying no streamed field (StreamPresent == 0, a
// keepalive) is dropped so it cannot trip a spurious subscriber notify.
func (s *Service) Put(_ context.Context, id string, snap vehicle.Snapshot) error {
	m := s.merger(id)
	if m == nil {
		return nil
	}
	if snap.State == nil || snap.StreamPresent == 0 {
		return nil
	}
	// Merge and detect session/wake boundaries atomically: MergeStreamDetect runs the
	// merge and the boundary checks under one Merger lock so two concurrent frames for
	// the same vehicle (a connection recycle's brief overlap) cannot feed a mismatched
	// prev/now pair to the detectors. A revealed boundary (gear Park<->Drive, charge
	// power ->0) or a confirmed wake from sleep fires a debounced PollNow so TeslaMate
	// gets the terminal/online state without waiting out the slow cadence.
	merged, poll := m.MergeStreamDetect(snap, s.staleAfter)
	s.replay.Append(id, merged)
	if poll && s.pollTrigger != nil {
		s.pollTrigger(id)
	}
	return nil
}

// OnStreamDisconnect implements streamsource.StreamDisconnectNotifier: fires a
// poll when a vehicle's stream genuinely falls silent, so a stream that drops
// mid-drive (without a GearPark frame) does not hold stale driving gear in the
// cache until the next scheduled poll.
//
// Tesla cycles the telemetry connection on a fixed ~120s lifetime and immediately
// redials, so MOST disconnects are routine recycles: the last frame is seconds old
// and the served cache is still current. Polling those manufactures a billed
// vehicle_data call (and a driving->online->driving flip that fragments the drive)
// for no new data. So suppress the poll when the last frame is newer than
// streamFreshWithin. A genuine silence (age past the window, or no stream ever)
// still polls. Known gap: a real park that drops with a fresh last frame looks
// identical to a recycle here, so its drive-close waits for the next scheduled poll
// -- up to one DrivingStreaming tick (~2m), since the poll loop does not re-evaluate
// its sleep mid-interval. The pollTrigger guard (minInterval) still throttles rapid
// genuine polls.
func (s *Service) OnStreamDisconnect(_ context.Context, id string) {
	if s.pollTrigger == nil {
		return
	}
	var streamAge time.Duration
	if last := s.LastStreamAt(id); !last.IsZero() {
		streamAge = time.Since(last)
	}
	if s.streamFreshWithin > 0 && streamAge > 0 && streamAge < s.streamFreshWithin {
		slog.Info("stream disconnect: recycle, poll suppressed", "vehicle", id, "stream_age_ms", streamAge.Milliseconds())
		return
	}
	slog.Info("stream disconnect poll", "vehicle", id, "stream_age_ms", streamAge.Milliseconds())
	s.pollTrigger(id)
}

// KnownIDs implements streamsource.RecordSink: the ids the cache expects.
func (s *Service) KnownIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.mergers))
	for id := range s.mergers {
		out[id] = true
	}
	return out
}

// Latest implements streamsink.CacheWatcher.
func (s *Service) Latest(id string) vehicle.Snapshot {
	if m := s.merger(id); m != nil {
		return m.Latest()
	}
	return vehicle.Snapshot{}
}

// Subscribe implements streamsink.CacheWatcher. Unknown id returns a closed
// channel (no events); a sink should reject the subscribe before reaching here.
func (s *Service) Subscribe(ctx context.Context, id string) <-chan struct{} {
	m := s.merger(id)
	if m == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return m.Subscribe(ctx)
}

// Vehicles implements streamsink.CacheWatcher.
func (s *Service) Vehicles() []vehicle.Identity { return s.identities }

// streamProvider is the sink.Provider adapter: Latest from the Merger (merged
// content), Stats delegated to the poll Manager. Built by Provider().
type streamProvider struct {
	svc *Service
	mgr *poll.Manager
}

func (p *streamProvider) Vehicles() []vehicle.Identity      { return p.svc.Vehicles() }
func (p *streamProvider) Latest(id string) vehicle.Snapshot { return p.svc.Latest(id) }
func (p *streamProvider) Stats(id string) poll.Stats {
	if p.mgr != nil {
		return p.mgr.Stats(id)
	}
	return poll.Stats{}
}

// Provider returns a sink.Provider that reads merged content from the Mergers and
// delegates poll health (Stats) to the Manager. serve swaps the REST sink onto
// this when streaming is active, so the REST sink serves merged snapshots while
// its health gauges still reflect the poll loop.
func (s *Service) Provider(mgr *poll.Manager) sink.Provider {
	return &streamProvider{svc: s, mgr: mgr}
}
