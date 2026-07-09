package wssutil

import (
	"testing"
	"time"
)

func TestReconnectBackoffEscalatesAndResets(t *testing.T) {
	b := NewReconnectBackoff(100*time.Millisecond, 5*time.Second)

	// First delay is the initial interval (+-10% jitter, so within [90,110]ms).
	d0 := b.NextDelay()
	if d0 < 90*time.Millisecond || d0 > 110*time.Millisecond {
		t.Fatalf("initial delay = %v, want ~100ms", d0)
	}

	// Subsequent delays escalate (double, +-10%), and never exceed the cap by
	// more than the jitter band (cenkalti randomizes around the capped interval).
	prev := d0
	var max time.Duration
	for i := 0; i < 20; i++ {
		d := b.NextDelay()
		if d > 5*time.Second*115/100 {
			t.Fatalf("delay %d = %v exceeded cap+jitter", i, d)
		}
		if d > max {
			max = d
		}
		_ = prev
	}
	if max <= d0 {
		t.Fatalf("backoff never escalated: max=%v initial=%v", max, d0)
	}

	// Reset returns to the initial band.
	b.Reset()
	d := b.NextDelay()
	if d < 90*time.Millisecond || d > 110*time.Millisecond {
		t.Fatalf("post-reset delay = %v, want ~100ms", d)
	}
}

func TestReconnectBackoffDefaults(t *testing.T) {
	b := NewReconnectBackoff(0, 0)
	d := b.NextDelay()
	if d <= 0 || d > defaultReconnectInitial+defaultReconnectInitial/5 {
		t.Fatalf("default initial delay = %v, want ~%v", d, defaultReconnectInitial)
	}
	for i := 0; i < 40; i++ {
		if d = b.NextDelay(); d > defaultReconnectMax*115/100 {
			t.Fatalf("delay exceeded default cap+jitter: %v", d)
		}
	}
}
