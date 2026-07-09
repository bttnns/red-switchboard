package cache

import (
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
)

func TestSessionBoundaryCrossed(t *testing.T) {
	gear := func(g vehicle.Gear) vehicle.Snapshot {
		return vehicle.Snapshot{State: &vehicle.State{Gear: g}}
	}
	charging := vehicle.Snapshot{State: &vehicle.State{}, Live: &vehicle.LiveSession{PowerKw: 3}}
	zeroFrame := vehicle.Snapshot{State: &vehicle.State{}, Live: &vehicle.LiveSession{PowerKw: 0}, StreamPresent: vehicle.StreamChargePower}
	nonzeroFrame := vehicle.Snapshot{State: &vehicle.State{}, Live: &vehicle.LiveSession{PowerKw: 3}, StreamPresent: vehicle.StreamChargePower}

	cases := []struct {
		name           string
		prev, raw, now vehicle.Snapshot
		want           bool
	}{
		{"drive start park->drive", gear(vehicle.GearPark), gear(vehicle.GearDrive), gear(vehicle.GearDrive), true},
		{"drive start unknown->drive", gear(vehicle.GearUnknown), gear(vehicle.GearDrive), gear(vehicle.GearDrive), true},
		{"drive end drive->park", gear(vehicle.GearDrive), gear(vehicle.GearPark), gear(vehicle.GearPark), true},
		{"drive end reverse->park", gear(vehicle.GearReverse), gear(vehicle.GearPark), gear(vehicle.GearPark), true},
		{"within-drive drive->reverse", gear(vehicle.GearDrive), gear(vehicle.GearReverse), gear(vehicle.GearReverse), false},
		{"still parked", gear(vehicle.GearPark), gear(vehicle.GearPark), gear(vehicle.GearPark), false},
		{"unknown->park (not a boundary)", gear(vehicle.GearUnknown), gear(vehicle.GearPark), gear(vehicle.GearPark), false},
		{"charge end power>0 -> 0 frame", charging, zeroFrame, charging, true},
		{"charge continues nonzero frame", charging, nonzeroFrame, charging, false},
		{"0 frame but was not charging", gear(vehicle.GearPark), zeroFrame, gear(vehicle.GearPark), false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, sessionBoundaryCrossed(c.prev, c.raw, c.now), c.name)
	}
}

// TestWakeDebouncer feeds wake sequences (whether the car was asleep before the
// frame + seconds since t0) through a fresh debouncer and counts the wake PollNows
// it fires. A single stray frame from asleep must NOT fire (TeslaMate sleep must
// survive a buffered frame), sustained frames must fire exactly once, an
// already-awake stream must never fire, and a later sleep->wake cycle must be able
// to fire again.
func TestWakeDebouncer(t *testing.T) {
	type step struct {
		sleeping bool    // car was asleep BEFORE this frame
		sec      float64 // seconds after t0
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()

	cases := []struct {
		name  string
		steps []step
		want  int // wake PollNows fired
	}{
		{
			name:  "single stray frame from asleep does not fire",
			steps: []step{{true, 0}},
			want:  0,
		},
		{
			name: "frame count met but window not spanned does not fire",
			// Three frames flushed in the same instant: a buffer dump, not a wake.
			steps: []step{{true, 0}, {true, 0}, {true, 0}},
			want:  0,
		},
		{
			name: "sustained frames from asleep fire exactly once",
			steps: []step{
				{true, 0},
				{true, 1.5},
				{true, 3}, // threshold count reached and window spanned
				{true, 4}, // still asleep-before, but latched: no re-fire
				{true, 5},
			},
			want: 1,
		},
		{
			name: "already online never fires",
			steps: []step{
				{false, 0}, {false, 2}, {false, 4}, {false, 6}, {false, 8},
			},
			want: 0,
		},
		{
			name: "wake then sleep then wake fires twice",
			steps: []step{
				{true, 0},
				{true, 2},
				{true, 4},  // first wake fires
				{false, 6}, // poll confirmed online: run resets, latch clears
				{false, 8}, // awake, streaming
				{true, 20}, // back asleep, then a fresh wake
				{true, 22},
				{true, 24}, // second wake fires
			},
			want: 2,
		},
		{
			name: "brief frame mid-run resets the sustain window",
			steps: []step{
				{true, 0},
				{false, 1}, // not asleep: resets the run before it could fire
				{true, 2},  // run restarts here
				{true, 3},
				{true, 5}, // now threshold + window met from the restart
			},
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var w wakeDebouncer
			fired := 0
			for _, s := range c.steps {
				if w.frame(s.sleeping, t0.Add(time.Duration(s.sec*float64(time.Second)))) {
					fired++
				}
			}
			assert.Equal(t, c.want, fired, c.name)
		})
	}
}

