// Package streamsink is the streaming OUTPUT seam of the hub: the push analogue
// of internal/sink. A StreamSink holds a long-lived outbound WebSocket per
// connected consumer and pushes canonical snapshots read from the cache, on cache
// change, encoded in the consumer's streaming wire shape. It does not fit the
// request/response sink.Sink interface (Handler answers one request), so it gets
// its own seam and registry, following the same database/sql driver pattern.
//
// This package is VENDOR-AGNOSTIC: it imports only internal/vehicle and the
// stdlib, never any internal/protocol/* package. The tesla-fleet-stream-v1 sink
// (serving TeslaMate's data:update shape) is the first plugin; a future sink
// serving a different consumer's streaming shape registers the same way. The
// hub's N+M rule applies: any streaming source feeds any streaming sink through
// the canonical model.
package streamsink

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// CacheWatcher is the read + change-notification view a StreamSink reads. It is
// implemented by the cache wiring (internal/cache.Service). Subscribe returns a
// channel of cache-change events for the given vehicle (closed when ctx ends);
// Latest reads the current snapshot. The sink uses Subscribe to know WHEN to push
// and Latest to know WHAT.
type CacheWatcher interface {
	// Latest returns the current cached snapshot for a vehicle (zero value if
	// unknown). Same as sink.Provider.Latest.
	Latest(id string) vehicle.Snapshot
	// Subscribe returns a channel that receives an event every time the cached
	// snapshot for id changes (by poll OR by stream). The channel is bounded; a
	// slow subscriber that does not drain is dropped, never blocks the cache
	// writer. Closes when ctx is done.
	Subscribe(ctx context.Context, id string) <-chan struct{}
	// Vehicles returns the served identities (same as sink.Provider.Vehicles).
	Vehicles() []vehicle.Identity
}

// CacheReplayer is the OPTIONAL replay-buffer capability (J2): a watcher that also
// holds a bounded on-disk ring of recent merged snapshots exposes Replay so a sink
// can re-emit the frames buffered during a restart to a reconnecting consumer,
// closing the mid-drive gap. A sink type-asserts its watcher to this; a watcher
// without the ring (or with the buffer disabled) returns nil and the sink falls
// straight through to the live loop.
type CacheReplayer interface {
	// Replay returns the buffered merged snapshots for a vehicle, oldest first
	// (nil when the buffer is disabled or the vehicle has no history).
	Replay(id string) []vehicle.Snapshot
}

// StreamSink is a push output plugin. Run owns its own listener (its own port +
// optional TLS), separate from the internal REST server, because the streaming
// sink is consumer-facing and serves wss:// while the REST sink stays internal
// plain-HTTP. Handler returns the WS handler (for tests / httptest); in
// production Run mounts it at /streaming/ inside its own listener.
type StreamSink interface {
	// Name is the registry key (e.g. "tesla-fleet-stream-v1").
	Name() string
	// Handler returns the http.Handler that upgrades to the streaming WS and
	// drives the per-connection broadcaster. Exists mainly for tests; in
	// production Run mounts it on its own listener.
	Handler() (http.Handler, error)
	// Run binds the sink's own HTTP listener (configured addr + optional TLS),
	// mounts Handler() at /streaming/, and serves until ctx is cancelled, then
	// shuts the listener down. Blocks until ctx is cancelled.
	Run(ctx context.Context) error
}

// Factory constructs a StreamSink from its settings node, the CacheWatcher, and a
// logger. settings may be nil.
type Factory func(settings *yaml.Node, w CacheWatcher, logger *log.Logger) (StreamSink, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a streaming sink plugin under name. Plugins call this from init().
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("streamsink: Register factory is nil")
	}
	if _, dup := factories[name]; dup {
		panic("streamsink: Register called twice for " + name)
	}
	factories[name] = f
}

// Open constructs the registered streaming sink plugin named name.
func Open(name string, settings *yaml.Node, w CacheWatcher, logger *log.Logger) (StreamSink, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("streamsink: unknown plugin %q (registered: %v)", name, Names())
	}
	return f(settings, w, logger)
}

// Names returns the registered streaming sink names, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
