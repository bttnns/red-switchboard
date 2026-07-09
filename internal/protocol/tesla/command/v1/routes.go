package v1

// routes.go mounts the Tesla command REST routes on the sink's HTTP surface.
// The routes are /api/1/vehicles/{vin}/command/{cmd} (POST), the exact path the
// tesla-http-proxy serves, so a consumer (evcc, etc.) repointed onto
// red-switchboard keeps its command URLs. Mount wraps the existing sink handler:
// a matching POST is handled here; every other request delegates to the sink
// unchanged. serve.go calls Mount only when commands.enabled is true, so with
// the default false the sink handler is used verbatim and no write path exists
// (the structural read-only guarantee).

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/bttnns/red-switchboard/internal/plugin/commander"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Mount returns an http.Handler that serves the command POST routes and
// delegates every other request to next (the REST sink handler). The command
// routes are registered ONLY here, so when commands are disabled serve.go never
// calls Mount and the sink handler is byte-for-byte unchanged.
func Mount(cmdr commander.Commander, logger *log.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Post("/api/1/vehicles/{vin}/command/{cmd}", func(w http.ResponseWriter, req *http.Request) {
		handleCommand(w, req, cmdr, logger)
	})
	// Everything else (all sink GET routes, /metrics, /status, ...) delegates to
	// the sink. chi's NotFound/MethodNotAllowed take http.HandlerFunc, so wrap the
	// sink; a non-command request falls through to next untouched.
	r.NotFound(next.ServeHTTP)
	r.MethodNotAllowed(next.ServeHTTP)
	return r
}

// handleCommand resolves the VIN + command name from the path, parses the JSON
// body into params, calls the commander, and writes the Tesla ack envelope
// {"response":{"result":...,"reason":...}} the way the real proxy does. A nominal
// failure (Result:false) is still 200 (the proxy's contract); an infrastructure
// error is 500/502.
func handleCommand(w http.ResponseWriter, req *http.Request, cmdr commander.Commander, logger *log.Logger) {
	vin := chi.URLParam(req, "vin")
	cmd := chi.URLParam(req, "cmd")

	// Bound the body so a malicious client cannot exhaust memory.
	rb := http.MaxBytesReader(w, req.Body, 1<<20)
	body, err := io.ReadAll(rb)
	if err != nil {
		writeAckError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	params, err := parseParams(body)
	if err != nil {
		writeAckError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ack, err := cmdr.SendCommand(req.Context(), vin, cmd, params)
	if err != nil {
		logger.Printf("tesla-command: %s %s: %v", vin, cmd, err)
		// 502: we could not submit the command to Tesla (auth/connect/signing).
		writeAckError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeAck(w, ack)
}

// writeAck writes the Tesla ack envelope for a completed (success or nominal
// failure) command: {"response":{"result":...,"reason":...}}.
func writeAck(w http.ResponseWriter, ack commander.Ack) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"response": ack})
}

// writeAckError writes the Tesla error envelope for a transport/client error.
func writeAckError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"response":          nil,
		"error":             reason,
		"error_description": "",
	})
}
