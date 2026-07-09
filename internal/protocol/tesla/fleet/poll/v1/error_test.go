package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/go-resty/resty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestErrorEnvelope verifies we emit the real Tesla Fleet API error shape,
// {"response":null,"error":...,"error_description":...}, so non-TeslaMate
// consumers that parse the envelope behave correctly.
func TestErrorEnvelope(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/1/vehicles/999/vehicle_data", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	// All three envelope keys must be present...
	assert.Contains(t, body, "response")
	assert.Contains(t, body, "error")
	assert.Contains(t, body, "error_description")
	// ...with response null and a machine-readable error code.
	assert.JSONEq(t, `null`, string(body["response"]))
	assert.JSONEq(t, `"not_found"`, string(body["error"]))
}

// TestMethodNotAllowed: chi returns 405 for the wrong method on a known route,
// so we no longer hand-check methods in handlers.
func TestMethodNotAllowed(t *testing.T) {
	h := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/1/products", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// resp builds a resty.Response stand-in for NewSourceError: RawResponse carries
// status/header (read by StatusCode/Status/Header/IsError) and SetBody carries
// the body (read by String).
func resp(t *testing.T, status int, body string, header http.Header) *resty.Response {
	t.Helper()
	if header == nil {
		header = http.Header{}
	}
	r := &resty.Response{RawResponse: &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     header,
	}}
	r.SetBody([]byte(body))
	return r
}

// TestSourceErrorClassification403 covers the 403 taxonomy: the
// "account disabled: EXCEEDED_LIMIT" family is a quota block that also wipes
// the telemetry config, while an unreachable-car 403 is NOT a quota block.
func TestSourceErrorClassification403(t *testing.T) {
	t.Run("quota block EXCEEDED_LIMIT", func(t *testing.T) {
		e := NewSourceError("tesla fleet api", resp(t, http.StatusForbidden,
			`{"error":"account disabled: EXCEEDED_LIMIT"}`, nil))
		assert.True(t, source.IsQuotaBlocked(e), "EXCEEDED_LIMIT 403 is a quota block")
		assert.True(t, source.IsAccountDisabled(e))
		assert.True(t, source.TelemetryConfigWiped(e), "EXCEEDED_LIMIT wipes telemetry config")
	})
	t.Run("quota block no telemetry wipe", func(t *testing.T) {
		e := NewSourceError("tesla fleet api", resp(t, http.StatusForbidden,
			`{"error":"account disabled: other reason"}`, nil))
		assert.True(t, source.IsQuotaBlocked(e), "account disabled 403 is a quota block")
		assert.False(t, source.TelemetryConfigWiped(e), "only EXCEEDED_LIMIT wipes telemetry")
	})
	t.Run("vehicle unavailable is not a quota block", func(t *testing.T) {
		e := NewSourceError("tesla fleet api", resp(t, http.StatusForbidden,
			`{"error":"vehicle unavailable"}`, nil))
		assert.False(t, source.IsQuotaBlocked(e))
		assert.False(t, source.TelemetryConfigWiped(e))
	})
}

// TestSourceErrorRetryAfterHonored: a server Retry-After (which can be hours) is
// parsed and surfaced; the poll layer honors it over its own backoff.
func TestSourceErrorRetryAfterHonored(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "10800") // 3 hours, as delta-seconds
	e := NewSourceError("tesla fleet api", resp(t, http.StatusTooManyRequests, `{}`, h))
	d, ok := source.RetryAfter(e)
	require.True(t, ok)
	assert.Equal(t, 3*time.Hour, d)
	assert.True(t, source.IsRateLimited(e))
}

// TestSourceErrorUnauthenticated: 401 classifies as needing re-login.
func TestSourceErrorUnauthenticated(t *testing.T) {
	e := NewSourceError("tesla fleet api", resp(t, http.StatusUnauthorized, `{}`, nil))
	assert.True(t, source.IsUnauthenticated(e))
	assert.False(t, source.IsQuotaBlocked(e))
}
