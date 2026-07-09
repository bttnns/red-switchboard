package v1

// source.go is the Tesla Owner API INPUT plugin: the legacy owner-api
// (owner-api.teslamotors.com), the predecessor of the Fleet API. It returns the
// same vehicle_data shape for the fields redswitchboard reads, so it reuses
// tesla-fleet-poll-v1's wire structs, creds loader, and DecodeVehicleData mapping
// (see source_mapping.go). Only the transport is Owner-specific: the host, the
// GET /api/1/vehicles list endpoint (Fleet uses /api/1/products), and addressing
// a car by its numeric id (Fleet uses the VIN). Outbound HTTP uses the
// standardized resty client; the OAuth bearer token is the owner-api token.

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	teslaauth "github.com/bttnns/red-switchboard/internal/protocol/tesla/auth"
	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/transport/restclient"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/go-resty/resty/v2"
	"gopkg.in/yaml.v3"
)

// SourcePluginName is the registry key for the Tesla Owner source plugin. The
// source and sink registries are separate maps, so it shares the "tesla-owner-poll-v1"
// name with the sink (PluginName in sink.go).
const SourcePluginName = PluginName

// ownerHost is the default Tesla Owner API base URL.
const ownerHost = "https://owner-api.teslamotors.com"

// init self-registers the Tesla Owner source plugin so a binary that blank-
// imports this package can select source: tesla-owner-poll-v1 in config.
func init() {
	source.Register(SourcePluginName, newSource)
}

// SourceSettings is the sources.tesla-owner config sub-block.
type SourceSettings struct {
	CredsFile string        `yaml:"creds_file"`
	BaseURL   string        `yaml:"base_url"`
	Timeout   time.Duration `yaml:"timeout"`
	Debug     bool          `yaml:"debug"`
}

func (s *SourceSettings) applyDefaults() {
	if s.CredsFile == "" {
		s.CredsFile = "/data/tesla-owner.json"
	}
	if s.BaseURL == "" {
		s.BaseURL = ownerHost
	}
	// Timeout left zero when unset: restclient.New fills it from the configured
	// http.timeout default, so a per-source timeout wins only when set.
}

// OwnerSource is the Tesla Owner implementation of source.Source.
type OwnerSource struct {
	client   *resty.Client
	mgr      *teslaauth.TokenManager
	settings SourceSettings
	logger   *log.Logger
}

// newSource is the source.Factory for "tesla-owner-poll-v1". It joins the central
// TokenManager for the creds file (shared with the Owner stream source); the token
// file shape is the same as the Fleet source's.
func newSource(node *yaml.Node, logger *log.Logger) (source.Source, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SourceSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("tesla-owner: decode settings: %w", err)
		}
	}
	s.applyDefaults()

	mgr, err := teslaauth.Shared(s.CredsFile, logger)
	if err != nil {
		return nil, fmt.Errorf("tesla-owner: %w (mint an owner-api access token first?)", err)
	}
	client := restclient.New(s.BaseURL, s.Timeout, s.Debug)
	return &OwnerSource{client: client, mgr: mgr, settings: s, logger: logger}, nil
}

// Name implements source.Source.
func (s *OwnerSource) Name() string { return SourcePluginName }

// Vehicles implements source.Source: GET /api/1/vehicles, mapped into canonical
// identities. The Owner API addresses a car by its numeric id, so Identity.ID is
// that id rendered as a string.
func (s *OwnerSource) Vehicles(ctx context.Context) ([]vehicle.Identity, error) {
	var env struct {
		Response []wire.Summary `json:"response"`
	}
	if err := s.get(ctx, "/api/1/vehicles", &env); err != nil {
		return nil, err
	}
	out := make([]vehicle.Identity, 0, len(env.Response))
	for _, v := range env.Response {
		out = append(out, vehicle.Identity{
			ID:          strconv.FormatInt(v.ID, 10),
			VIN:         v.VIN,
			DisplayName: v.DisplayName,
			Make:        "Tesla",
		})
	}
	return out, nil
}

// Poll implements source.Source. Like the Fleet source it first reads the cheap
// per-vehicle summary (GET /api/1/vehicles/{id}), which reports online/asleep/
// offline WITHOUT waking the car, and only fetches vehicle_data when online, so a
// sleeping car is never woken just to be polled. id is the numeric id from Vehicles.
func (s *OwnerSource) Poll(ctx context.Context, id string) (vehicle.Snapshot, error) {
	var summ struct {
		Response wire.Summary `json:"response"`
	}
	if err := s.get(ctx, "/api/1/vehicles/"+id, &summ); err != nil {
		return vehicle.Snapshot{}, err
	}
	if summ.Response.State != "online" {
		return teslafleet.SummaryToSnapshot(summ.Response), nil
	}

	var env struct {
		Response wire.VehicleData `json:"response"`
	}
	if err := s.get(ctx, "/api/1/vehicles/"+id+"/vehicle_data", &env); err != nil {
		return vehicle.Snapshot{}, err
	}
	snap := decodeVehicleData(&env.Response)
	snap.FetchedAt = time.Now()
	return snap, nil
}

// get issues a GET, decoding a 2xx body into out. The bearer is read per request
// from the central TokenManager; on a 401 it refreshes once and retries (the poll
// layer's "source already retried its own re-auth once" contract). A persistent
// non-2xx becomes a typed SourceError (reused from tesla-fleet-poll-v1) carrying
// the status (so 401/429/403 classify for the poll layer) and any server-specified
// Retry-After backoff; a transport failure passes through unchanged.
func (s *OwnerSource) get(ctx context.Context, path string, out any) error {
	return s.getWithRefresh(ctx, path, out, true)
}

func (s *OwnerSource) getWithRefresh(ctx context.Context, path string, out any, retry bool) error {
	tok, err := s.mgr.Token(ctx)
	if err != nil {
		return err
	}
	resp, err := s.client.R().SetContext(ctx).SetAuthToken(tok).SetResult(out).Get(path)
	if err != nil {
		return err
	}
	if resp.IsError() {
		srcErr := teslafleet.NewSourceError("tesla owner api", resp)
		if retry && srcErr.Unauthenticated() {
			if newTok, rerr := s.mgr.RefreshAfter401(ctx, tok); rerr == nil && newTok != tok {
				return s.getWithRefresh(ctx, path, out, false)
			}
		}
		return srcErr
	}
	return nil
}
