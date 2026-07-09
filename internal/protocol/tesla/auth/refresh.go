package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-resty/resty/v2"
)

// tokenResponse is the OAuth token endpoint's reply to a refresh_token grant.
// Tesla may rotate the refresh token, so RefreshToken is read back and persisted
// when present.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// RefreshError is a non-2xx response from the token endpoint. A 400/401 is
// terminal: the refresh token itself is rejected (revoked or wrong client_id), so
// retrying it is pointless and the operator must mint fresh creds out of band.
type RefreshError struct {
	Status int
	Body   string
}

func (e *RefreshError) Error() string {
	return fmt.Sprintf("tesla auth: token refresh failed (status=%d): %s", e.Status, e.Body)
}

// Terminal reports a rejected refresh token (vs a transient/server error worth
// retrying). 400 invalid_grant and 401 mean the token will never work again.
func (e *RefreshError) Terminal() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized
}

// exchangeRefreshToken runs the OAuth refresh_token grant against c.TokenURL and
// returns the new tokens. The recipe (grant_type/client_id/refresh_token/scope,
// form-encoded) matches TeslaMate's Tesla.Auth.Refresh.
func exchangeRefreshToken(ctx context.Context, client *resty.Client, c *Creds) (*tokenResponse, error) {
	var out tokenResponse
	resp, err := client.R().
		SetContext(ctx).
		SetFormData(map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     c.ClientID,
			"refresh_token": c.RefreshToken,
			"scope":         c.Scope,
		}).
		SetResult(&out).
		Post(c.TokenURL)
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, &RefreshError{Status: resp.StatusCode(), Body: resp.String()}
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("tesla auth: refresh response carried no access_token")
	}
	return &out, nil
}
