package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"/api/1/vehicles/5107700305316668735/vehicle_data", "/api/1/vehicles/{id}/vehicle_data"},
		{"/api/1/vehicles/5107700305316668735", "/api/1/vehicles/{id}"},
		{"/api/gql", "/api/gql"}, // Rivian's single GraphQL path, untouched
		{"/status", "/status"},
		{"/stats", "/stats"},
		{"/api/1/vehicles", "/api/1/vehicles"},
		// UUID / GUID style ids collapse (long opaque token with digits).
		{"/v/2a8f1c4e-9b3d-4f10-aa12-77c0d9e1b234/data", "/v/{id}/data"},
		// short version segments and words are preserved.
		{"/api/v1/health", "/api/v1/health"},
		// trailing slash preserved as empty segment.
		{"/api/1/vehicles/5107700305316668735/", "/api/1/vehicles/{id}/"},
		// short numeric segments (version-like) are preserved, not collapsed.
		{"/api/1/health", "/api/1/health"},
	}
	for _, c := range cases {
		if got := NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderStreamingSinkGauges(t *testing.T) {
	reg := New()
	reg.SetStreamSink(func() StreamSinkStats {
		return StreamSinkStats{Consumers: 3, FramesPushed: 100, FramesDropped: 2}
	})
	body := reg.render(0)
	for _, want := range []string{
		`redswitchboard_stream_sink_consumers 3`,
		`redswitchboard_stream_sink_frames_pushed_total 100`,
		`redswitchboard_stream_sink_frames_dropped_total 2`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderStreamingSourceGauges(t *testing.T) {
	reg := New()
	reg.SetStreamSource(func() StreamSourceStats {
		return StreamSourceStats{Connected: 2, Frames: 42, LastFrameAge: 5 * time.Second}
	})
	body := reg.render(0)
	for _, want := range []string{
		`redswitchboard_stream_source_connected 2`,
		`redswitchboard_stream_source_frames_total 42`,
		`redswitchboard_stream_source_last_frame_age_seconds 5`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderStreamSourceDetail(t *testing.T) {
	reg := New()
	reg.SetStreamSource(func() StreamSourceStats {
		return StreamSourceStats{
			Connected: 1, Connects: 4, Frames: 10, LastFrameAge: 2 * time.Second,
			GapCount: 9, GapSum: 13.5,
			GapBuckets:   map[float64]uint64{0.5: 1, 1: 3, 2: 6, 5: 9, 10: 9, 20: 9, 30: 9, 60: 9, 120: 9, 300: 9},
			FieldFrames:  map[string]int64{"location": 10, "range": 4, "charge_power": 2},
			Rejects:      map[string]int64{"unknown_vin": 3, "identity": 1},
			Disconnects:  3,
			IdleTimeouts: 1,
		}
	})
	body := reg.render(0)
	for _, want := range []string{
		`redswitchboard_stream_source_connects_total 4`,
		`redswitchboard_stream_source_disconnects_total 3`,
		`redswitchboard_stream_source_idle_timeouts_total 1`,
		`redswitchboard_stream_source_frame_gap_seconds_count 9`,
		`redswitchboard_stream_source_frame_gap_seconds_sum 13.5`,
		`redswitchboard_stream_source_field_frames_total{field="location"} 10`,
		`redswitchboard_stream_source_field_frames_total{field="range"} 4`,
		`redswitchboard_stream_source_field_frames_total{field="charge_power"} 2`,
		`redswitchboard_stream_source_rejects_total{reason="unknown_vin"} 3`,
		`redswitchboard_stream_source_rejects_total{reason="identity"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderSourceCostAndStateMetrics(t *testing.T) {
	reg := New()
	reg.SetSource("tesla-fleet-poll-v1", func() []VehicleMetric {
		return []VehicleMetric{{
			VIN:                 "5YJTEST000000001",
			SuccessCount:        100,
			VehicleDataFetches:  40,
			PollsByState:        map[string]int64{"asleep": 60, "driving": 30, "charging_dc": 10},
			ScheduledInterval:   90 * time.Second,
			StreamBackoffActive: true,
			DerivedState:        "driving",
		}}
	})
	body := reg.render(1)
	for _, want := range []string{
		`redswitchboard_source_vehicle_data_fetches_total{source="tesla-fleet-poll-v1",vin="5YJTEST000000001"} 40`,
		`redswitchboard_source_scheduled_interval_seconds{source="tesla-fleet-poll-v1",vin="5YJTEST000000001"} 90`,
		`redswitchboard_source_stream_backoff_active{source="tesla-fleet-poll-v1",vin="5YJTEST000000001"} 1`,
		`redswitchboard_source_state_polls_total{source="tesla-fleet-poll-v1",state="charging_dc",vin="5YJTEST000000001"} 10`,
		`redswitchboard_source_state_polls_total{source="tesla-fleet-poll-v1",state="driving",vin="5YJTEST000000001"} 30`,
		`redswitchboard_source_state{source="tesla-fleet-poll-v1",state="driving",vin="5YJTEST000000001"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderStreamIntegrityRejections(t *testing.T) {
	reg := New()
	reg.SetStreamIntegrity(func() map[string]int64 {
		return map[string]int64{
			"odometer_regress": 2,
			"gps_teleport":     1,
			"soc_range":        0,
			"speed_range":      3,
		}
	})
	body := reg.render(0)
	for _, want := range []string{
		`redswitchboard_stream_integrity_rejections_total{reason="odometer_regress"} 2`,
		`redswitchboard_stream_integrity_rejections_total{reason="gps_teleport"} 1`,
		`redswitchboard_stream_integrity_rejections_total{reason="soc_range"} 0`,
		`redswitchboard_stream_integrity_rejections_total{reason="speed_range"} 3`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderSessionCounters(t *testing.T) {
	reg := New()
	reg.SetSession(func() map[string]int64 {
		return map[string]int64{
			"drives_opened":  3,
			"drives_closed":  2,
			"charges_opened": 1,
			"charges_closed": 1,
		}
	})
	body := reg.render(0)
	for _, want := range []string{
		`redswitchboard_cache_sessions_total{edge="opened",kind="drives"} 3`,
		`redswitchboard_cache_sessions_total{edge="closed",kind="drives"} 2`,
		`redswitchboard_cache_sessions_total{edge="opened",kind="charges"} 1`,
		`redswitchboard_cache_sessions_total{edge="closed",kind="charges"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderUnchangedReads(t *testing.T) {
	reg := New()
	reg.SetSourceUnchanged("tesla-fleet-poll-v1", func() int64 { return 7 })
	body := reg.render(0)
	want := `redswitchboard_source_unchanged_reads_total{source="tesla-fleet-poll-v1"} 7`
	if !strings.Contains(body, want+"\n") {
		t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
	}
}

func TestRenderCostEstimate(t *testing.T) {
	reg := New()
	reg.SetSourceCost("tesla-fleet-poll-v1", 0.002, func() []VehicleMetric {
		return []VehicleMetric{
			{VIN: "5YJTEST000000001", VehicleDataFetches: 40},
			{VIN: "5YJTEST000000002", VehicleDataFetches: 10},
		}
	})
	body := reg.render(0)
	for _, want := range []string{
		`redswitchboard_source_vehicle_data_price_usd{source="tesla-fleet-poll-v1"} 0.002`,
		`redswitchboard_source_estimated_cost_usd_total{source="tesla-fleet-poll-v1"} 0.1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("rendered body missing line:\n  %s\n--- body ---\n%s", want, body)
		}
	}
}

func TestRenderNoCostWhenPriceUnset(t *testing.T) {
	reg := New()
	// A zero/unset price must not emit a misleading $0 cost series.
	reg.SetSourceCost("tesla-fleet-poll-v1", 0, func() []VehicleMetric {
		return []VehicleMetric{{VIN: "5YJTEST000000001", VehicleDataFetches: 40}}
	})
	body := reg.render(0)
	if strings.Contains(body, "redswitchboard_source_estimated_cost_usd_total") {
		t.Errorf("zero price should emit no cost series:\n%s", body)
	}
}

func TestRenderNoStreamingGaugesWhenUnset(t *testing.T) {
	reg := New()
	body := reg.render(0)
	if strings.Contains(body, "redswitchboard_stream_") {
		t.Errorf("unset streaming gauges should not render:\n%s", body)
	}
}

func TestRenderExposition(t *testing.T) {
	reg := New()

	// Record some HTTP observations.
	reg.observe("GET", "/api/1/vehicles/{id}/vehicle_data", 200, 100*time.Millisecond)
	reg.observe("GET", "/api/1/vehicles/{id}/vehicle_data", 200, 300*time.Millisecond)
	reg.observe("GET", "/api/1/vehicles/{id}/vehicle_data", 404, 5*time.Millisecond)
	reg.observe("POST", "/api/gql", 200, 50*time.Millisecond)

	// Feed fake poll stats.
	reg.SetSource("rivian-graphql-poll-v1", func() []VehicleMetric {
		return []VehicleMetric{{
			VIN:                 "7FATESTVIN0000001",
			SuccessCount:        42,
			ErrorCount:          3,
			RateLimitedCount:    1,
			ChangedCount:        17,
			Backoff:             30 * time.Second,
			ConsecutiveFailures: 2,
			NeedsReauth:         true,
		}}
	})

	body := reg.render(1)

	wantLines := []string{
		`# TYPE redswitchboard_http_requests_total counter`,
		`redswitchboard_http_requests_total{method="GET",route="/api/1/vehicles/{id}/vehicle_data",status="200"} 2`,
		`redswitchboard_http_requests_total{method="GET",route="/api/1/vehicles/{id}/vehicle_data",status="404"} 1`,
		`redswitchboard_http_requests_total{method="POST",route="/api/gql",status="200"} 1`,
		`# TYPE redswitchboard_http_request_duration_seconds histogram`,
		`redswitchboard_http_request_duration_seconds_count{method="GET",route="/api/1/vehicles/{id}/vehicle_data"} 3`,
		`redswitchboard_source_polls_total{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 42`,
		`redswitchboard_source_poll_errors_total{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 3`,
		`redswitchboard_source_poll_changes_total{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 17`,
		`redswitchboard_source_rate_limited_total{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 1`,
		`redswitchboard_source_poll_backoff_seconds{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 30`,
		`redswitchboard_source_consecutive_failures{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 2`,
		`redswitchboard_source_needs_reauth{source="rivian-graphql-poll-v1",vin="7FATESTVIN0000001"} 1`,
		`redswitchboard_vehicles_known 1`,
		`# TYPE redswitchboard_uptime_seconds gauge`,
		`# TYPE go_goroutines gauge`,
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want+"\n") && !strings.HasSuffix(body, want) {
			t.Errorf("rendered body missing line:\n  %s\n--- full body ---\n%s", want, body)
		}
	}
	// The histogram now emits real buckets (the point of using the library):
	// a _bucket line and a _sum line must appear.
	if !strings.Contains(body, "redswitchboard_http_request_duration_seconds_bucket{") {
		t.Errorf("rendered body missing histogram bucket line:\n%s", body)
	}
	if !strings.Contains(body, "redswitchboard_http_request_duration_seconds_sum{") {
		t.Errorf("rendered body missing histogram sum line:\n%s", body)
	}
}

func TestEscapeLabelValueIsHandledByLibrary(t *testing.T) {
	// Escaping is now the Prometheus client library's job; we only assert a
	// label value with special characters survives a scrape round-trip in the
	// route label without breaking exposition.
	reg := New()
	reg.observe("GET", `route"with\special
chars`, 200, time.Millisecond)
	body := reg.render(0)
	if !strings.Contains(body, `redswitchboard_http_requests_total{method="GET",route="route\"with\\special\nchars",status="200"} 1`) {
		t.Errorf("escaped route label not rendered as expected:\n%s", body)
	}
}

func TestMiddlewareRecordsStatusAndRoute(t *testing.T) {
	reg := New()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	})
	h := reg.Middleware(inner)

	req := httptest.NewRequest("GET", "/api/1/vehicles/5107700305316668735/vehicle_data", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status passthrough = %d, want %d", rec.Code, http.StatusTeapot)
	}

	body := reg.render(0)
	want := `redswitchboard_http_requests_total{method="GET",route="/api/1/vehicles/{id}/vehicle_data",status="418"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("middleware did not record normalized route+status; body:\n%s", body)
	}
}

func TestHandlerContentType(t *testing.T) {
	reg := New()
	rec := httptest.NewRecorder()
	reg.Handler(0).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4 prefix", ct)
	}
	if _, err := io.ReadAll(rec.Body); err != nil {
		t.Fatal(err)
	}
}
