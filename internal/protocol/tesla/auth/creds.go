// Package auth is the central owner of Tesla OAuth credentials. Every Tesla
// consumer (Fleet poll, Owner poll, Owner stream, the command plugin) reads the
// same creds file, so one TokenManager per file owns the access token: it serves
// the current bearer to all consumers, refreshes it proactively before expiry and
// reactively on a 401, and persists rotated tokens back to the file. Centralizing
// here also removes a latent bug where independent loaders could each refresh and
// race-write the same file.
//
// The creds file is self-describing: alongside the tokens it carries the OAuth
// parameters needed to refresh (client_id, token_url, scope), so refresh needs no
// per-plugin config and blocks sharing a file cannot disagree.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
)

const (
	// defaultTokenURL is Tesla's OAuth token endpoint (global; the access token's
	// audience, not this URL, selects the regional Fleet data host).
	defaultTokenURL = "https://auth.tesla.com/oauth2/v3/token"
	// defaultScope is the scope a refresh_token grant needs; offline_access is what
	// makes the refresh token usable.
	defaultScope = "openid email offline_access"
)

// Creds holds the Tesla OAuth tokens plus the parameters needed to refresh them.
// AccessToken is the short-lived bearer sent on every request; RefreshToken is
// exchanged for a new AccessToken; ExpiresAt drives proactive refresh. ClientID
// (with RefreshToken) is what makes refresh possible: with it empty the manager
// runs read-only, serving the static token and never refreshing.
type Creds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is the unix-seconds expiry of the access token. Zero means unknown
	// (the manager then refreshes once on startup to discover the lifetime).
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// ClientID is the OAuth client_id for the refresh_token grant. Fleet uses the
	// registered third-party app id; legacy Owner uses "ownerapi". Empty disables
	// refresh (read-only mode).
	ClientID string `json:"client_id,omitempty"`
	// TokenURL overrides the OAuth token endpoint (defaults to defaultTokenURL).
	// Lets a dev harness point refresh at a mock server.
	TokenURL string `json:"token_url,omitempty"`
	// Scope overrides the refresh scope (defaults to defaultScope).
	Scope string `json:"scope,omitempty"`
}

// applyDefaults fills the optional OAuth fields so the manager always has a usable
// token URL and scope.
func (c *Creds) applyDefaults() {
	if c.TokenURL == "" {
		c.TokenURL = defaultTokenURL
	}
	if c.Scope == "" {
		c.Scope = defaultScope
	}
}

// canRefresh reports whether the creds carry what a refresh_token grant needs.
// Without it the manager serves the static access token unchanged.
func (c *Creds) canRefresh() bool {
	return c.ClientID != "" && c.RefreshToken != ""
}

// LoadCreds reads and decodes Creds from a creds file (plain JSON). A missing
// file, malformed JSON, or empty access token is an error so a consumer fails
// fast at construction rather than on the first request.
func LoadCreds(path string) (*Creds, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading creds file: %w", err)
	}
	var c Creds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("error parsing creds file: %w", err)
	}
	if c.AccessToken == "" {
		return nil, fmt.Errorf("no access token in creds file %q", path)
	}
	c.applyDefaults()
	return &c, nil
}

// SaveCreds writes Creds back to the creds file (indented JSON, 0600) after a
// refresh so a restart resumes with a live access token and the rotated refresh
// token.
func SaveCreds(path string, c *Creds) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling creds: %w", err)
	}
	// Write atomically (temp file + rename) so a crash mid-write cannot truncate the
	// creds file: LoadCreds rejects a partial file ("no access token"), which would
	// leave every Tesla consumer unable to construct on the next restart.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return fmt.Errorf("error writing creds file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("error replacing creds file: %w", err)
	}
	return nil
}
