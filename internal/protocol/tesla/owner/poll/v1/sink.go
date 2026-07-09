package v1

// sink.go is the Tesla Owner API OUTPUT plugin. The Owner API output surface is
// the same Tesla /api/1 vehicle_data surface as the Fleet API, so the sink reuses
// tesla-fleet-poll-v1 end to end: teslafleet.BuildHandlerSampler constructs the idmap
// + cache adapter + chi server, and the canonical -> vehicle_data encode is
// shared (see sink_mapping.go). Only the registry name differs.

import (
	"log"
	"net/http"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"gopkg.in/yaml.v3"
)

// PluginName is the registry key for the Tesla Owner API output sink.
const PluginName = "tesla-owner-poll-v1"

// init self-registers the Tesla Owner output sink so a binary that blank-imports
// this package can select sink: tesla-owner-poll-v1 in config.
func init() {
	sink.Register(PluginName, newSink)
}

// ownerSink is the Tesla Owner implementation of sink.Sink. It wraps the shared
// Tesla server handler under the Owner registry name.
type ownerSink struct {
	handler http.Handler
	sample  func(canonID string) (*http.Request, error)
}

func newSink(node *yaml.Node, prov sink.Provider, logger *log.Logger) (sink.Sink, error) {
	var s teslafleet.Settings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, err
		}
	}
	h, sample, _, err := teslafleet.BuildHandlerSampler(prov, s, logger)
	if err != nil {
		return nil, err
	}
	return &ownerSink{handler: h, sample: sample}, nil
}

// Name implements sink.Sink.
func (o *ownerSink) Name() string { return PluginName }

// Handler implements sink.Sink: the shared Tesla chi server.
func (o *ownerSink) Handler() (http.Handler, error) { return o.handler, nil }

// SampleRequest implements sink.Sampler: GET vehicle_data for one served vehicle.
func (o *ownerSink) SampleRequest(canonID string) (*http.Request, error) {
	return o.sample(canonID)
}
