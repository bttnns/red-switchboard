package auth

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/bttnns/red-switchboard/internal/transport/restclient"
	"github.com/go-resty/resty/v2"
)

const (
	// refreshTimeout caps a single token exchange.
	refreshTimeout = 30 * time.Second
	// defaultRetryFloor is the wait between proactive refresh attempts after a
	// transient failure (TeslaMate's 5m floor). A field on the manager so tests
	// can shrink it.
	defaultRetryFloor = 5 * time.Minute
	// defaultTokenLifetime is the assumed access-token lifetime when the token
	// endpoint returns a non-positive expires_in. Tesla access tokens last 8h; the
	// fallback keeps the proactive refresh schedule sane so a degenerate (zero/
	// missing) expires_in cannot collapse nextRefreshDelay to 0 and hammer the
	// endpoint.
	defaultTokenLifetime = 8 * time.Hour
)

// ErrRefreshDisabled is returned by RefreshAfter401 when the creds carry no
// client_id/refresh_token: there is nothing to exchange, so the caller should
// surface the original UNAUTHENTICATED and let the operator re-mint creds.
var ErrRefreshDisabled = errors.New("tesla auth: refresh disabled (no client_id/refresh_token in creds)")

// TokenManager centrally owns one creds file's access token. It is shared by every
// consumer of that file (keyed by path in the registry below), serves the current
// bearer via Token, and keeps it live with a proactive background refresh plus a
// reactive RefreshAfter401. Safe for concurrent use.
type TokenManager struct {
	path   string
	logger *log.Logger
	http   *resty.Client

	mu    sync.Mutex // guards creds + ttl (short critical sections only)
	creds *Creds
	ttl   time.Duration // last known access-token lifetime, set on each refresh

	refreshMu sync.Mutex // serializes token exchanges (single-flight)

	retryFloor time.Duration
	stop       chan struct{}
	stopOnce   sync.Once
}

var (
	regMu    sync.Mutex
	registry = map[string]*TokenManager{}
)

// Shared returns the TokenManager for credsFile, creating and starting it on first
// reference and returning the identical instance for every later consumer of the
// same file. The first reference's proactive refresh goroutine is the only one;
// subsequent consumers just share the token.
func Shared(credsFile string, logger *log.Logger) (*TokenManager, error) {
	if logger == nil {
		logger = log.Default()
	}
	abs, err := filepath.Abs(credsFile)
	if err != nil {
		abs = credsFile
	}

	regMu.Lock()
	defer regMu.Unlock()
	if m, ok := registry[abs]; ok {
		return m, nil
	}
	creds, err := LoadCreds(abs)
	if err != nil {
		return nil, err
	}
	m := newManager(abs, creds, logger)
	if creds.canRefresh() {
		go m.loop()
	}
	registry[abs] = m
	return m, nil
}

// newManager builds a manager without starting its goroutine (used directly by
// tests; Shared wraps it with the registry and the proactive loop).
func newManager(path string, creds *Creds, logger *log.Logger) *TokenManager {
	return &TokenManager{
		path:       path,
		logger:     logger,
		http:       restclient.New("", refreshTimeout, false),
		creds:      creds,
		retryFloor: defaultRetryFloor,
		stop:       make(chan struct{}),
	}
}

// Token returns the current access token. It never does network I/O (refresh is
// driven by the background loop and RefreshAfter401), so the hot polling path
// stays lock-and-return.
func (m *TokenManager) Token(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creds.AccessToken, nil
}

// RefreshAfter401 is the reactive path: a consumer that just got a 401 with token
// `stale` asks for a fresh one. It is single-flight and compare-and-swap deduped,
// so concurrent 401s from many vehicles trigger exactly one exchange and the rest
// get the already-refreshed token without a network call.
func (m *TokenManager) RefreshAfter401(ctx context.Context, stale string) (string, error) {
	return m.refresh(ctx, stale)
}

// Stop halts the proactive refresh goroutine. Idempotent; for tests/shutdown.
func (m *TokenManager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
}

// refresh exchanges the refresh token for a new access token. When stale is
// non-empty it first re-checks under refreshMu and returns the current token
// unchanged if another caller already refreshed past stale (the CAS dedupe). The
// network exchange runs on a snapshot, NOT under m.mu, so Token stays responsive
// during a refresh.
func (m *TokenManager) refresh(ctx context.Context, stale string) (string, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	cur := m.snapshot()
	if !cur.canRefresh() {
		return "", ErrRefreshDisabled
	}
	if stale != "" && cur.AccessToken != stale {
		return cur.AccessToken, nil // someone already refreshed
	}

	resp, err := exchangeRefreshToken(ctx, m.http, cur)
	if err != nil {
		return "", err
	}

	// A non-positive expires_in (a malformed/omitting token endpoint) would set
	// ExpiresAt to ~now and ttl to 0, collapsing nextRefreshDelay to 0 so the
	// proactive loop refreshes back-to-back. Fall back to the standard lifetime.
	ttlSec := resp.ExpiresIn
	if ttlSec <= 0 {
		ttlSec = int64(defaultTokenLifetime / time.Second)
	}
	ttl := time.Duration(ttlSec) * time.Second

	m.mu.Lock()
	m.creds.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		m.creds.RefreshToken = resp.RefreshToken // Tesla rotates refresh tokens
	}
	m.creds.ExpiresAt = time.Now().Add(ttl).Unix()
	m.ttl = ttl
	toSave := *m.creds
	m.mu.Unlock()

	if err := SaveCreds(m.path, &toSave); err != nil {
		// Non-fatal: the new token is live in memory; only a restart would lose it.
		m.logger.Printf("tesla auth: refreshed token but could not persist to %s: %v", m.path, err)
	}
	return resp.AccessToken, nil
}

// snapshot returns a copy of the current creds under the short lock.
func (m *TokenManager) snapshot() *Creds {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *m.creds
	return &c
}

// loop is the proactive refresher: it sleeps until ~75% of the token lifetime has
// elapsed, refreshes, and reschedules off the new lifetime. A transient failure
// retries after retryFloor; a terminal failure (rejected refresh token) stops the
// loop and leaves recovery to the reactive path + operator re-mint.
func (m *TokenManager) loop() {
	for {
		select {
		case <-m.stop:
			return
		case <-time.After(m.nextRefreshDelay()):
		}

		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		_, err := m.refresh(ctx, "")
		cancel()
		if err == nil {
			continue
		}

		var re *RefreshError
		if errors.As(err, &re) && re.Terminal() {
			m.logger.Printf("tesla auth: refresh token rejected for %s (%v); re-mint creds out of band. Stopping proactive refresh.", m.path, err)
			return
		}
		m.logger.Printf("tesla auth: proactive refresh failed for %s (retrying in %s): %v", m.path, m.retryFloor, err)
		select {
		case <-m.stop:
			return
		case <-time.After(m.retryFloor):
		}
	}
}

// nextRefreshDelay returns how long to wait before the next proactive refresh:
// 75% of the token lifetime (refresh once a quarter of it remains). With the
// lifetime not yet known (creds carried no expires_at), it returns 0 so the loop
// refreshes immediately to discover it.
func (m *TokenManager) nextRefreshDelay() time.Duration {
	m.mu.Lock()
	exp, ttl := m.creds.ExpiresAt, m.ttl
	m.mu.Unlock()

	if exp == 0 {
		return 0
	}
	remaining := time.Until(time.Unix(exp, 0))
	if ttl <= 0 {
		ttl = remaining // first schedule from a file: assume we are near token start
	}
	delay := remaining - ttl/4
	if delay < 0 {
		delay = 0
	}
	return delay
}
