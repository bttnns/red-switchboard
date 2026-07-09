package poll

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// captureSlog redirects slog.Default to an in-memory text handler for the duration
// of the test and returns the buffer plus a restore func.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestPollDecisionTrail: a synthetic poll cycle that walks asleep -> online ->
// driving produces a readable, structured decision trail: one "poll" line per
// poll (carrying vehicle/state/interval/trigger) and one "state transition" line
// per derived-state change (carrying from/to).
func TestPollDecisionTrail(t *testing.T) {
	buf := captureSlog(t)

	src := &fakeSource{}
	p := New(src, "VINTEST", Intervals{
		Online: 2 * time.Minute, Driving: 10 * time.Second,
		Asleep: 15 * time.Minute, Default: 60 * time.Second,
	}, nil)

	// asleep -> online -> driving, each a distinct poll with a distinct trigger.
	src.snap = snapOf(&vehicle.State{Power: vehicle.PowerSleep}, nil)
	p.pollOnce(context.Background(), "scheduled")
	src.snap = snapOf(&vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearPark}, nil)
	p.pollOnce(context.Background(), "boundary")
	src.snap = snapOf(&vehicle.State{Power: vehicle.PowerOnline, Gear: vehicle.GearDrive}, nil)
	p.pollOnce(context.Background(), "scheduled")

	out := buf.String()
	lines := strings.Count(out, `msg=poll`)
	if lines != 3 {
		t.Fatalf("want 3 poll lines, got %d\n%s", lines, out)
	}

	// Each poll line carries the operator-facing decision fields.
	for _, want := range []string{
		`msg=poll`, `vehicle=VINTEST`, `state=asleep`, `trigger=scheduled`,
		`state=online`, `trigger=boundary`, `state=driving`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("poll trail missing %q in:\n%s", want, out)
		}
	}

	// The two derived-state transitions are logged with from/to.
	if !strings.Contains(out, `msg="state transition"`) ||
		!strings.Contains(out, "from=asleep to=online") ||
		!strings.Contains(out, "from=online to=driving") {
		t.Errorf("missing expected state transitions in:\n%s", out)
	}
}
