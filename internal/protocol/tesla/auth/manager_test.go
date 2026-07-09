package auth

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tokenServer is a mock OAuth token endpoint recording every refresh_token grant
// it serves. status/expiresIn are configurable; each new access token is unique so
// callers can observe a rotation.
type tokenServer struct {
	*httptest.Server
	hits      atomic.Int32
	status    int
	expiresIn int64
	delay     time.Duration
	mu        sync.Mutex
	lastForm  map[string]string
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{status: http.StatusOK, expiresIn: 3600}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ts.delay > 0 {
			time.Sleep(ts.delay)
		}
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.lastForm = map[string]string{
			"grant_type":    r.PostFormValue("grant_type"),
			"client_id":     r.PostFormValue("client_id"),
			"refresh_token": r.PostFormValue("refresh_token"),
			"scope":         r.PostFormValue("scope"),
		}
		ts.mu.Unlock()
		n := ts.hits.Add(1)
		if ts.status != http.StatusOK {
			w.WriteHeader(ts.status)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access-`+itoa(n)+`","refresh_token":"refresh-`+itoa(n)+`","expires_in":`+itoa64(ts.expiresIn)+`,"token_type":"Bearer"}`)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func itoa(n int32) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func testManager(srv *tokenServer, c *Creds) *TokenManager {
	c.applyDefaults()
	if srv != nil {
		c.TokenURL = srv.URL
	}
	return newManager("/tmp/does-not-exist.json", c, log.New(io.Discard, "", 0))
}

func TestRefreshExchangeBuildsGrantAndParses(t *testing.T) {
	srv := newTokenServer(t)
	m := testManager(srv, &Creds{AccessToken: "old", RefreshToken: "r0", ClientID: "my-client"})

	tok, err := m.RefreshAfter401(context.Background(), "old")
	if err != nil {
		t.Fatalf("RefreshAfter401: %v", err)
	}
	if tok != "access-1" {
		t.Errorf("token = %q, want access-1", tok)
	}

	srv.mu.Lock()
	form := srv.lastForm
	srv.mu.Unlock()
	if form["grant_type"] != "refresh_token" || form["client_id"] != "my-client" || form["refresh_token"] != "r0" {
		t.Errorf("grant body wrong: %+v", form)
	}
	if form["scope"] != defaultScope {
		t.Errorf("scope = %q, want %q", form["scope"], defaultScope)
	}

	// The rotated refresh token is persisted in memory for the next exchange.
	if got := m.snapshot().RefreshToken; got != "refresh-1" {
		t.Errorf("refresh token not rotated: %q", got)
	}
}

func TestRefreshAfter401SingleFlight(t *testing.T) {
	srv := newTokenServer(t)
	srv.delay = 30 * time.Millisecond // widen the window so goroutines pile up
	m := testManager(srv, &Creds{AccessToken: "stale", RefreshToken: "r0", ClientID: "c"})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.RefreshAfter401(context.Background(), "stale"); err != nil {
				t.Errorf("RefreshAfter401: %v", err)
			}
		}()
	}
	wg.Wait()

	if hits := srv.hits.Load(); hits != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1 (single-flight)", hits)
	}
}

func TestRefreshAfter401StaleMismatchSkipsNetwork(t *testing.T) {
	srv := newTokenServer(t)
	m := testManager(srv, &Creds{AccessToken: "current", RefreshToken: "r0", ClientID: "c"})

	// Caller failed on an already-superseded token: nothing to do, no network call.
	tok, err := m.RefreshAfter401(context.Background(), "some-older-token")
	if err != nil {
		t.Fatalf("RefreshAfter401: %v", err)
	}
	if tok != "current" {
		t.Errorf("token = %q, want current", tok)
	}
	if hits := srv.hits.Load(); hits != 0 {
		t.Errorf("token endpoint hit %d times, want 0", hits)
	}
}

func TestRefreshDisabledWithoutClientID(t *testing.T) {
	srv := newTokenServer(t)
	m := testManager(srv, &Creds{AccessToken: "static", RefreshToken: "r0"}) // no client_id

	_, err := m.RefreshAfter401(context.Background(), "static")
	if !errors.Is(err, ErrRefreshDisabled) {
		t.Errorf("err = %v, want ErrRefreshDisabled", err)
	}
	if hits := srv.hits.Load(); hits != 0 {
		t.Errorf("read-only manager made %d network calls, want 0", hits)
	}
}

func TestRefreshTerminalErrorIsClassified(t *testing.T) {
	srv := newTokenServer(t)
	srv.status = http.StatusBadRequest // invalid_grant: revoked refresh token
	m := testManager(srv, &Creds{AccessToken: "old", RefreshToken: "dead", ClientID: "c"})

	_, err := m.RefreshAfter401(context.Background(), "old")
	var re *RefreshError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *RefreshError", err)
	}
	if !re.Terminal() {
		t.Errorf("Terminal() = false for status %d, want true", re.Status)
	}
}

func TestProactiveLoopRefreshesBeforeExpiry(t *testing.T) {
	srv := newTokenServer(t)
	srv.expiresIn = 1 // short, so the reschedule stays brisk
	m := testManager(srv, &Creds{
		AccessToken:  "old",
		RefreshToken: "r0",
		ClientID:     "c",
		ExpiresAt:    time.Now().Add(200 * time.Millisecond).Unix(),
	})
	go m.loop()
	defer m.Stop()

	// 75% of 200ms is ~150ms, so a refresh should land well within 500ms.
	deadline := time.After(600 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatalf("no proactive refresh within 600ms (hits=%d)", srv.hits.Load())
		case <-time.After(20 * time.Millisecond):
			if srv.hits.Load() >= 1 && m.snapshot().AccessToken != "old" {
				return
			}
		}
	}
}

func TestProactiveLoopStopsOnTerminalError(t *testing.T) {
	srv := newTokenServer(t)
	srv.status = http.StatusBadRequest
	m := testManager(srv, &Creds{
		AccessToken:  "old",
		RefreshToken: "dead",
		ClientID:     "c",
		ExpiresAt:    time.Now().Add(100 * time.Millisecond).Unix(),
	})
	m.retryFloor = 10 * time.Millisecond
	go m.loop()
	defer m.Stop()

	// The loop should refresh once, get a terminal 400, and stop, NOT keep hammering.
	time.Sleep(300 * time.Millisecond)
	if hits := srv.hits.Load(); hits != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (loop must stop on terminal error)", hits)
	}
}
