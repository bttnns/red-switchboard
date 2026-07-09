package v1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gopkg.in/yaml.v3"
)

// SinkPluginName is the registry key for the Rivian GraphQL output sink. It is the
// same string as the source PluginName, but the sink registry is a SEPARATE map
// (internal/plugin/sink vs internal/plugin/source), so registering it here does not collide.
const SinkPluginName = "rivian-graphql-poll-v1"

// init self-registers the Rivian output sink so a binary that blank-imports this
// package can select sink: rivian-graphql-poll-v1 in config.
func init() {
	sink.Register(SinkPluginName, newSink)
}

// SinkSettings is the sinks.rivian config sub-block. base_path is the GraphQL API
// root the sink serves under (the inverse of the source's base_url), so a Rivian
// client can be pointed straight at it.
type SinkSettings struct {
	BasePath string `yaml:"base_path"`
}

func (s *SinkSettings) applyDefaults() {
	if s.BasePath == "" {
		s.BasePath = "/api/gql"
	}
	s.BasePath = "/" + strings.Trim(s.BasePath, "/")
}

// rivianSink is the Rivian implementation of sink.Sink. It serves the gateway
// vehicleState and charging getLiveSessionData GraphQL endpoints, rendering the
// canonical snapshots it reads from the Provider into Rivian wire shapes (the
// inverse of the source client that parses them).
type rivianSink struct {
	prov     sink.Provider
	settings SinkSettings
	logger   *log.Logger
}

// newSink is the sink.Factory for "rivian-graphql-poll-v1".
func newSink(node *yaml.Node, prov sink.Provider, logger *log.Logger) (sink.Sink, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SinkSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("rivian sink: decode settings: %w", err)
		}
	}
	s.applyDefaults()
	return &rivianSink{prov: prov, settings: s, logger: logger}, nil
}

// Name implements sink.Sink.
func (s *rivianSink) Name() string { return SinkPluginName }

// SampleRequest implements sink.Sampler: a POST to the gateway GraphQL endpoint
// running GetVehicleState (the op whose response carries vehicle state), built
// with the same request body the handler routes on. An empty canonID picks the
// first served vehicle.
func (s *rivianSink) SampleRequest(canonID string) (*http.Request, error) {
	if canonID == "" {
		vs := s.prov.Vehicles()
		if len(vs) == 0 {
			return nil, fmt.Errorf("rivian sink: no vehicles to sample")
		}
		canonID = vs[0].ID
	}
	body := graphQLRequest{
		OperationName: "GetVehicleState",
		Variables:     map[string]any{"vehicleID": canonID},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, s.settings.BasePath+"/gateway/graphql", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// Handler implements sink.Sink: a chi router serving the Rivian GraphQL gateway
// and charging endpoints, dispatched by operation name (mirroring the mock
// server's dispatch).
func (s *rivianSink) Handler() (http.Handler, error) {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Post(s.settings.BasePath+"/gateway/graphql", s.handleGateway)
	r.Post(s.settings.BasePath+"/chrg/user/graphql", s.handleCharging)
	return r, nil
}

// graphQLRequest is the body shape every Rivian call POSTs.
type graphQLRequest struct {
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func (s *rivianSink) handleGateway(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	switch req.OperationName {
	case "GetVehicleState":
		guid, _ := req.Variables["vehicleID"].(string)
		snap, found := s.lookup(guid)
		if !found {
			writeGQLJSON(w, gqlError("BAD_USER_INPUT", "unknown vehicle id"))
			return
		}
		writeGQLJSON(w, vehicleStateBody(snap.State))

	case "getUserInfo":
		writeGQLJSON(w, s.userInfoBody())

	default:
		writeGQLJSON(w, gqlError("BAD_USER_INPUT", "unsupported operation: "+req.OperationName))
	}
}

func (s *rivianSink) handleCharging(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	if req.OperationName != "getLiveSessionData" {
		writeGQLJSON(w, gqlError("BAD_USER_INPUT", "unsupported operation: "+req.OperationName))
		return
	}
	guid, _ := req.Variables["vehicleId"].(string)
	snap, found := s.lookup(guid)
	if !found {
		writeGQLJSON(w, gqlError("BAD_USER_INPUT", "unknown vehicle id"))
		return
	}
	writeGQLJSON(w, liveSessionBody(snap.Live, liveStamp(snap)))
}

// lookup resolves a vehicle GUID to its latest cached snapshot, reporting whether
// the GUID is a known served vehicle.
func (s *rivianSink) lookup(guid string) (vehicle.Snapshot, bool) {
	if guid == "" {
		return vehicle.Snapshot{}, false
	}
	for _, v := range s.prov.Vehicles() {
		if v.ID == guid {
			return s.prov.Latest(guid), true
		}
	}
	return vehicle.Snapshot{}, false
}

// userInfoBody renders the account vehicle list in the getUserInfo shape so a
// Rivian client can discover the served vehicles (id/name/vin), mirroring the
// mock server's getUserInfo.
func (s *rivianSink) userInfoBody() map[string]any {
	vehicles := make([]map[string]any, 0)
	for _, v := range s.prov.Vehicles() {
		vehicles = append(vehicles, map[string]any{"id": v.ID, "name": v.DisplayName, "vin": v.VIN})
	}
	return map[string]any{"data": map[string]any{"currentUser": map[string]any{"vehicles": vehicles}}}
}

// liveStamp picks the timestamp to stamp live-session records with: the snapshot's
// state LastUpdate when present, else the fetch time.
func liveStamp(snap vehicle.Snapshot) time.Time {
	if snap.State != nil && !snap.State.LastUpdate.IsZero() {
		return snap.State.LastUpdate
	}
	return snap.FetchedAt
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (graphQLRequest, bool) {
	var req graphQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGQLJSON(w, gqlError("BAD_USER_INPUT", "invalid request body"))
		return graphQLRequest{}, false
	}
	return req, true
}

// gqlError builds a GraphQL error envelope (HTTP 200 with errors[], matching how
// the real Rivian API surfaces errors).
func gqlError(code, message string) map[string]any {
	return map[string]any{
		"data": nil,
		"errors": []map[string]any{{
			"message":    message,
			"extensions": map[string]any{"code": code},
		}},
	}
}

func writeGQLJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
