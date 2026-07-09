package v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLiveSessionStringScalars pins the validation-found bug: the real Rivian
// charging endpoint sends timeElapsed (and sometimes currentPrice) as JSON
// strings, which a bare numeric field would fail to decode. flexFloat tolerates
// both forms so the whole getLiveSessionData response still parses.
func TestLiveSessionStringScalars(t *testing.T) {
	body := []byte(`{"data":{"getLiveSessionData":{
		"__typename":"LiveSessionData",
		"vehicleChargerState":{"value":"charging_active","updatedAt":"2026-06-15T12:00:00Z"},
		"power":{"value":175,"updatedAt":"2026-06-15T12:00:00Z"},
		"soc":{"value":"55","updatedAt":"2026-06-15T12:00:00Z"},
		"timeRemaining":{"value":"9651","updatedAt":"2026-06-15T12:00:00Z"},
		"timeElapsed":"2229",
		"currentPrice":"12.5",
		"currentCurrency":"USD"
	}}}`)

	var raw rawLiveSessionResponse
	require.NoError(t, json.Unmarshal(body, &raw))
	require.NotNil(t, raw.Data.Session)
	ls := raw.Data.Session.flatten()
	assert.Equal(t, 2229, ls.TimeElapsed, "timeElapsed sent as a string must decode")
	assert.InDelta(t, 12.5, ls.CurrentPrice, 0.001, "currentPrice sent as a string must decode")
	assert.Equal(t, "USD", ls.CurrentCurrency)
	assert.InDelta(t, 175.0, ls.Power, 0.001)
	assert.Equal(t, 9651, ls.TimeRemaining)
}
