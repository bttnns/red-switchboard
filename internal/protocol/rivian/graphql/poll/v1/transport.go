package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bttnns/red-switchboard/internal/transport/restclient"
	"github.com/go-resty/resty/v2"
)

// transport is a Rivian HTTP client bound to a single GraphQL endpoint. All
// Rivian calls are POSTs of a JSON body to baseURL, so the request shape is
// fixed and only the body/headers vary. It composes the standardized resty
// client (internal/transport/restclient); resty transparently decompresses gzip and
// applies the shared retry/backoff.
type transport struct {
	rc      *resty.Client
	baseURL string
	debug   bool
}

// newTransport creates a transport for the given GraphQL endpoint. The resty
// client carries a 30s timeout as a backstop in addition to the per-request
// context, and sends the Rivian default headers on every request.
func newTransport(baseURL string, debug bool) *transport {
	rc := restclient.New(baseURL, 30*time.Second, debug).SetHeaders(DefaultHeaders)
	return &transport{rc: rc, baseURL: baseURL, debug: debug}
}

// do POSTs body to the endpoint with the given headers, honoring ctx, and
// decodes the JSON response into result. GraphQL-level errors are surfaced as
// *APIError regardless of HTTP status.
func (t *transport) do(ctx context.Context, body string, headers map[string]string, result any) error {
	resp, err := t.rc.R().
		SetContext(ctx).
		SetHeaders(headers).
		SetBody(body).
		Post("")
	if err != nil {
		return fmt.Errorf("error making request: %v", err)
	}
	respBody := resp.Body()
	status := resp.StatusCode()

	// Surface GraphQL-level errors regardless of HTTP status: a Rivian GraphQL
	// error (e.g. UNAUTHENTICATED, RATE_LIMIT) typically still returns HTTP 200
	// with a populated `errors[]` array. Parse that first so callers can branch
	// on the error code.
	if apiErr := parseGraphQLError(respBody, status); apiErr != nil {
		return apiErr
	}

	if status != http.StatusOK {
		return &APIError{HTTPStatus: status}
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("error decoding response: %v", err)
		}
	}

	return nil
}

// parseGraphQLError inspects a response body for a GraphQL `errors[]` array and
// returns an *APIError carrying the first error's code/reason, or nil if there
// are no GraphQL errors. An HTTP 401 with no GraphQL errors is also reported so
// callers can treat it as an auth failure.
func parseGraphQLError(body []byte, status int) *APIError {
	var envelope struct {
		Errors []GraphQLError `json:"errors"`
	}
	// Ignore unmarshal errors: a non-JSON body simply has no GraphQL errors.
	_ = json.Unmarshal(body, &envelope)

	if len(envelope.Errors) == 0 {
		if status == http.StatusUnauthorized {
			return &APIError{HTTPStatus: status}
		}
		return nil
	}

	first := envelope.Errors[0]
	return &APIError{
		Code:       first.Extensions.Code,
		Reason:     first.Extensions.Reason,
		HTTPStatus: status,
		Errors:     envelope.Errors,
	}
}
