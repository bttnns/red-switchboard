package v1

// source.go is the Tesla Fleet API INPUT plugin: it reads a Tesla account's
// vehicles and polls one into the canonical vehicle.Snapshot. It composes the
// standardized resty client with a bearer token (Tesla Fleet auth is an OAuth
// access token in the Authorization header, unlike Rivian's CSRF/app-session
// pair) and decodes the {"response": ...} envelope each endpoint returns. The
// wire shapes and the imperial->SI decode live in source_mapping.go; the inverse
// encode (canonical->Tesla) is the sink half in sink_mapping.go.
//
// Errors are wrapped into a typed SourceError so the generic poll layer can
// classify them vendor-agnostically: HTTP 401 -> Unauthenticated, HTTP 429 ->
// RateLimited, 403 "account disabled: EXCEEDED_LIMIT" -> QuotaBlocked +
// TelemetryConfigWiped.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	teslaauth "github.com/bttnns/red-switchboard/internal/protocol/tesla/auth"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/transport/restclient"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/go-resty/resty/v2"
	"gopkg.in/yaml.v3"
)

// SourcePluginName is the registry key for the Tesla Fleet source plugin. The
// source and sink registries are separate maps, so this intentionally shares the
// "tesla-fleet-poll-v1" name with the sink (PluginName in sink.go): one protocol name,
// usable as either a source or a sink.
const SourcePluginName = PluginName

// teslaFleetHost is the default Tesla Fleet API base URL (North America region).
// Operators in other regions override base_url in config.
const teslaFleetHost = "https://fleet-api.prd.na.vn.cloud.tesla.com"

// init self-registers the Tesla Fleet source plugin so a binary that blank-
// imports this package can select source: tesla-fleet-poll-v1 in config.
func init() {
	source.Register(SourcePluginName, newSource)
}

// SourceSettings is the sources.tesla config sub-block. Defaults mirror rivian's
// Settings so a partial (or absent) block still works.
type SourceSettings struct {
	CredsFile string        `yaml:"creds_file"`
	BaseURL   string        `yaml:"base_url"`
	Timeout   time.Duration `yaml:"timeout"`
	Debug     bool          `yaml:"debug"`
}

func (s *SourceSettings) applyDefaults() {
	if s.CredsFile == "" {
		s.CredsFile = "/data/tesla.json"
	}
	if s.BaseURL == "" {
		s.BaseURL = teslaFleetHost
	}
	// Timeout is intentionally left zero when unset: restclient.New fills it from
	// the configured http.timeout default, so a per-source timeout here wins only
	// when explicitly set.
}

// FleetSource is the Tesla Fleet implementation of source.Source. It composes the
// standardized resty client with the central TokenManager, which serves (and
// refreshes) the OAuth bearer token applied per request.
type FleetSource struct {
	client   *resty.Client
	mgr      *teslaauth.TokenManager
	settings SourceSettings
	logger   *log.Logger
}

// newSource is the source.Factory for "tesla-fleet-poll-v1". It decodes the
// settings node, applies defaults, and joins the central TokenManager for the
// creds file (shared with the command plugin), which reads the creds eagerly so a
// missing token fails fast.
func newSource(node *yaml.Node, logger *log.Logger) (source.Source, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SourceSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("tesla: decode settings: %w", err)
		}
	}
	s.applyDefaults()

	mgr, err := teslaauth.Shared(s.CredsFile, logger)
	if err != nil {
		return nil, fmt.Errorf("tesla: %w (mint a Fleet access token first?)", err)
	}

	client := restclient.New(s.BaseURL, s.Timeout, s.Debug)
	return &FleetSource{client: client, mgr: mgr, settings: s, logger: logger}, nil
}

// Name implements source.Source.
func (s *FleetSource) Name() string { return SourcePluginName }

