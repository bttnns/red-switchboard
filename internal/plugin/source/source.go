// Package source is the INPUT seam of the hub: it defines the Source interface
// that every vendor input plugin (Rivian, and later Ford, Kia, ...) implements,
// plus a registry following the database/sql driver pattern. A plugin self-
// registers in its init() with Register(name, factory); the binary blank-imports
// the plugins it ships; the active plugin is selected by config and constructed
// with Open(name, settings, logger).
//
// A Source normalizes its vendor's wire shape into the source-neutral
// vehicle.Snapshot, so everything downstream (the poll cache, the sink) is
// vendor-agnostic.
package source

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// unauthenticated / rateLimited / retryAfter / quotaBlocked / accountDisabled
// / telemetryWiped are the vendor-agnostic error classifications the generic
// poll layer branches on. A source's error type implements the matching method
// so poll can react (re-auth circuit breaker, rate-limit backoff, server-
// specified Retry-After, billing-quota long backoff) without importing any
// vendor package.
type unauthenticated interface{ Unauthenticated() bool }
type rateLimited interface{ RateLimited() bool }
type retryAfter interface {
	RetryAfter() (time.Duration, bool)
}
type quotaBlocked interface{ QuotaBlocked() bool }
type accountDisabled interface{ AccountDisabled() bool }
type telemetryWiped interface{ TelemetryConfigWiped() bool }

// IsUnauthenticated reports whether err (or anything it wraps) is an auth
// failure the active source classifies as needing re-login.
func IsUnauthenticated(err error) bool {
	var u unauthenticated
	return errors.As(err, &u) && u.Unauthenticated()
}

// IsRateLimited reports whether err (or anything it wraps) is a rate-limit the
// active source classifies as a back-off signal.
func IsRateLimited(err error) bool {
	var r rateLimited
	return errors.As(err, &r) && r.RateLimited()
}

// IsQuotaBlocked reports whether err (or anything it wraps) is a billing/quota
// block (e.g. Tesla's 403 "account disabled: EXCEEDED_LIMIT"): a cap that clears
// only when the limit is raised or the cycle resets (hours to days), NOT a
// transient rate-limit. The poll layer backs off hard and surfaces it distinctly
// so a consumer cannot hammer the API for hours through an unrecognized block.
func IsQuotaBlocked(err error) bool {
	var q quotaBlocked
	return errors.As(err, &q) && q.QuotaBlocked()
}

// IsAccountDisabled reports whether err (or anything it wraps) is an account-
// disabled block. Today every quota block is also account-disabled (the Tesla
// 403 family), but the two are surfaced separately so a future vendor whose
// quota and account states diverge can classify them independently.
func IsAccountDisabled(err error) bool {
	var a accountDisabled
	return errors.As(err, &a) && a.AccountDisabled()
}

// TelemetryConfigWiped reports whether err (or anything it wraps) carries the
// signal that a billing-cap hit wiped the vendor's push-telemetry config (Tesla:
// hitting the limit REMOVES fleet_telemetry_config and does NOT auto-restore it).
// The operator must re-pair/reconfigure after the cap is raised, not just
// restart; this flag is that signal surfaced to /status and alerts.
func TelemetryConfigWiped(err error) bool {
	var w telemetryWiped
	return errors.As(err, &w) && w.TelemetryConfigWiped()
}

// RetryAfter returns the server-requested backoff carried by err (or anything it
// wraps), e.g. from an HTTP Retry-After or RateLimit-Reset header. The poll layer
// honors it over its own exponential backoff (the server knows best, and ignoring
// it, especially the Owner API's multi-hour lockouts, only makes things worse).
func RetryAfter(err error) (time.Duration, bool) {
	var r retryAfter
	if errors.As(err, &r) {
		return r.RetryAfter()
	}
	return 0, false
}

// Source is a vendor input plugin. It enumerates the account's vehicles and
// polls one of them into the canonical model. Implementations must be safe for
// concurrent Poll calls across vehicles (one account session serves every car).
type Source interface {
	// Name is the registry key (e.g. "rivian-graphql-poll-v1").
	Name() string
	// Vehicles returns the account's vehicles with their source-native identity.
	Vehicles(ctx context.Context) ([]vehicle.Identity, error)
	// Poll fetches the latest snapshot for one vehicle by its source-native id.
	Poll(ctx context.Context, id string) (vehicle.Snapshot, error)
}

// Factory constructs a Source from its plugin-specific settings (the YAML node
// under sources.<name>) and a logger. settings may be nil when the config
// omitted the block; the plugin must apply its own defaults.
type Factory func(settings *yaml.Node, logger *log.Logger) (Source, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a source plugin factory under name. Plugins call this from
// init(). A duplicate name or nil factory panics (a programming error).
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("source: Register factory is nil")
	}
	if _, dup := factories[name]; dup {
		panic("source: Register called twice for " + name)
	}
	factories[name] = f
}

// Open constructs the registered source plugin named name with its settings.
func Open(name string, settings *yaml.Node, logger *log.Logger) (Source, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("source: unknown plugin %q (registered: %v)", name, Names())
	}
	return f(settings, logger)
}

// Names returns the registered source plugin names, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
