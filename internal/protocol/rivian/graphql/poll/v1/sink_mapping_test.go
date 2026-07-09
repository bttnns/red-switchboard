package v1

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleState is a fully populated canonical state used as the round-trip seed.
// It exercises every load-bearing field the encode/decode pair must preserve:
// power/gear/charger/plug enums, battery %, odometer/range, and location.
func sampleState(now time.Time) *vehicle.State {
	return &vehicle.State{
		Power:          vehicle.PowerOnline,
		UserPresent:    true,
		CloudOnline:    true,
		LastUpdate:     now,
		CloudLastSync:  now,
		SpeedMps:       10,
		HeadingDeg:     90,
		OdometerMeters: 16093,
		RangeKm:        161,

		BatteryLevelPct: 78,
		BatteryLimitPct: 90,
		Gear:            vehicle.GearDrive,
		Charger:         vehicle.ChargerCharging,
		Plug:            vehicle.PlugConnected,
		ChargePortOpen:  true,
		Location:        &vehicle.Location{Latitude: 37.5, Longitude: -122.3, TimeStamp: now},
		OtaVersion:      "2026.6.1",
	}
}

func sampleLive() *vehicle.LiveSession {
	return &vehicle.LiveSession{PowerKw: 175, CurrentA: 220, TotalChargedEnergy: 23.4, TimeRemainingSec: 1800}
}

// TestSinkRoundTrip is the mock loop: ENCODE a canonical state+live session into
// the Rivian wire body, then parse it back with the REAL parse.go decoder and
// toCanonical, and assert the load-bearing fields survive. This proves the encode
// map is the inverse of source_mapping.toCanonical and that the wire output is
// accurate enough for the real parser.
func TestSinkRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	in := sampleState(now)
	live := sampleLive()

	// ENCODE -> wire JSON -> decode with the production raw structs.
	vsBody := vehicleStateBody(in)
	liveBody := liveSessionBody(live, now)

	var rawVS rawVehicleStateResponse
	require.NoError(t, json.Unmarshal(mustJSON(t, vsBody), &rawVS))
	var rawLive rawLiveSessionResponse
	require.NoError(t, json.Unmarshal(mustJSON(t, liveBody), &rawLive))
	require.NotNil(t, rawLive.Data.Session, "live session should decode to a session object")

	snap := toCanonical(rawVS.flatten(), rawLive.Data.Session.flatten())
	require.NotNil(t, snap.State)
	out := snap.State

	// Liveness / enums.
	assert.Equal(t, vehicle.PowerOnline, out.Power)
	assert.True(t, out.UserPresent)
	assert.Equal(t, vehicle.GearDrive, out.Gear)
	assert.Equal(t, vehicle.ChargerCharging, out.Charger)
	assert.Equal(t, vehicle.PlugConnected, out.Plug)
	assert.True(t, out.ChargePortOpen)

	// Scalars (Rivian is metric, so values are identical, no conversion).
	assert.Equal(t, in.BatteryLevelPct, out.BatteryLevelPct)
	assert.Equal(t, in.BatteryLimitPct, out.BatteryLimitPct)
	assert.Equal(t, in.OdometerMeters, out.OdometerMeters)
	assert.Equal(t, in.RangeKm, out.RangeKm)
	assert.Equal(t, in.SpeedMps, out.SpeedMps)
	assert.Equal(t, in.OtaVersion, out.OtaVersion)

	// Location.
	require.NotNil(t, out.Location)
	assert.Equal(t, in.Location.Latitude, out.Location.Latitude)
	assert.Equal(t, in.Location.Longitude, out.Location.Longitude)

	// Live session.
	require.NotNil(t, snap.Live)
	assert.Equal(t, live.PowerKw, snap.Live.PowerKw)
	assert.Equal(t, live.CurrentA, snap.Live.CurrentA)
	assert.Equal(t, live.TotalChargedEnergy, snap.Live.TotalChargedEnergy)
	assert.Equal(t, live.TimeRemainingSec, snap.Live.TimeRemainingSec)
}

// TestSinkRoundTripNoLive checks the no-active-session path: a nil live session
// must encode to the {getLiveSessionData:null} shape that parse.go reads as "no
// session", so the loop yields a nil canonical LiveSession.
func TestSinkRoundTripNoLive(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	body := liveSessionBody(nil, now)

	var rawLive rawLiveSessionResponse
	require.NoError(t, json.Unmarshal(mustJSON(t, body), &rawLive))
	assert.Nil(t, rawLive.Data.Session, "nil live session must decode to a null session (no active session)")
}

// fakeProvider is a static sink.Provider for the e2e server test.
type fakeProvider struct {
	ids  []vehicle.Identity
	snap map[string]vehicle.Snapshot
}

func (f *fakeProvider) Vehicles() []vehicle.Identity { return f.ids }
func (f *fakeProvider) Latest(id string) vehicle.Snapshot {
	return f.snap[id]
}
func (f *fakeProvider) Stats(id string) poll.Stats { return poll.Stats{} }

// TestSinkServerEndToEnd serves the sink over httptest and reads it back with the
// REAL Rivian source Client (base_url pointed at the test server), exercising the
// full HTTP -> encode -> wire -> parse -> canonical loop through the production
// client path.
func TestSinkServerEndToEnd(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	const guid = "veh-guid-1"

	prov := &fakeProvider{
		ids: []vehicle.Identity{{ID: guid, VIN: "VIN123", DisplayName: "Test R1T", Make: "Rivian", Model: "R1T"}},
		snap: map[string]vehicle.Snapshot{
			guid: {State: sampleState(now), Live: sampleLive(), FetchedAt: now},
		},
	}

	sk, err := newSink(nil, prov, log.New(os.Stderr, "", 0))
	require.NoError(t, err)
	h, err := sk.Handler()
	require.NoError(t, err)

	srv := httptest.NewServer(h)
	defer srv.Close()

	client := newTestClient(t, srv.URL+"/api/gql", guid)

	vs, err := client.VehicleState(context.Background(), guid)
	require.NoError(t, err)
	snap := toCanonical(vs, nil)
	require.NotNil(t, snap.State)
	assert.Equal(t, vehicle.GearDrive, snap.State.Gear)
	assert.Equal(t, 78.0, snap.State.BatteryLevelPct)
	assert.Equal(t, 16093, snap.State.OdometerMeters)

	ls, active, err := client.LiveSession(context.Background(), guid)
	require.NoError(t, err)
	require.True(t, active, "expected an active live session")
	assert.Equal(t, 175.0, ls.Power)
	assert.Equal(t, 23.4, ls.TotalChargedEnergy)
}

// newTestClient builds a real source Client pointed at baseURL by writing a
// throwaway base64 creds file (New reads it eagerly), so the e2e test drives the
// production client/transport against the sink.
func newTestClient(t *testing.T, baseURL, guid string) *Client {
	t.Helper()
	creds := AuthData{
		Token:            "t",
		UserSessionToken: "u",
		CSRFToken:        "c",
		AppSessionToken:  "a",
		VehicleID:        guid,
		Username:         "test@example.com",
		Vehicles:         []Vehicle{{GUID: guid, VIN: "VIN123", Name: "Test R1T"}},
	}
	raw, err := json.Marshal(creds)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "creds.json")
	require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(raw)), 0600))

	c, err := New(path, baseURL, false)
	require.NoError(t, err)
	return c
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