// TestDriveDebouncer feeds gear sequences (gear + seconds since t0) through a fresh
// debouncer and counts the boundaries it fires. A gear flap (P->R->D shuffle, rapid
// alternation) must collapse to a single boundary, while genuine park-then-drive
// boundaries spaced beyond the flap window must each fire.
func TestDriveDebouncer(t *testing.T) {
	gearSnap := func(g vehicle.Gear) vehicle.Snapshot {
		return vehicle.Snapshot{State: &vehicle.State{Gear: g}}
	}
	type step struct {
		gear vehicle.Gear
		sec  float64 // seconds after t0
	}
	t0 := time.Unix(1_700_000_000, 0).UTC()

	cases := []struct {
		name  string
		seed  vehicle.Gear // the settled gear before the sequence (prev of the first step)
		steps []step
		want  int // boundaries fired
	}{
		{
			name: "parking shuffle P->R->D forks once",
			seed: vehicle.GearPark,
			// Pull out of a spot: R then D within a couple seconds is one drive start.
			steps: []step{{vehicle.GearReverse, 0}, {vehicle.GearDrive, 1.5}},
			want:  1,
		},
		{
			name: "drive end then parking shuffle D->P->R->P stays one boundary",
			seed: vehicle.GearDrive,
			steps: []step{
				{vehicle.GearPark, 0},    // drive ends: real stop
				{vehicle.GearReverse, 1}, // reposition: flap back into drive within window
				{vehicle.GearPark, 2},    // settle parked again
			},
			want: 1,
		},
		{
			name: "rapid alternation collapses to one",
			seed: vehicle.GearPark,
			steps: []step{
				{vehicle.GearDrive, 0},
				{vehicle.GearPark, 0.5},
				{vehicle.GearDrive, 1},
				{vehicle.GearPark, 1.5},
				{vehicle.GearDrive, 2},
			},
			want: 1,
		},
		{
			name:  "genuine park then drive fires once",
			seed:  vehicle.GearPark,
			steps: []step{{vehicle.GearDrive, 0}},
			want:  1,
		},
		{
			name: "full drive: start and stop both fire",
			seed: vehicle.GearPark,
			// A real drive lasts well past the flap window, so the stop is not swallowed.
			steps: []step{{vehicle.GearDrive, 0}, {vehicle.GearPark, 600}},
			want:  2,
		},
		{
			name: "sustained re-entry past the window fires again",
			seed: vehicle.GearDrive,
			steps: []step{
				{vehicle.GearPark, 0},   // stop
				{vehicle.GearDrive, 10}, // a fresh drive 10s later, beyond the window
			},
			want: 2,
		},
		{
			name:  "no boundary while parked",
			seed:  vehicle.GearPark,
			steps: []step{{vehicle.GearPark, 1}, {vehicle.GearPark, 2}},
			want:  0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var d driveDebouncer
			prev := gearSnap(c.seed)
			fired := 0
			for _, s := range c.steps {
				now := gearSnap(s.gear)
				if d.boundary(prev, now, t0.Add(time.Duration(s.sec*float64(time.Second)))) {
					fired++
				}
				prev = now
			}
			assert.Equal(t, c.want, fired, c.name)
		})
	}
}
