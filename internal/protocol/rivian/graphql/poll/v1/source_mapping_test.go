package v1

import (
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToCanonical locks the rivian -> canonical mapping: enum normalization,
// field renames, and that the cloud/power collapse reproduces the old topState
// behavior.
func TestToCanonical(t *testing.T) {
	t.Parallel()
	now := time.Now()
	vs := &VehicleState{
		CloudConnectionOnline: true,
		PowerState:            "go",
		GearStatus:            "drive",
		ChargerState:          "charging_active",
		ChargerStatus:         "chrgr_sts_connected_charging",
		ChargePortState:       "open",
		VehicleMileage:        16093,
		DistanceToEmpty:       161,
		BatteryLevel:          78,
		BatteryLimit:          90,
		CabinInteriorTemp:     21.5,
		GnssSpeed:             10,
		GnssBearing:           90,
		Location:              &GnssLocation{Latitude: 37.5, Longitude: -122.3, TimeStamp: now},
		OtaCurrentVersion:     "2026.6.1",
		LastUpdate:            now,
	}
	live := &LiveSessionData{Power: 175, Current: 220, TotalChargedEnergy: 23.4, TimeRemaining: 1800}

	snap := toCanonical(vs, live)
	require.NotNil(t, snap.State)
	st := snap.State

	assert.Equal(t, vehicle.PowerOnline, st.Power)
	assert.True(t, st.UserPresent, "powerState=go -> user present")
	assert.Equal(t, vehicle.GearDrive, st.Gear)
	assert.Equal(t, vehicle.ChargerCharging, st.Charger)
	assert.Equal(t, vehicle.PlugConnected, st.Plug)
	assert.True(t, st.ChargePortOpen)
	assert.Equal(t, 16093, st.OdometerMeters)
	assert.Equal(t, 161, st.RangeKm)
	assert.Equal(t, 78.0, st.BatteryLevelPct)
	assert.Equal(t, 10.0, st.SpeedMps)
	require.NotNil(t, st.Location)
	assert.Equal(t, 37.5, st.Location.Latitude)
	assert.Equal(t, "2026.6.1", st.OtaVersion)

	require.NotNil(t, snap.Live)
	assert.Equal(t, 175.0, snap.Live.PowerKw)
	assert.Equal(t, 23.4, snap.Live.TotalChargedEnergy)
	assert.Equal(t, 1800, snap.Live.TimeRemainingSec)
}

// TestPowerToCanonical reproduces the old translate.topState truth table.
func TestPowerToCanonical(t *testing.T) {
	t.Parallel()
	cases := []struct {
		power string
		cloud bool
		want  vehicle.Power
	}{
		{"sleep", true, vehicle.PowerSleep},
		{"go", true, vehicle.PowerOnline},
		{"ready", true, vehicle.PowerOnline},
		{"standby", true, vehicle.PowerOffline},
		{"go", false, vehicle.PowerOffline},
		{"weird", true, vehicle.PowerOffline},
		{"", true, vehicle.PowerOffline},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, powerToCanonical(c.power, c.cloud), "power=%q cloud=%v", c.power, c.cloud)
	}
}

// TestPluggedInGatesLiveCall: pluggedIn drives whether Poll fetches the charging
// endpoint. Verify the predicate (the live-call gating logic moved here from the
// poller).
func TestPluggedIn(t *testing.T) {
	t.Parallel()
	assert.False(t, pluggedIn(&VehicleState{ChargerStatus: "chrgr_sts_not_connected"}))
	assert.True(t, pluggedIn(&VehicleState{ChargerState: "charging_active"}))
	assert.True(t, pluggedIn(&VehicleState{ChargerState: "charging_ready"}))
	assert.True(t, pluggedIn(&VehicleState{ChargerStatus: "chrgr_sts_connected_idle"}))
	assert.False(t, pluggedIn(&VehicleState{}))
}
