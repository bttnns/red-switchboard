package v1

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/plugin/sink/idmap"
	"gopkg.in/yaml.v3"
)

// sampler builds a SampleRequest from a resolved adapter, shared by the Tesla
// Fleet and Owner sinks (both serve the same vehicle_data surface). An empty
// canonID picks the first served vehicle.
func sampler(adapter *sourceAdapter) func(canonID string) (*http.Request, error) {
	return func(canonID string) (*http.Request, error) {
		id, err := adapter.teslaID(canonID)
		if err != nil {
			return nil, fmt.Errorf("tesla: sample request: %w", err)
		}
		return http.NewRequest(http.MethodGet, fmt.Sprintf("/api/1/vehicles/%d/vehicle_data", id), nil)
	}
}

// PluginName is the registry key for the Tesla Fleet API output sink.
const PluginName = "tesla-fleet-poll-v1"

// init self-registers the Tesla output sink so a binary that blank-imports this
// package can select sink: tesla-fleet-poll-v1 in config.
func init() {
	sink.Register(PluginName, newSink)
}

// Settings is the sinks.tesla config sub-block.
type Settings struct {
	// ProviderToken is the qts- token value reported to TeslaMate (cosmetic).
	ProviderToken string `yaml:"provider_token"`
	// IDMapFile persists the canonical-id -> synthetic int64 id mapping. Empty
	// means in-memory only (still stable, derived by hash).
	IDMapFile string `yaml:"idmap_file"`
	// AuthToken, when set, requires consumers to send "Authorization: Bearer
	// <AuthToken>" on the live-data read routes (P14). Empty (the default) leaves
	// the surface unauthenticated, exactly as before, so enabling auth is opt-in
	// and must be configured on both this sink and the consumer (TeslaMate).
	AuthToken string `yaml:"auth_token"`
}

// wiring holds cross-cutting inputs the sink needs from the wiring layer (the
// CLI), set before Open. Keeping them here lets the sink.Factory signature stay
// generic (settings + provider) while still receiving the global stale-after and
// the source's per-VIN car_type placeholder.
var wiring struct {
	carType    func(vin string) string
	staleAfter time.Duration
}

// SetCarTypeResolver registers the per-VIN car_type resolver used when building
// vehicle_config (vehicle_config must carry a car_type or the FSM crashes).
func SetCarTypeResolver(f func(vin string) string) { wiring.carType = f }

// SetStaleAfter sets the cache age past which a vehicle degrades to offline
// (from the global poll.stale_after).
func SetStaleAfter(d time.Duration) { wiring.staleAfter = d }

// teslaSink is the Tesla implementation of sink.Sink.
type teslaSink struct {
	name      string
	handler   http.Handler
	sample    func(canonID string) (*http.Request, error)
	unchanged func() int64 // P7 unchanged-reads count, surfaced for the metrics registry
}

func newSink(node *yaml.Node, prov sink.Provider, logger *log.Logger) (sink.Sink, error) {
	var s Settings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("tesla: decode settings: %w", err)
		}
	}
	h, sample, unchanged, err := BuildHandlerSampler(prov, s, logger)
	if err != nil {
		return nil, err
	}
	return &teslaSink{name: PluginName, handler: h, sample: sample, unchanged: unchanged}, nil
}

// BuildHandlerSampler builds the Tesla API HTTP handler (idmap + cache adapter +
// chi server) and a matching sink.Sampler closure from the same resolved adapter
// (so the sampled vehicle_data request routes to a real served vehicle). It is
// exported so the tesla-owner-poll-v1 sink, which impersonates the same Tesla
// vehicle_data surface (the Owner API is Fleet's predecessor and shares the
// shape), can reuse the whole server under its own registry name instead of
// duplicating it. It reads the cross-cutting wiring (stale-after, car_type
// resolver) set by the CLI via SetStaleAfter / SetCarTypeResolver.
func BuildHandlerSampler(prov sink.Provider, s Settings, logger *log.Logger) (http.Handler, func(canonID string) (*http.Request, error), func() int64, error) {
	if logger == nil {
		logger = log.Default()
	}
	ids, err := idmap.New(s.IDMapFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tesla: idmap: %w", err)
	}
	adapter, err := newSourceAdapter(prov, ids, wiring.staleAfter, wiring.carType, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("tesla: %w", err)
	}
	return newServer(adapter, s.AuthToken, logger).Handler(), sampler(adapter), adapter.UnchangedReads, nil
}

// Name implements sink.Sink.
func (t *teslaSink) Name() string { return t.name }

// Handler implements sink.Sink: the chi router serving the Tesla API surface.
func (t *teslaSink) Handler() (http.Handler, error) { return t.handler, nil }

// SampleRequest implements sink.Sampler: GET vehicle_data for one served vehicle.
func (t *teslaSink) SampleRequest(canonID string) (*http.Request, error) {
	return t.sample(canonID)
}

// UnchangedReads reports vehicle_data reads served from the AsOf fast path (P7):
// the spinning-consumer signal, surfaced for the metrics registry.
func (t *teslaSink) UnchangedReads() int64 { return t.unchanged() }
