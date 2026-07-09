// Package streamsource is the streaming INPUT seam of the hub: the push analogue
// of internal/source. A StreamSource does not fit the pull source.Source interface
// (Poll is on-demand; a stream is a long-lived listener or dialer), so it gets its
// own seam and registry, following the same database/sql driver pattern. A
// StreamSource owns process-lifetime components (an inbound mTLS listener, or an
// outbound dialer per awake vehicle) and writes canonical snapshots into the cache
// asynchronously, not on a poll tick.
//
// This package is VENDOR-AGNOSTIC: it imports only internal/vehicle and the
// stdlib, never any internal/protocol/* package. Tesla Fleet Telemetry, Tesla
// Owner streaming, and a future Rivian streaming source are all plugins against
// this one interface; the inbound-vs-outbound direction is a plugin property, not
// a seam split. Adding a make's streaming source must not require editing this
// package (only registering a new plugin from that make's package).
package streamsource

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// RecordSink is where a StreamSource delivers decoded canonical snapshots. It is
// implemented by the cache writer (internal/stream merges pushed records with
// polled ones). Decoupling the delivery target from the cache internals keeps the
// streamsource seam cache-agnostic, mirroring how source.Source returns a Snapshot
// and the poll loop owns the cache.
type RecordSink interface {
	// Put accepts a decoded snapshot for one vehicle, keyed by source-native id.
	// Put takes only the per-vehicle Merger mutex for the duration of the merge +
	// a non-blocking subscriber fan-out, so it is bounded and never blocks on a
	// slow consumer. The streamsource must never stall the vehicle-facing
	// connection on cache work, which this bound guarantees.
	Put(ctx context.Context, id string, snap vehicle.Snapshot) error
	// KnownIDs returns the ids the cache expects (from source.Vehicles), so an
	// inbound-push source can reject an unknown vehicle before it invests in a
	// session. A rogue vehicle must not fill the cache.
	KnownIDs() map[string]bool
}

// StreamDisconnectNotifier is an optional capability a RecordSink may implement.
// When a vehicle's stream connection drops, the source calls OnStreamDisconnect so
// the cache can fire an immediate poll: without it, a stream that disconnects
// mid-drive (without sending a GearPark frame) leaves a stale driving state in the
// cache until the next scheduled poll, which may arrive after TeslaMate has already
// stopped querying and gone quiet -- causing an unclosed drive session.
type StreamDisconnectNotifier interface {
	OnStreamDisconnect(ctx context.Context, id string)
}

// CacheWatcher is the read + change-notification view an OUTBOUND-DIALER source
// needs to drive connect/disconnect off the cache's derived state (the Owner
// dialer must connect only while a car is awake). It is passed to Run alongside
// the RecordSink and implemented by internal/cache.Service. Inbound-push sources
// (Fleet Telemetry) ignore it: the car dials them, so they never read the cache.
// Declaring it in the seam makes a dialer source's dependency visible instead of
// recovered by a runtime type assertion.
type CacheWatcher interface {
	Latest(id string) vehicle.Snapshot
	Subscribe(ctx context.Context, id string) <-chan struct{}
	Vehicles() []vehicle.Identity
}

// StreamSource is a push input plugin. Run owns process-lifetime components and
// writes snapshots into sink until ctx is cancelled. It must be safe to Run once.
type StreamSource interface {
	// Name is the registry key (e.g. "tesla-fleet-stream-v1").
	Name() string
	// Vehicles returns the account's vehicles (same shape as source.Source, so the
	// CLI can enumerate identities once and start the poll loop as a gap-filler
	// alongside the stream). May return an empty list if pairing is async and the
	// source learns VINs only as vehicles connect; the stream still runs.
	Vehicles(ctx context.Context) ([]vehicle.Identity, error)
	// Run starts the listener/dialer. It blocks until ctx is cancelled; it must
	// drain in-flight frames and close all connections on cancellation. sink is
	// where decoded snapshots go; watch is the cache read view an outbound dialer
	// uses to follow vehicle state (nil/ignored for inbound-push sources).
	Run(ctx context.Context, sink RecordSink, watch CacheWatcher) error
}

// Factory constructs a StreamSource from its plugin-specific settings node and a
// logger. settings may be nil.
type Factory func(settings *yaml.Node, logger *log.Logger) (StreamSource, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a streaming source plugin under name. Plugins call this from
// init(). A duplicate name or nil factory panics (a programming error).
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("streamsource: Register factory is nil")
	}
	if _, dup := factories[name]; dup {
		panic("streamsource: Register called twice for " + name)
	}
	factories[name] = f
}

// Open constructs the registered streaming source plugin named name.
func Open(name string, settings *yaml.Node, logger *log.Logger) (StreamSource, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("streamsource: unknown plugin %q (registered: %v)", name, Names())
	}
	return f(settings, logger)
}

// Names returns the registered streaming source names, sorted.
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
