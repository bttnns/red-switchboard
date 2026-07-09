package wssutil

// backoff.go is the reconnect backoff wrapper over cenkalti/backoff/v5, added
// with the Owner streaming dialer (its first caller). It mirrors internal/poll's
// exponential backoff (initial interval, doubling, +-10% jitter, cap) so a
// dialer that keeps failing escalates instead of tight-retrying: the exact bug
// that made TeslaMate hammer the streaming endpoint every 10s on an
// unrecognized error. One ReconnectBackoff belongs to one dialer goroutine; it
// is not shared across vehicles.

import (
	"time"

	"github.com/cenkalti/backoff/v5"
)

const (
	defaultReconnectInitial = 1 * time.Second
	defaultReconnectMax     = 5 * time.Minute
)

// ReconnectBackoff wraps an exponential backoff for a reconnect loop. NextDelay
// returns the next delay (with jitter) and escalates on each call; Reset returns
// to the initial interval after a successful connect so a brief blip does not
// inherit a long backoff. It is NOT safe for concurrent use; each dialer owns
// its own instance.
type ReconnectBackoff struct {
	eb *backoff.ExponentialBackOff
}

// NewReconnectBackoff builds an exponential backoff that doubles from initial,
// capped at max, with +-10% jitter. Zero initial/max fall back to defaults.
func NewReconnectBackoff(initial, max time.Duration) *ReconnectBackoff {
	if initial <= 0 {
		initial = defaultReconnectInitial
	}
	if max <= 0 {
		max = defaultReconnectMax
	}
	eb := backoff.NewExponentialBackOff()
	eb.InitialInterval = initial
	eb.Multiplier = 2
	eb.RandomizationFactor = 0.1
	eb.MaxInterval = max
	eb.Reset()
	return &ReconnectBackoff{eb: eb}
}

// NextDelay returns the next reconnect delay, escalating on each call.
func (b *ReconnectBackoff) NextDelay() time.Duration { return b.eb.NextBackOff() }

// Reset returns the backoff to its initial interval. Call after a successful
// connect so a sustained connection followed by a brief blip does not inherit a
// long backoff.
func (b *ReconnectBackoff) Reset() { b.eb.Reset() }
