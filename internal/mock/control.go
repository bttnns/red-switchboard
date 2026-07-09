package mock

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Control wraps a protocol sink's handler with a small /mock/scenario control
// surface, so the same fake serves the protocol API (everything else) and lets a
// developer flip scenarios at runtime:
//
//	curl        http://localhost:5050/mock/scenario             # read per-vehicle scenario
//	curl -XPOST http://localhost:5050/mock/scenario/driving     # switch all vehicles
//	curl -XPOST 'http://localhost:5050/mock/scenario/charging?vehicle=<guid>'
//
// Optional query params drive SOC-waypoint cycling: `to` is a target SOC the car
// drives toward then AUTO-STOPS at (a floor for driving, a limit for charging),
// and `kw` overrides the charge power (e.g. a 35kW public charger):
//
//	curl -XPOST 'http://localhost:5050/mock/scenario/driving?vehicle=<guid>&to=10'
//	curl -XPOST 'http://localhost:5050/mock/scenario/charging?vehicle=<guid>&to=70&kw=35'
func Control(engine *Engine, inner http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mock/scenario", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"scenarios": engine.Scenarios()})
	})
	mux.HandleFunc("/mock/scenario/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/mock/scenario/")
		q := r.URL.Query()
		guid := q.Get("vehicle")
		var opts ScenarioOpts
		if v := q.Get("to"); v != "" {
			opts.TargetSOC, _ = strconv.ParseFloat(v, 64)
		}
		if v := q.Get("kw"); v != "" {
			opts.PowerKw, _ = strconv.ParseFloat(v, 64)
		}
		if !engine.SetScenarioOpts(name, guid, opts) {
			http.Error(w, "unknown scenario or vehicle", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"scenario": name, "vehicle": guid, "to": opts.TargetSOC, "kw": opts.PowerKw})
	})
	mux.Handle("/", inner)
	return mux
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}
