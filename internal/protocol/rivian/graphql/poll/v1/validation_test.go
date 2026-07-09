package v1

import (
	"encoding/json"
	"os"
	"testing"
)

// Shape validation against a REAL captured Rivian GraphQL response.
//
// The fixtures testdata/unofficial_vehicle_state.json and
// testdata/unofficial_live_session.json are the VEHICLE_STATE_RESPONSE and
// LIVE_CHARGING_SESSION_RESPONSE samples from the community rivian-python-client
// (github.com/bretterer/rivian-python-client, MIT licensed). They are committed
// here as golden samples so this validation runs in CI without a network or that
// repo.
// If Rivian (or our parser) drifts, this test fails: the community's real
// response no longer maps cleanly into our wire structs and canonical model.
func TestValidateShapeAgainstUnofficialRivian(t *testing.T) {
	vsBytes, err := os.ReadFile("testdata/unofficial_vehicle_state.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw rawVehicleStateResponse
	if err := json.Unmarshal(vsBytes, &raw); err != nil {
		t.Fatalf("unmarshal vehicleState into our wire struct: %v", err)
	}
	vs := raw.flatten()
	if vs == nil {
		t.Fatal("flattened VehicleState is nil (shape drift)")
	}

	liveBytes, err := os.ReadFile("testdata/unofficial_live_session.json")
	if err != nil {
		t.Fatal(err)
	}
	var lraw rawLiveSessionResponse
	if err := json.Unmarshal(liveBytes, &lraw); err != nil {
		t.Fatalf("unmarshal getLiveSessionData into our wire struct: %v", err)
	}
	var live *LiveSessionData
	if lraw.Data.Session != nil {
		live = lraw.Data.Session.flatten()
	}

	snap := toCanonical(vs, live)
	if snap.State == nil {
		t.Fatal("canonical State is nil after mapping the real sample")
	}
	if b := snap.State.BatteryLevelPct; b < 0 || b > 100 {
		t.Errorf("battery level out of range: %v", b)
	}
	if snap.State.Location == nil {
		t.Error("expected a location: the sample carries gnssLocation")
	}
	// The sample is an active charging session, so a live session must map.
	if live == nil {
		t.Error("expected a live charging session from the sample")
	} else if live.TotalChargedEnergy < 0 {
		t.Errorf("negative charged energy: %v", live.TotalChargedEnergy)
	}
}
