// Package rivian is the Rivian API client used by the translator. It fetches a
// vehicle snapshot from the gateway GraphQL endpoint (vehicleState) and the
// live charging session from the charging GraphQL endpoint (getLiveSessionData),
// parses the raw `{ timeStamp value }` wrappers into clean typed Go structs, and
// transparently refreshes its short-lived session tokens (minting a fresh CSRF +
// app-session token, since Rivian has no refresh mutation) on an UNAUTHENTICATED
// error. The long-lived creds file it reads is minted out of band by a separate
// login tool (see github.com/bttnns/rivian_auth).
package v1

import (
	"context"
	"fmt"
	"sync"
)

// Client is a Rivian API client bound to a creds file minted out of band by a
// separate login tool (github.com/bttnns/rivian_auth). It is safe for concurrent use.
type Client struct {
	credsPath string
	debug     bool

	gateway  *transport // https://rivian.com/api/gql/gateway/graphql
	charging *transport // https://rivian.com/api/gql/chrg/user/graphql
	session  *sessionRefresher

	mu   sync.Mutex
	data *AuthData
}

// sessionRefresher wraps the session-token refresh so it can be swapped in tests.
type sessionRefresher struct {
	refresh func(username, credsPath string) error
}

// New constructs a Client from a creds-file path, talking to the Rivian GraphQL
// API rooted at baseURL (empty falls back to the production root; point it at a
// mock instance for local dev). The creds file is the base64-encoded JSON
// minted out of band by a separate login tool (see github.com/bttnns/rivian_auth),
// read immediately so construction fails fast if it is missing or malformed.
func New(credsPath, baseURL string, debug bool) (*Client, error) {
	data, err := LoadCreds(credsPath)
	if err != nil {
		return nil, err
	}
	return &Client{
		credsPath: credsPath,
		debug:     debug,
		gateway:   newTransport(GatewayURL(baseURL), debug),
		charging:  newTransport(ChargingURL(baseURL), debug),
		session:   defaultSessionRefresher(baseURL, debug),
		data:      data,
	}, nil
}

// defaultSessionRefresher returns the production refresh closure: it mints a
// fresh CSRF + app-session token for the stored session and rewrites the creds
// file. Rivian has no refresh mutation, so this is the credential-free way to
// recover an expired app-session/CSRF; a revoked user session needs a fresh
// creds file minted out of band (see github.com/bttnns/rivian_auth).
func defaultSessionRefresher(baseURL string, debug bool) *sessionRefresher {
	return &sessionRefresher{
		refresh: func(username, credsPath string) error {
			return refreshSession(baseURL, debug, username, credsPath)
		},
	}
}

// ---- identity accessors ---------------------------------------------------

// Vehicles returns the account vehicle list captured at auth (one login is
// account-level, so this can be more than one car).
func (c *Client) Vehicles() []Vehicle {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.VehicleList()
}

// VehicleGUID returns the first vehicle's GUID (back-compat single-vehicle helper).
func (c *Client) VehicleGUID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.VehicleID
}

// ---- data calls -----------------------------------------------------------

// VehicleState fetches and parses the gateway vehicleState snapshot for the given
// vehicle GUID. The session tokens are account-level, so the same client serves
// every vehicle; only the query's id varies. On an UNAUTHENTICATED GraphQL error
// (or HTTP 401) it refreshes the session once and retries.
func (c *Client) VehicleState(ctx context.Context, guid string) (*VehicleState, error) {
	var raw rawVehicleStateResponse
	err := c.callWithRefresh(ctx, func(ctx context.Context, a *AuthData) error {
		return c.gateway.do(ctx, vehicleStateQuery(guid), gatewayHeaders(a), &raw)
	})
	if err != nil {
		return nil, err
	}
	return raw.flatten(), nil
}

// LiveSession fetches and parses the live charging session for the given vehicle
// GUID. The bool result is true when a session is active; when there is no active
// session it returns (nil, false, nil) and the caller should fall back to
// VehicleState.ChargerState / ChargerStatus. Re-authenticates once on
// UNAUTHENTICATED.
func (c *Client) LiveSession(ctx context.Context, guid string) (*LiveSessionData, bool, error) {
	var raw rawLiveSessionResponse
	err := c.callWithRefresh(ctx, func(ctx context.Context, a *AuthData) error {
		return c.charging.do(ctx, liveSessionQuery(guid), chargingHeaders(a), &raw)
	})
	if err != nil {
		return nil, false, err
	}
	if raw.Data.Session == nil {
		return nil, false, nil
	}
	return raw.Data.Session.flatten(), true, nil
}

// callWithRefresh runs fn with the current creds; on an UNAUTHENTICATED error it
// refreshes the session once (minting a fresh CSRF + app-session token),
// persists the refreshed creds, and retries fn exactly once.
func (c *Client) callWithRefresh(ctx context.Context, fn func(context.Context, *AuthData) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	data := c.data
	c.mu.Unlock()

	err := fn(ctx, data)
	if err == nil {
		return nil
	}
	if !IsUnauthenticated(err) {
		return err
	}

	// Refresh the session (no refresh mutation exists) then retry once.
	if rerr := c.refreshAndReload(); rerr != nil {
		return fmt.Errorf("session refresh after UNAUTHENTICATED failed: %w (original: %v)", rerr, err)
	}

	c.mu.Lock()
	data = c.data
	c.mu.Unlock()

	return fn(ctx, data)
}

// refreshAndReload mints fresh session tokens for the stored username, rewrites
// the creds file, and reloads it into the client.
func (c *Client) refreshAndReload() error {
	c.mu.Lock()
	username := c.data.Username
	c.mu.Unlock()

	if err := c.session.refresh(username, c.credsPath); err != nil {
		return err
	}
	data, err := LoadCreds(c.credsPath)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.data = data
	c.mu.Unlock()
	return nil
}
