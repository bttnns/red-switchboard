package v1

import (
	"context"
	"fmt"
)

// CSRFResponse is the response from the CSRF token mutation. The CSRF and
// app-session tokens it returns are short-lived and refreshed at runtime; the
// long-lived user session token is minted out of band by a separate login tool
// and only read here.
type CSRFResponse struct {
	Data struct {
		CreateCsrfToken struct {
			TypeName        string `json:"__typename"`
			CSRFToken       string `json:"csrfToken"`
			AppSessionToken string `json:"appSessionToken"`
		} `json:"createCsrfToken"`
	} `json:"data"`
}

// refreshSession refreshes the short-lived CSRF and app-session tokens against an
// existing creds file, keeping the user session token. Rivian has no refresh
// mutation, so this credential-free path (mint a fresh CSRF + app-session token
// and persist) is the recovery for app-session/CSRF expiry, mirroring the HA
// coordinator's create_csrf_token refresh. If the user session token itself is
// revoked, data calls keep failing and the operator must mint a fresh creds file
// out of band (see github.com/bttnns/rivian_auth); callers should surface that
// as needs-re-login. username is accepted for parity and future use.
func refreshSession(baseURL string, debug bool, username, credsFile string) error {
	t := newTransport(GatewayURL(baseURL), debug)

	data, err := LoadCreds(credsFile)
	if err != nil {
		return err
	}

	csrf, err := mintCSRF(t)
	if err != nil {
		return fmt.Errorf("session refresh: create csrf token failed: %w", err)
	}
	data.CSRFToken = csrf.Data.CreateCsrfToken.CSRFToken
	data.AppSessionToken = csrf.Data.CreateCsrfToken.AppSessionToken
	if username != "" {
		data.Username = username
	}

	return SaveCreds(credsFile, data)
}

// mintCSRF mints a fresh CSRF token and app session token from the gateway.
func mintCSRF(t *transport) (*CSRFResponse, error) {
	body := marshalGraphQL(
		"CreateCSRFToken",
		"mutation CreateCSRFToken { createCsrfToken { __typename csrfToken appSessionToken } }",
		nil,
	)
	var resp CSRFResponse
	if err := t.do(context.Background(), body, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
