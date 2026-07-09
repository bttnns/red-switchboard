package v1

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"net/http/httptest"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// TestOwnerSourceSinkRoundTrip stands up the tesla-owner-poll-v1 SINK (which reuses the
// tesla-fleet-poll-v1 server) over a two-car mock engine, then points the
// tesla-owner-poll-v1 SOURCE at it and verifies the source lists both cars via the
// Owner API /api/1/vehicles endpoint and decodes their scenarios. This is the
// Owner-API stop-gap path in-process and in CI.
func TestOwnerSourceSinkRoundTrip(t *testing.T) {
	eng := mock.NewEngine([]mock.VehicleSpec{
		{GUID: "T1", VIN: "5YJ3E1EA1KF000001", Name: "Model 3", Model: "model3", Make: "Tesla"},
		{GUID: "T2", VIN: "5YJYGDEE1LF000002", Name: "Model Y", Model: "modely", Make: "Tesla"},
	}, 1, 0, nil)
	// The Owner sink is the Fleet server (shared surface); it must serve the Owner
	// list endpoint GET /api/1/vehicles, which the owner source calls.
	h, _, _, err := teslafleet.BuildHandlerSampler(eng, teslafleet.Settings{}, nil)
	if err != nil {
		t.Fatalf("BuildHandlerSampler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	creds := filepath.Join(t.TempDir(), "tesla-owner.json")
	if err := os.WriteFile(creds, []byte(`{"access_token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("base_url: \""+srv.URL+"\"\ncreds_file: \""+creds+"\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	s, err := newSource(doc.Content[0], nil)
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	ctx := context.Background()

	ids, err := s.Vehicles(ctx)
	if err != nil {
		t.Fatalf("Vehicles (Owner /api/1/vehicles list): %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 vehicles, got %d", len(ids))
	}

	eng.SetScenario(mock.ScenarioCharging, "T1")
	snap, err := s.Poll(ctx, ids[0].ID)
	if err != nil {
		t.Fatalf("Poll(%s): %v", ids[0].ID, err)
	}
	if snap.State == nil || snap.State.Charger != vehicle.ChargerCharging {
		t.Fatalf("car1 expected charger Charging, got %+v", snap.State)
	}
}
