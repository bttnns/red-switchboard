package v1

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/commander"
)

// fakeSink is the stand-in for the REST sink handler: it records every request
// that delegates to it (everything that is NOT a command POST) and answers 200
// with a marker so the test can assert the sink was reached unchanged.
type fakeSink struct {
	mu   sync.Mutex
	hits []string
}

func (f *fakeSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"sink":true}`))
}

// fakeCommander records calls and returns a configurable ack/error.
type fakeCommander struct {
	mu    sync.Mutex
	calls []call
	ack   commander.Ack
	err   error
	delay time.Duration
}

type call struct {
	vin, cmd string
	params   map[string]any
}

func (f *fakeCommander) Name() string { return "fake" }
func (f *fakeCommander) SendCommand(ctx context.Context, vin, cmd string, params map[string]any) (commander.Ack, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return commander.Ack{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{vin, cmd, params})
	f.mu.Unlock()
	return f.ack, f.err
}

func newMountedHandler(cmdr commander.Commander, sink http.Handler) http.Handler {
	return Mount(cmdr, log.New(io.Discard, "", 0), sink)
}

// TestMountCommandPostHandled asserts a POST to the command route reaches the
// commander (not the sink) and the response is the Tesla ack envelope.
func TestMountCommandPostHandled(t *testing.T) {
	cmdr := &fakeCommander{ack: commander.Ack{Result: true}}
	h := newMountedHandler(cmdr, &fakeSink{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/1/vehicles/5YJSA11111111111/command/charge_start", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Response commander.Ack `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode ack: %v (body: %s)", err, rec.Body.String())
	}
	if !env.Response.Result {
		t.Errorf("ack.Result = false, want true; body: %s", rec.Body.String())
	}
	cmdr.mu.Lock()
	defer cmdr.mu.Unlock()
	if len(cmdr.calls) != 1 || cmdr.calls[0].vin != "5YJSA11111111111" || cmdr.calls[0].cmd != "charge_start" {
		t.Errorf("commander calls = %+v, want one charge_start to the VIN", cmdr.calls)
	}
}

// TestMountParamsPassed asserts the JSON body is parsed and forwarded to the
// commander as params (e.g. set_charge_limit's charge_limit).
func TestMountParamsPassed(t *testing.T) {
	cmdr := &fakeCommander{ack: commander.Ack{Result: true}}
	h := newMountedHandler(cmdr, &fakeSink{})

	req := httptest.NewRequest(http.MethodPost, "/api/1/vehicles/VIN/command/set_charge_limit", strings.NewReader(`{"charge_limit":80}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	cmdr.mu.Lock()
	defer cmdr.mu.Unlock()
	if len(cmdr.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(cmdr.calls))
	}
	if got := cmdr.calls[0].params["charge_limit"]; got != float64(80) {
		t.Errorf("params[charge_limit] = %v (%T), want 80", got, got)
	}
}

// TestMountNominalFailureIs200 asserts a nominal failure (Result:false, nil err)
// answers 200 with the reason, the proxy's contract (not 5xx).
func TestMountNominalFailureIs200(t *testing.T) {
	cmdr := &fakeCommander{ack: commander.Ack{Result: false, Reason: "is_charging"}}
	h := newMountedHandler(cmdr, &fakeSink{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/1/vehicles/VIN/command/charge_start", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for nominal failure", rec.Code)
	}
	var env struct {
		Response struct {
			Result bool   `json:"result"`
			Reason string `json:"reason"`
		} `json:"response"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Response.Result || env.Response.Reason != "is_charging" {
		t.Errorf("nominal ack = %+v, want Result=false Reason=is_charging", env.Response)
	}
}

// TestMountInfraErrorIs502 asserts an infrastructure error (non-nil err) answers
// 502 with the Tesla error envelope.
func TestMountInfraErrorIs502(t *testing.T) {
	cmdr := &fakeCommander{err: errFake}
	h := newMountedHandler(cmdr, &fakeSink{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/1/vehicles/VIN/command/charge_start", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var env struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error == "" {
		t.Errorf("error envelope missing error field; body: %s", rec.Body.String())
	}
}

// TestMountNonCommandDelegates asserts every non-command request (GET routes,
// other paths) reaches the sink unchanged, so enabling commands does not alter
// the read surface.
func TestMountNonCommandDelegates(t *testing.T) {
	sink := &fakeSink{}
	h := newMountedHandler(&fakeCommander{ack: commander.Ack{Result: true}}, sink)

	for _, p := range []string{
		"/api/1/vehicles",
		"/api/1/vehicles/123/vehicle_data",
		"/api/1/products",
		"/status",
		"/metrics",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	// A POST to a non-command path also delegates (the sink 404/405s it).
	req := httptest.NewRequest(http.MethodPost, "/api/1/vehicles/123/wake_up", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.hits) != 6 {
		t.Fatalf("sink hits = %d (%+v), want 6 (5 GETs + 1 non-command POST)", len(sink.hits), sink.hits)
	}
}

// TestMountGetOnCommandPathDelegates asserts a GET to the command path does NOT
// reach the commander; it delegates to the sink (which 404/405s it), so the
// command route is POST-only.
func TestMountGetOnCommandPathDelegates(t *testing.T) {
	sink := &fakeSink{}
	cmdr := &fakeCommander{ack: commander.Ack{Result: true}}
	h := newMountedHandler(cmdr, sink)

	req := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/VIN/command/charge_start", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	cmdr.mu.Lock()
	defer cmdr.mu.Unlock()
	if len(cmdr.calls) != 0 {
		t.Errorf("commander received a GET: %+v", cmdr.calls)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.hits) != 1 {
		t.Errorf("sink hits = %d, want 1 (GET delegates to sink)", len(sink.hits))
	}
}

// TestMountTimeoutEnforcedByCommander asserts the route passes the request
// context through, so a commander that honors ctx deadline surfaces the timeout
// (the per-command deadline lives on the commander's context.WithTimeout).
func TestMountTimeoutEnforcedByCommander(t *testing.T) {
	cmdr := &fakeCommander{ack: commander.Ack{Result: true}, delay: 100 * time.Millisecond}
	h := newMountedHandler(cmdr, &fakeSink{})

	// A deadline shorter than the fake's delay: SendCommand returns ctx.Err().
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/1/vehicles/VIN/command/charge_start", nil).WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (context deadline surfaced as infra error)", rec.Code)
	}
}

var errFake = &fakeErr{"simulated infrastructure failure"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }
