package v1

import (
	"fmt"
	"strings"
)

// GraphQLError represents a single error object in a GraphQL `errors[]` array.
type GraphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"extensions"`
}

// APIError is returned when a Rivian GraphQL response contains an `errors[]`
// array. It carries the first error code so callers can branch on it (e.g.
// UNAUTHENTICATED to trigger a re-auth, RATE_LIMIT to back off).
type APIError struct {
	// Code is the GraphQL extensions.code of the first error (e.g.
	// "UNAUTHENTICATED", "RATE_LIMIT", "BAD_USER_INPUT").
	Code string
	// Reason is the GraphQL extensions.reason of the first error, if any.
	Reason string
	// HTTPStatus is the HTTP status code of the response (200 for a typical
	// GraphQL error, 401 for an HTTP-level auth failure).
	HTTPStatus int
	// Errors holds every error object returned by the API.
	Errors []GraphQLError
}

// Error code constants for the codes callers commonly branch on.
const (
	ErrCodeUnauthenticated = "UNAUTHENTICATED"
	ErrCodeRateLimit       = "RATE_LIMIT"
)

func (e *APIError) Error() string {
	msgs := make([]string, 0, len(e.Errors))
	for _, ge := range e.Errors {
		msgs = append(msgs, ge.Message)
	}
	return fmt.Sprintf("rivian api error (code=%s status=%d): %s",
		e.Code, e.HTTPStatus, strings.Join(msgs, "; "))
}

// Unauthenticated reports whether this error is an auth failure. It satisfies
// the vendor-agnostic source.unauthenticated interface so the generic poll layer
// can classify it without importing the rivian package.
func (e *APIError) Unauthenticated() bool {
	return e.Code == ErrCodeUnauthenticated || e.HTTPStatus == 401
}

// RateLimited reports whether this error is a rate-limit. It satisfies the
// vendor-agnostic source.rateLimited interface.
func (e *APIError) RateLimited() bool { return e.Code == ErrCodeRateLimit }

// IsUnauthenticated reports whether err is an APIError carrying an
// UNAUTHENTICATED code (the signal to re-authenticate) or an HTTP 401.
func IsUnauthenticated(err error) bool {
	if ae, ok := err.(*APIError); ok {
		return ae.Code == ErrCodeUnauthenticated || ae.HTTPStatus == 401
	}
	return false
}

// IsRateLimited reports whether err is an APIError carrying a RATE_LIMIT code.
func IsRateLimited(err error) bool {
	if ae, ok := err.(*APIError); ok {
		return ae.Code == ErrCodeRateLimit
	}
	return false
}
