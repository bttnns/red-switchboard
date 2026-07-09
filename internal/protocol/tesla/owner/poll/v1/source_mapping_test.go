package v1

import (
	"math"
	"testing"
	"time"

	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// TestOwnerRoundTripThroughSharedSink proves the Owner source decode (which
// delegates to the shared tesla-fleet-poll-v1 mapping) is the inverse of the shared
// canonical -> vehicle_data encode. The Fleet sink encoder is used as the mock:
// canonical -> wire -> canonical must survive for the fields both directions
// support. This is the mock-based test for tesla-owner-poll-v1.
func TestOwnerRoundTripThroughSharedSink(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	in := &vehicle.State{
		Power:           vehicle.PowerOnline,
		UserPresent:     true,
		Gear:            vehicle.GearDrive,
		Charger:         vehicle.ChargerCharging,
		Plug:            vehicle.PlugConnected,
		BatteryLevelPct: 72,
		OdometerMeters:  16093, // ~10 mi
		RangeKm:         161,   // ~100 mi
		SpeedMps:        10,
		Location:        &vehicle.Location{Latitude: 37.7, Longitude: -122.4, TimeStamp: fixed},
		LastUpdate:      fixed,
	}
	live := &vehicle.LiveSession{PowerKw: 7, CurrentA: 30, TotalChargedEnergy: 5.5}

	wired := teslafleet.VehicleData(nil, in, live,
		teslafleet.IDs{ID: 1, VehicleID: 2, VIN: "5YJ0000000000001", DisplayName: "Owner Test"},
		teslafleet.Cfg{CarType: "model3", Model: "S"}, time.Time{}, nil)

	snap := decodeVehicleData(&wired)
	if snap.State == nil {
		t.Fatal("decoded snapshot has nil State")
	}
	got := snap.State

	if got.Power != vehicle.PowerOnline {
		t.Errorf("power = %v, want online", got.Power)
	}
	if got.Gear != vehicle.GearDrive {
		t.Errorf("gear = %v, want drive", got.Gear)
	}
	if got.Charger != vehicle.ChargerCharging {
		t.Errorf("charger = %v, want charging", got.Charger)
	}
	if got.Plug != vehicle.PlugConnected {
		t.Errorf("plug = %v, want connected", got.Plug)
	}
	if math.Abs(got.BatteryLevelPct-72) > 0.5 {
		t.Errorf("battery = %v, want ~72", got.BatteryLevelPct)
	}
	if math.Abs(float64(got.OdometerMeters)-units.MilesToMeters(units.MetersToMiles(16093))) > 2 {
		t.Errorf("odometer = %v, want ~16093 m", got.OdometerMeters)
	}
	if got.Location == nil || math.Abs(got.Location.Latitude-37.7) > 0.001 {
		t.Errorf("location lat = %v, want 37.7", got.Location)
	}
	if snap.Live == nil || math.Abs(snap.Live.TotalChargedEnergy-5.5) > 0.01 {
		t.Errorf("live energy = %v, want 5.5", snap.Live)
	}
}
