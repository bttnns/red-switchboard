package v1

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

func TestParseRetryAfter(t *testing.T) {
	// Build headers via Set so keys are canonicalized exactly as a real HTTP
	// response stores them (http.Header.Get canonicalizes on read too).
	hdr := func(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }

	t.Run("delta seconds", func(t *testing.T) {
		d, ok := ParseRetryAfter(hdr("Retry-After", "120"))
		if !ok || d != 2*time.Minute {
			t.Fatalf("got %v %v, want 2m true", d, ok)
		}
	})
	t.Run("http date", func(t *testing.T) {
		future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
		d, ok := ParseRetryAfter(hdr("Retry-After", future))
		if !ok || d < 55*time.Minute {
			t.Fatalf("got %v %v, want ~1h true", d, ok)
		}
	})
	t.Run("ratelimit-reset epoch", func(t *testing.T) {
		reset := strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
		d, ok := ParseRetryAfter(hdr("RateLimit-Reset", reset))
		if !ok || d < 25*time.Minute {
			t.Fatalf("got %v %v, want ~30m true", d, ok)
		}
	})
	t.Run("none", func(t *testing.T) {
		if _, ok := ParseRetryAfter(http.Header{}); ok {
			t.Fatal("want ok=false when no header present")
		}
	})
}

// TestSourceErrorClassification confirms the typed error surfaces through the
// vendor-agnostic source helpers the poll layer uses.
func TestSourceErrorClassification(t *testing.T) {
	err := &SourceError{Label: "tesla fleet api", Status: http.StatusTooManyRequests, Retry: 90 * time.Minute, HasRetry: true}
	if !source.IsRateLimited(err) {
		t.Error("429 should classify as rate limited")
	}
	d, ok := source.RetryAfter(err)
	if !ok || d != 90*time.Minute {
		t.Errorf("RetryAfter: got %v %v, want 90m true", d, ok)
	}
}

func TestSummaryToSnapshot(t *testing.T) {
	cases := []struct {
		state  string
		power  vehicle.Power
		online bool
	}{
		{"online", vehicle.PowerOnline, true},
		{"asleep", vehicle.PowerSleep, true},
		{"offline", vehicle.PowerOffline, false},
	}
	for _, tc := range cases {
		snap := SummaryToSnapshot(wire.Summary{State: tc.state})
		if snap.State == nil {
			t.Fatalf("%s: nil state", tc.state)
		}
		if snap.State.Power != tc.power {
			t.Errorf("%s: power = %v, want %v", tc.state, snap.State.Power, tc.power)
		}
		if snap.State.CloudOnline != tc.online {
			t.Errorf("%s: cloud_online = %v, want %v", tc.state, snap.State.CloudOnline, tc.online)
		}
	}
}
