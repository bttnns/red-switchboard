package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/plugin/sink/idmap"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// newTestFleetSource points a tesla-fleet-poll-v1 source at baseURL via a temp creds
// file, exercising the real factory (settings decode + creds load).
func newTestFleetSource(t *testing.T, baseURL string) *FleetSource {
	t.Helper()
	creds := filepath.Join(t.TempDir(), "tesla.json")
	if err := os.WriteFile(creds, []byte(`{"access_token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("base_url: \""+baseURL+"\"\ncreds_file: \""+creds+"\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	s, err := newSource(doc.Content[0], nil)
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	return s.(*FleetSource)
}

// TestFleetSourceSinkRoundTrip stands up the tesla-fleet-poll-v1 SINK over a two-car
// mock engine, then points the tesla-fleet-poll-v1 SOURCE at it and verifies the
// source lists both cars and decodes their scenarios back from the wire. This is
// the same-protocol round trip the e2e exercises, but in-process and in CI. It
// covers VIN-addressed vehicle_data (the source addresses by VIN; the sink must
// resolve it).
func TestFleetSourceSinkRoundTrip(t *testing.T) {
	eng := mock.NewEngine([]mock.VehicleSpec{
		{GUID: "T1", VIN: "5YJ3E1EA1KF000001", Name: "Model 3", Model: "model3", Make: "Tesla"},
		{GUID: "T2", VIN: "5YJYGDEE1LF000002", Name: "Model Y", Model: "modely", Make: "Tesla"},
	}, 1, 0, nil)
	h, _, _, err := BuildHandlerSampler(eng, Settings{ProviderToken: "local"}, nil)
	if err != nil {
		t.Fatalf("BuildHandlerSampler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	src := newTestFleetSource(t, srv.URL)
	ctx := context.Background()

	ids, err := src.Vehicles(ctx)
	if err != nil {
		t.Fatalf("Vehicles: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 vehicles, got %d", len(ids))
	}
	// The Fleet source addresses cars by VIN.
	if ids[0].ID != ids[0].VIN {
		t.Fatalf("fleet source id should be the VIN, got id=%q vin=%q", ids[0].ID, ids[0].VIN)
	}

	// Put car 1 driving, car 2 charging, then poll each (by VIN) and confirm the
	// scenario survives the canonical -> wire -> canonical round trip.
	eng.SetScenario(mock.ScenarioDriving, "T1")
	eng.SetScenario(mock.ScenarioCharging, "T2")

	snap1, err := src.Poll(ctx, ids[0].ID)
	if err != nil {
		t.Fatalf("Poll(%s): %v", ids[0].ID, err)
	}
	if snap1.State == nil || snap1.State.Gear != vehicle.GearDrive {
		t.Fatalf("car1 expected gear Drive, got %+v", snap1.State)
	}

	snap2, err := src.Poll(ctx, ids[1].ID)
	if err != nil {
		t.Fatalf("Poll(%s): %v", ids[1].ID, err)
	}
	if snap2.State == nil || snap2.State.Charger != vehicle.ChargerCharging {
		t.Fatalf("car2 expected charger Charging, got %+v", snap2.State)
	}
}

// TestResolveTagVINorID confirms the sink adapter resolves a Fleet vehicle_tag
// that is either the synthetic integer id or the VIN (like the real Fleet API).
func TestResolveTagVINorID(t *testing.T) {
	eng := mock.NewEngine([]mock.VehicleSpec{
		{GUID: "T1", VIN: "5YJ3E1EA1KF000001", Name: "Model 3", Model: "model3", Make: "Tesla"},
	}, 1, 0, nil)
	ids, err := idmap.New("")
	if err != nil {
		t.Fatalf("idmap.New: %v", err)
	}
	a, err := newSourceAdapter(eng, ids, 0, nil, nil)
	if err != nil {
		t.Fatalf("newSourceAdapter: %v", err)
	}
	// By VIN.
	id, ok := a.ResolveTag("5YJ3E1EA1KF000001")
	if !ok {
		t.Fatal("ResolveTag by VIN failed")
	}
	// The same id resolves by its integer form.
	if got, ok := a.ResolveTag(strconv.FormatInt(id, 10)); !ok || got != id {
		t.Fatalf("ResolveTag by id: got %d ok=%v, want %d", got, ok, id)
	}
	// Unknown tag.
	if _, ok := a.ResolveTag("NOPE"); ok {
		t.Fatal("ResolveTag should fail for an unknown tag")
	}
}

// TestVehiclesListRoute confirms GET /api/1/vehicles returns the vehicle list
// (the Owner API list endpoint the tesla-owner-poll-v1 source/sink relies on).
func TestVehiclesListRoute(t *testing.T) {
	eng := mock.NewEngine([]mock.VehicleSpec{
		{GUID: "T1", VIN: "5YJ3E1EA1KF000001", Name: "Model 3", Model: "model3", Make: "Tesla"},
		{GUID: "T2", VIN: "5YJYGDEE1LF000002", Name: "Model Y", Model: "modely", Make: "Tesla"},
	}, 1, 0, nil)
	h, _, _, err := BuildHandlerSampler(eng, Settings{}, nil)
	if err != nil {
		t.Fatalf("BuildHandlerSampler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/1/vehicles")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/1/vehicles: status %d", resp.StatusCode)
	}
}
