package v1

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// Shape validation against a REAL captured Tesla Owner API response.
//
// testdata/owner_vehicle_data.json is the vehicle-vehicle_data cassette from
// timdorr/tesla-api (github.com/timdorr/tesla-api, MIT, the canonical reference
// for the owner-api). It is a genuine owner-api GET /api/1/vehicles/{id}/
// vehicle_data body, committed as a golden sample so this runs in CI offline. If
// it no longer maps into our (Fleet-shared) wire structs and canonical model,
// our Owner shape has drifted.
func TestValidateShapeAgainstOwnerAPI(t *testing.T) {
	raw, err := os.ReadFile("testdata/owner_vehicle_data.json")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Response wire.VehicleData `json:"response"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal owner vehicle_data into our wire struct: %v", err)
	}

	snap := decodeVehicleData(&env.Response)
	if snap.State == nil {
		t.Fatal("canonical State is nil after mapping the real owner sample")
	}
	if b := snap.State.BatteryLevelPct; b <= 0 || b > 100 {
		t.Errorf("battery level out of range: %v", b)
	}
	if snap.State.Power == vehicle.PowerUnknown {
		t.Error("power should resolve from the sample state")
	}
	if snap.State.OdometerMeters < 0 {
		t.Errorf("negative odometer: %v", snap.State.OdometerMeters)
	}
}
