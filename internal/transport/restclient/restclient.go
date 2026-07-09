// Package restclient is the standardized HTTP client every redswitchboard source
// composes. redswitchboard standardizes outbound HTTP on go-resty/resty: this
// package centralizes the shared setup (base URL, backstop timeout, retry with
// backoff on transient failures, debug logging) so each make's source client gets
// consistent behavior and only adds its vendor-specific parts (auth headers, the
// request shape, endpoint paths) on top of the returned *resty.Client.
//
// It is deliberately thin: it returns a real *resty.Client, so callers use the
// full resty request API (c.R().SetContext(...).SetResult(...).Get(...)) directly
// rather than through a bespoke wrapper. The matching server side standardizes on
// go-chi/chi.
package restclient

import (
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
)

// Built-in defaults for the retry/backoff behavior. These match the historical
// hardcoded values, so behavior is unchanged unless a deployer overrides them via
// the config file (see SetDefaults).
const (
	// defaultRetries is the number of automatic retries for transient failures
	// (network errors, and the 429/5xx responses resty retries by default).
	defaultRetries      = 2
	defaultRetryWait    = 500 * time.Millisecond
	defaultRetryMaxWait = 5 * time.Second
	// defaultTimeout is the per-request backstop applied when a caller passes a
	// non-positive timeout to New.
	defaultTimeout = 30 * time.Second
	// defaultUserAgent overrides resty's default ("go-resty/..."), which Akamai's
	// bot manager denylists on auth.tesla.com: the token-refresh POST gets a 403
	// "Access Denied" HTML page instead of an OAuth reply, so refresh can never
	// succeed and every consumer falls back to stale cache once the access token
	// expires. Any honest non-go-resty UA clears the block. SetUserAgent replaces
	// this with a version-stamped value at startup.
	defaultUserAgent = "redswitchboard"
)

// Options bundles the tunables every source client shares. A zero value is NOT
// usable directly; callers should start from the package defaults (see
// NewWithOptions, which fills zero fields from the configured defaults).
type Options struct {
	Timeout      time.Duration // per-request backstop timeout
	Retries      int           // automatic retries on transient failures
	RetryWait    time.Duration // base wait between retries
	RetryMaxWait time.Duration // cap on the retry wait
	Debug        bool          // when true, resty logs each request/response
}

// pkgDefaults holds the process-wide retry/backoff defaults. serve sets these
// from the config's `http` block once at startup (see restclient.SetDefaults),
// mirroring how other cross-cutting settings are pushed into a plugin package
// (e.g. teslasink.SetStaleAfter). Guarded by mu because Open may run on a
// different goroutine than startup.
var (
	mu          sync.RWMutex
	pkgDefaults = Options{
		Retries:      defaultRetries,
		RetryWait:    defaultRetryWait,
		RetryMaxWait: defaultRetryMaxWait,
		Timeout:      defaultTimeout,
	}
	userAgent = defaultUserAgent
)

// SetDefaults overrides the process-wide retry/backoff/timeout defaults used by
// New and as the fill-in for zero fields in NewWithOptions. Only positive fields
// are applied, so a partially-populated Options (e.g. from a config block that
// sets only `retries`) leaves the other defaults intact. Call once at startup
// before opening any source.
func SetDefaults(opts Options) {
	mu.Lock()
	defer mu.Unlock()
	if opts.Timeout > 0 {
		pkgDefaults.Timeout = opts.Timeout
	}
	if opts.Retries > 0 {
		pkgDefaults.Retries = opts.Retries
	}
	if opts.RetryWait > 0 {
		pkgDefaults.RetryWait = opts.RetryWait
	}
	if opts.RetryMaxWait > 0 {
		pkgDefaults.RetryMaxWait = opts.RetryMaxWait
	}
}

// currentDefaults returns a snapshot of the process-wide defaults.
func currentDefaults() Options {
	mu.RLock()
	defer mu.RUnlock()
	return pkgDefaults
}

// SetUserAgent stamps the build version onto the outbound User-Agent (e.g.
// "redswitchboard/0.1.0"). Called once at startup, like SetDefaults, before any
// source opens. A blank version leaves the bare app name.
func SetUserAgent(version string) {
	mu.Lock()
	defer mu.Unlock()
	if version == "" {
		userAgent = defaultUserAgent
		return
	}
	userAgent = defaultUserAgent + "/" + version
}

// currentUserAgent returns the configured outbound User-Agent.
func currentUserAgent() string {
	mu.RLock()
	defer mu.RUnlock()
	return userAgent
}

// New returns a *resty.Client rooted at baseURL with redswitchboard's standard
// defaults applied. baseURL may be the full endpoint (then call with an empty or
// relative path) or an API root (then call with absolute paths). timeout is a
// per-request backstop in addition to any context deadline; a non-positive
// timeout falls back to the configured default. The retry/backoff behavior comes
// from the process-wide defaults (see SetDefaults). When debug is true resty logs
// each request and response.
func New(baseURL string, timeout time.Duration, debug bool) *resty.Client {
	opts := currentDefaults()
	opts.Timeout = timeout // NewWithOptions fills a non-positive timeout from defaults
	opts.Debug = debug
	return NewWithOptions(baseURL, opts)
}

// NewWithOptions returns a *resty.Client rooted at baseURL configured from opts.
// Any zero/non-positive field in opts is filled from the process-wide defaults,
// so callers may set only the fields they care about.
func NewWithOptions(baseURL string, opts Options) *resty.Client {
	d := currentDefaults()
	if opts.Timeout <= 0 {
		opts.Timeout = d.Timeout
	}
	if opts.Retries <= 0 {
		opts.Retries = d.Retries
	}
	if opts.RetryWait <= 0 {
		opts.RetryWait = d.RetryWait
	}
	if opts.RetryMaxWait <= 0 {
		opts.RetryMaxWait = d.RetryMaxWait
	}
	return resty.New().
		SetBaseURL(baseURL).
		SetHeader("User-Agent", currentUserAgent()).
		SetTimeout(opts.Timeout).
		SetRetryCount(opts.Retries).
		SetRetryWaitTime(opts.RetryWait).
		SetRetryMaxWaitTime(opts.RetryMaxWait).
		SetDebug(opts.Debug)
}
