// Package tesla is the Tesla Fleet API output sink. It exposes the minimal Fleet
// API surface that stock TeslaMate (and any other Fleet API consumer) polls: a
// token endpoint plus products, a per-vehicle summary, and vehicle_data. Routing
// is handled by chi; the car is resolved by the {id} path segment. Responses
// follow the real Fleet API shapes (including the {response,error,
// error_description} error envelope) so the service is a faithful translator,
// not a TeslaMate-only shim.
//
// It reads the canonical model through sink.Provider, translates it into the
// Tesla wire shape (subpackage translate -> wire), and mints synthetic integer
// ids (subpackage idmap, since Tesla specifically needs int64 ids). The plugin
// registers itself as sink "tesla-fleet-poll-v1" in register.go.
package v1

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server holds the configured data source and HTTP routing.
type Server struct {
	source VehicleDataSource
	logger *log.Logger

	// authToken, when non-empty, gates the live-data read routes: a request must
	// carry "Authorization: Bearer <authToken>" or it gets 401 (P14). Empty means
	// no auth (the historical behavior), so an unconfigured deploy is unchanged.
	authToken string

	// Introspection (registered only when the source supports it).
	intro     Introspector
	startedAt time.Time
	counters  *requestCounters
}

// newServer builds a Server serving the Tesla Fleet API surface from the given
// source. The caller owns the *http.Server (so it can manage graceful shutdown)
// and wires in Handler().
func newServer(source VehicleDataSource, authToken string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		source:    source,
		authToken: authToken,
		logger:    logger,
		startedAt: time.Now(),
		counters:  newRequestCounters(),
	}
}

// Handler returns the chi router with recovery + request logging applied.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.logRequests)

	// The OAuth token endpoint stays open: it is the bootstrap a consumer hits to
	// obtain the bearer it then sends, so gating it would deadlock sign-in.
	r.Post("/api/oauth2/v3/token", s.handleToken)

	// Live-data reads are grouped so requireAuth applies to all of them at once.
	// When authToken is empty requireAuth is a pass-through, so the routes behave
	// exactly as before (P14: default-off).
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/api/1/products", s.handleProducts)
		r.Get("/api/1/vehicles", s.handleVehiclesList)
		r.Get("/api/1/vehicles/{id}", s.handleSummary)
		r.Get("/api/1/vehicles/{id}/vehicle_data", s.handleVehicleData)

		// Introspection endpoints: only when the source can expose cache internals
		// (the canonical-backed adapter does; the static test source does not).
		if intro, ok := s.source.(Introspector); ok {
			s.intro = intro
			r.Get("/status", s.handleStatus)
			r.Get("/stats", s.handleStats)
			r.Get("/api/1/vehicles/{id}/source_extras", s.handleExtras)
		}
	})
	return r
}

// requireAuth gates a route on a static bearer token when one is configured.
// With no token it is a pass-through (default-off). The compare is constant-time
// to avoid leaking the token via response timing.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.authToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests counts every request and logs only failures. The per-request
// success line was demoted: a hammering consumer once emitted ~7.5M lines and
// tripped journald rate-limiting (P16). The count stays visible via the /stats
// counters and the redswitchboard_http_requests_total metric.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Count by matched route pattern (populated by chi after routing), so the
		// per-vehicle {id} routes aggregate instead of exploding by id.
		if rc := chi.RouteContext(r.Context()); rc != nil {
			s.counters.inc(rc.RoutePattern())
		}
		if rec.status >= http.StatusBadRequest {
			s.logger.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// handleToken always returns a valid qts- token so TeslaMate's first sign-in,
// restart re-sign-in, and scheduled refresh all succeed. The request body and
// the ?token= query are ignored. When an authToken is configured it is returned
// as the access_token, so a consumer that signs in here ends up holding the same
// bearer the read routes require (P14); unset keeps the historical qts-local.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	access := "qts-local"
	if s.authToken != "" {
		access = s.authToken
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token":  access,
		"refresh_token": "local",
		"token_type":    "Bearer",
		"expires_in":    28800,
		"created_at":    time.Now().Unix(),
	})
}

func (s *Server) handleProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.source.Products()
	if err != nil {
		s.logger.Printf("products error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, wire.Response{Response: products})
}

// handleVehiclesList serves GET /api/1/vehicles: the vehicle list. This is the
// Owner API's list endpoint (so the tesla-owner-poll-v1 sink, which reuses this server,
// serves a real Owner API client) and is also a valid Fleet API endpoint. It
// returns the same vehicles as /api/1/products.
func (s *Server) handleVehiclesList(w http.ResponseWriter, r *http.Request) {
	products, err := s.source.Products()
	if err != nil {
		s.logger.Printf("vehicles list error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, wire.Response{Response: products})
}

// handleSummary serves GET /api/1/vehicles/{id} (the cheap state check).
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	summary, err := s.source.Summary(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, wire.Response{Response: summary})
}

// handleVehicleData serves GET /api/1/vehicles/{id}/vehicle_data. The ?endpoints=
// filter TeslaMate sends is advisory: we always return the full payload (all five
// sub-objects), which is the additive/superset behavior any consumer tolerates.
func (s *Server) handleVehicleData(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	data, err := s.source.VehicleData(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, wire.Response{Response: data})
}

// tagResolver is an optional data-source capability: resolve a Fleet API
// vehicle_tag (a VIN or an integer id) to the served integer id. The canonical
// adapter implements it so the sink accepts VIN-addressed requests (our own
// tesla-fleet-poll-v1 source uses VINs) like the real Fleet API, not just int64 ids.
type tagResolver interface {
	ResolveTag(tag string) (int64, bool)
}

// pathID resolves the {id} path segment to a served integer id. If the source can
// resolve tags (VIN or id), it is asked; otherwise the segment must be an int64.
func (s *Server) pathID(r *http.Request) (int64, bool) {
	tag := chi.URLParam(r, "id")
	if tr, ok := s.source.(tagResolver); ok {
		return tr.ResolveTag(tag)
	}
	id, err := strconv.ParseInt(tag, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the real Tesla Fleet API error envelope so non-TeslaMate
// consumers that parse it behave correctly: {"response":null,"error":...,
// "error_description":...}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"response":          nil,
		"error":             msg,
		"error_description": "",
	})
}