// Vehicles implements source.Source: GET /api/1/products, filtered to entries
// that carry a vehicle_id (TeslaMate's own rule for "this product is a car"),
// mapped into canonical identities. The Fleet API addresses a car by its VIN, so
// Identity.ID is the VIN.
func (s *FleetSource) Vehicles(ctx context.Context) ([]vehicle.Identity, error) {
	// /products mixes vehicles and energy sites; an energy site's `id` comes back
	// as a JSON string while a vehicle's is a number, so a strict []wire.Product
	// decode (int64 id) fails the whole list. Decode only the fields we need and
	// key off vehicle_id (TeslaMate's "this product is a car" rule).
	var env struct {
		Response []struct {
			VehicleID   int64  `json:"vehicle_id"`
			VIN         string `json:"vin"`
			DisplayName string `json:"display_name"`
		} `json:"response"`
	}
	if err := s.get(ctx, "/api/1/products", &env); err != nil {
		return nil, err
	}

	out := make([]vehicle.Identity, 0, len(env.Response))
	for _, p := range env.Response {
		if p.VehicleID == 0 { // not a vehicle (e.g. an energy site)
			continue
		}
		out = append(out, vehicle.Identity{
			ID:          p.VIN,
			VIN:         p.VIN,
			DisplayName: p.DisplayName,
			Make:        "Tesla",
		})
	}
	return out, nil
}

// Poll implements source.Source. It first reads the cheap per-vehicle summary
// (GET /api/1/vehicles/{id}), which reports online/asleep/offline WITHOUT waking
// the car, and only fetches the full vehicle_data when the car is online. This
// avoids waking a sleeping Tesla just to poll it (a wake is the most expensive
// Fleet operation, and constant wakes prevent the car from ever sleeping). id is
// the source-native id from Vehicles (the VIN).
func (s *FleetSource) Poll(ctx context.Context, id string) (vehicle.Snapshot, error) {
	var summ struct {
		Response wire.Summary `json:"response"`
	}
	if err := s.get(ctx, "/api/1/vehicles/"+id, &summ); err != nil {
		return vehicle.Snapshot{}, err
	}
	if summ.Response.State != "online" {
		// Asleep/offline: report liveness from the summary; do NOT call vehicle_data.
		return SummaryToSnapshot(summ.Response), nil
	}

	var env struct {
		Response wire.VehicleData `json:"response"`
	}
	// Tesla only includes GPS (drive_state.latitude/longitude) when the
	// location_data endpoint is explicitly requested AND the token has the
	// vehicle_location scope; the default vehicle_data response omits it, so
	// location is otherwise always null. %3B is the URL-encoded ";" separator.
	const vehicleDataEndpoints = "?endpoints=location_data%3Bcharge_state%3Bclimate_state%3Bdrive_state%3Bgui_settings%3Bvehicle_config%3Bvehicle_state"
	if err := s.get(ctx, "/api/1/vehicles/"+id+"/vehicle_data"+vehicleDataEndpoints, &env); err != nil {
		return vehicle.Snapshot{}, err
	}
	snap := DecodeVehicleData(&env.Response)
	snap.FetchedAt = time.Now()
	return snap, nil
}

