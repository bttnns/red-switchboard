// Package sink is the OUTPUT seam of the hub: it defines the Sink interface that
// every output plugin (Tesla Fleet API now; others later) implements, plus a
// registry following the database/sql driver pattern. A plugin self-registers in
// its init() with Register(name, factory); the binary blank-imports the plugins
// it ships; the active plugin is selected by config and constructed with
// Open(name, settings, provider, logger).
//
// A Sink reads the canonical model through the Provider (what the poll cache
// exposes) and serves it in some external API's shape, so each output is
// independent of which input source produced the data.
package sink

import (
	"fmt"
	"iter"
	"log"
	"net/http"
	"sort"
	"sync"

	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// Provider is the canonical-model view a Sink reads. It is implemented by the
// poll cache wiring: vehicle identities, the latest cached snapshot per vehicle,
// and poll health stats.
type Provider interface {
	// Vehicles returns the canonical identities of every served vehicle.
	Vehicles() []vehicle.Identity
	// Latest returns the most recent cached snapshot for a vehicle (zero value
	// if unknown / not yet polled).
	Latest(id string) vehicle.Snapshot
	// Stats returns the poll health for a vehicle.
	Stats(id string) poll.Stats
}

// Sink is an output plugin. It builds the HTTP handler that serves the canonical
// data (read through the Provider) in its external API shape.
type Sink interface {
	// Name is the registry key (e.g. "tesla-fleet-poll-v1").
	Name() string
	// Handler returns the HTTP handler serving the sink's API surface.
	Handler() (http.Handler, error)
}

// Sampler is an optional Sink capability: it returns a sample HTTP request that
// exercises the sink's primary vehicle-data endpoint for one canonical vehicle,
// so tooling (the `show` command) can render the protocol's API shape without
// hardcoding per-protocol routes. Sinks that can be introspected implement it.
// canonID selects the vehicle by its canonical (source-native) id; an empty
// canonID means the first served vehicle.
type Sampler interface {
	SampleRequest(canonID string) (*http.Request, error)
}

// HistorySample is one timestamped canonical snapshot for a vehicle, the unit an
// Exporter consumes. CanonID is the vehicle's source-native id; Snap.FetchedAt
// carries the moment it represents.
type HistorySample struct {
	CanonID string
	Snap    vehicle.Snapshot
}

// Exporter is an optional Sink capability: write a time-ordered stream of
// canonical snapshots in the sink's batch/file format (e.g. TeslaFi CSV monthly
// files) instead of serving them over HTTP. The destination (e.g. export_dir) is
// the sink's own configuration, not a call argument. samples is a pull iterator
// so a long window never has to materialize in memory.
type Exporter interface {
	ExportHistory(samples iter.Seq[HistorySample]) error
}

// Factory constructs a Sink from its plugin-specific settings (the YAML node
// under sinks.<name>), the Provider, and a logger. settings may be nil; the
// plugin must apply its own defaults.
type Factory func(settings *yaml.Node, p Provider, logger *log.Logger) (Sink, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a sink plugin factory under name. Plugins call this from init().
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("sink: Register factory is nil")
	}
	if _, dup := factories[name]; dup {
		panic("sink: Register called twice for " + name)
	}
	factories[name] = f
}

// Open constructs the registered sink plugin named name.
func Open(name string, settings *yaml.Node, p Provider, logger *log.Logger) (Sink, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sink: unknown plugin %q (registered: %v)", name, Names())
	}
	return f(settings, p, logger)
}

// Names returns the registered sink plugin names, sorted.
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
