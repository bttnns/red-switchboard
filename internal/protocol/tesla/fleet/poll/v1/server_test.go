package v1

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
)

func newTestServer() http.Handler {
	src := NewStaticSource(101, 202, "TESTVIN0000000001", "Test Car")
	return newServer(src, "", nil).Handler()
}

func TestTokenEndpoint(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth2/v3/token?token=local", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["access_token"] != "qts-local" {
		t.Errorf("access_token = %v, want qts-local", body["access_token"])
	}
	if body["expires_in"].(float64) != 28800 {
		t.Errorf("expires_in = %v, want 28800", body["expires_in"])
	}
}

func TestProductsEndpoint(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/1/products", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Response []wire.Product `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Response) != 1 || env.Response[0].VehicleID != 202 {
		t.Fatalf("unexpected products: %+v", env.Response)
	}
}

func TestVehicleDataAllSubObjectsPresent(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/101/vehicle_data?endpoints=charge_state", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"charge_state", "climate_state", "drive_state", "vehicle_config", "vehicle_state"} {
		raw, ok := env.Response[key]
		if !ok || string(raw) == "null" {
			t.Errorf("%s missing or null", key)
		}
	}
}

func TestVehicleSummary(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/101", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestUnknownVehicle404(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/999/vehicle_data", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// P16: the per-request success line is demoted to keep journald volume down;
// only failures still log a line. The count stays in /stats and the metric.
func TestRequestLoggingFailureOnly(t *testing.T) {
	var buf bytes.Buffer
	src := NewStaticSource(101, 202, "TESTVIN0000000001", "Test Car")
	h := newServer(src, "", log.New(&buf, "", 0)).Handler()

	okReq := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/101", nil)
	h.ServeHTTP(httptest.NewRecorder(), okReq)
	if buf.Len() != 0 {
		t.Fatalf("2xx logged a line, want none: %q", buf.String())
	}

	failReq := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/999/vehicle_data", nil)
	h.ServeHTTP(httptest.NewRecorder(), failReq)
	if !strings.Contains(buf.String(), "-> 404") {
		t.Fatalf("4xx did not log a line: %q", buf.String())
	}
}

// P14: with no auth_token configured the read surface is open (back-compat).
func TestAuthDisabledServesReads(t *testing.T) {
	h := newTestServer() // empty token
	for _, path := range []string{"/api/1/products", "/api/1/vehicles/101", "/api/1/vehicles/101/vehicle_data"} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // no Authorization header
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (auth off)", path, rec.Code)
		}
	}
}

// P14: with auth_token set, a request missing the bearer is rejected and the
// correct bearer is accepted; the token endpoint stays open and echoes the bearer.
func TestAuthEnabledGatesReads(t *testing.T) {
	const token = "s3cr3t-bearer"
	src := NewStaticSource(101, 202, "TESTVIN0000000001", "Test Car")
	h := newServer(src, token, nil).Handler()

	readPaths := []string{"/api/1/products", "/api/1/vehicles/101", "/api/1/vehicles/101/vehicle_data"}

	// Missing or wrong bearer -> 401.
	for _, path := range readPaths {
		for _, hdr := range []string{"", "Bearer wrong", "Bearer " + token + "x"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if hdr != "" {
				req.Header.Set("Authorization", hdr)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s (auth=%q): status = %d, want 401", path, hdr, rec.Code)
			}
		}
	}

	// Correct bearer -> 200.
	for _, path := range readPaths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d with correct bearer, want 200", path, rec.Code)
		}
	}

	// The OAuth token endpoint stays open and returns the configured bearer so a
	// signing-in consumer ends up holding exactly the token the reads require.
	req := httptest.NewRequest(http.MethodPost, "/api/oauth2/v3/token", nil) // no Authorization header
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token endpoint status = %d, want 200 (must stay open)", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if body["access_token"] != token {
		t.Errorf("access_token = %v, want %q", body["access_token"], token)
	}
}