// SourceError wraps a Tesla HTTP failure with the vendor-agnostic
// classification the generic poll layer branches on: 401 -> Unauthenticated,
// 429 -> RateLimited, and the 403 "account disabled: EXCEEDED_LIMIT" family ->
// QuotaBlocked + TelemetryConfigWiped. It also carries any server-specified
// Retry-After/RateLimit-Reset backoff. The Owner source reuses it (the two
// plugins share the transport shape and the bug, so they share the fix),
// mirroring the existing LoadCreds/ParseRetryAfter reuse. Label prefixes the
// Error() message so fleet and owner failures stay distinguishable in logs.
type SourceError struct {
	Label          string
	Status         int
	Body           string // response body, parsed for 403 classification
	Err            error
	Retry          time.Duration
	HasRetry       bool
	TelemetryWiped bool // EXCEEDED_LIMIT implies the fleet_telemetry_config was wiped
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("%s error (status=%d): %v", e.Label, e.Status, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }

// Unauthenticated reports whether this is an auth failure (HTTP 401) the poll
// layer should treat as needing re-login.
func (e *SourceError) Unauthenticated() bool { return e.Status == http.StatusUnauthorized }

// RateLimited reports whether this is a rate-limit (HTTP 429) the poll layer
// should back off on.
func (e *SourceError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// RetryAfter returns the server-specified backoff parsed from the response, if any.
func (e *SourceError) RetryAfter() (time.Duration, bool) { return e.Retry, e.HasRetry }

// QuotaBlocked reports a billing/quota block: a 403 whose body marks the
// account disabled (Tesla's "account disabled: EXCEEDED_LIMIT" family). These
// clear only when the cap is raised or the cycle resets (hours to days), NOT a
// transient rate-limit, so the poll layer backs off near-fixed rather than
// exponentially. A 403 for an unreachable car ("vehicle unavailable" etc.) is
// NOT a quota block and falls through to the generic error path.
func (e *SourceError) QuotaBlocked() bool {
	return e.Status == http.StatusForbidden && strings.Contains(e.Body, "account disabled")
}

// AccountDisabled mirrors QuotaBlocked: today every Tesla quota block is also an
// account-disabled block. Surfaced separately so a future vendor whose quota and
// account states diverge can classify them independently.
func (e *SourceError) AccountDisabled() bool { return e.QuotaBlocked() }

// TelemetryConfigWiped reports whether hitting the billing cap wiped the
// vendor's push-telemetry config (Tesla: EXCEEDED_LIMIT REMOVES
// fleet_telemetry_config and does NOT auto-restore it). The operator must
// re-pair/reconfigure after the cap is raised, not just restart.
func (e *SourceError) TelemetryConfigWiped() bool { return e.TelemetryWiped }

// NewSourceError builds a SourceError from a non-2xx resty response, parsing the
// body for 403 classification and any Retry-After/RateLimit-Reset backoff.
// Exported so the Owner source reuses the same classification.
func NewSourceError(label string, resp *resty.Response) *SourceError {
	body := resp.String()
	e := &SourceError{
		Label:  label,
		Status: resp.StatusCode(),
		Body:   body,
		Err:    fmt.Errorf("%s: %s", resp.Status(), body),
	}
	e.Retry, e.HasRetry = ParseRetryAfter(resp.Header())
	if e.Status == http.StatusForbidden && strings.Contains(body, "EXCEEDED_LIMIT") {
		e.TelemetryWiped = true
	}
	return e
}

// get issues a GET, decoding a 2xx body into out. The bearer is read per request
// from the central TokenManager; on a 401 it refreshes the token once and retries
// (satisfying the poll layer's "source already retried its own re-auth once"
// contract). A persistent non-2xx becomes a typed SourceError carrying the status
// (so 401/429/403 classify for the poll layer) and any Retry-After/RateLimit-Reset
// backoff; a transport failure passes through.
func (s *FleetSource) get(ctx context.Context, path string, out any) error {
	return s.getWithRefresh(ctx, path, out, true)
}

func (s *FleetSource) getWithRefresh(ctx context.Context, path string, out any, retry bool) error {
	tok, err := s.mgr.Token(ctx)
	if err != nil {
		return err
	}
	resp, err := s.client.R().SetContext(ctx).SetAuthToken(tok).SetResult(out).Get(path)
	if err != nil {
		return err
	}
	if resp.IsError() {
		srcErr := NewSourceError("tesla fleet api", resp)
		if retry && srcErr.Unauthenticated() {
			if newTok, rerr := s.mgr.RefreshAfter401(ctx, tok); rerr == nil && newTok != tok {
				return s.getWithRefresh(ctx, path, out, false)
			}
		}
		return srcErr
	}
	return nil
}

// ParseRetryAfter extracts a backoff hint from a throttled response: the standard
// Retry-After header (delta-seconds or HTTP-date) or Tesla's RateLimit-Reset (unix
// epoch seconds). It returns false when neither is present or parseable. Exported
// so the Owner source (which shares Tesla's response shape) can reuse it.
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return clampNonNeg(time.Duration(secs) * time.Second), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return clampNonNeg(time.Until(t)), true
		}
	}
	if v := strings.TrimSpace(h.Get("RateLimit-Reset")); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			return clampNonNeg(time.Until(time.Unix(epoch, 0))), true
		}
	}
	return 0, false
}

func clampNonNeg(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
