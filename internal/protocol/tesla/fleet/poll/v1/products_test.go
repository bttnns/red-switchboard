package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVehiclesSkipsEnergySiteStringID is a regression test for the /products
// decode: Tesla returns a vehicle's `id` as a JSON number but an energy site's
// `id` as a JSON string. A strict []wire.Product (int64 id) decode failed the
// ENTIRE list ("cannot unmarshal string into ... id of type int64"), so an
// account with a Powerwall/solar site could not list its car. Vehicles must
// decode leniently and skip non-vehicle products (no vehicle_id).
func TestVehiclesSkipsEnergySiteStringID(t *testing.T) {
	const products = `{"response":[
		{"id":"313dbc37-de2b-4d0e-9c0a-1a2b3c4d5e6f","energy_site_id":12345,"resource_type":"solar","site_name":"My Home"},
		{"id":100021234567890123,"vehicle_id":200021234567890123,"vin":"5YJSA1E26MF000001","display_name":"Model S","state":"online"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/1/products" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(products))
	}))
	defer srv.Close()

	src := newTestFleetSource(t, srv.URL)
	ids, err := src.Vehicles(context.Background())
	if err != nil {
		t.Fatalf("Vehicles: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 vehicle (energy site skipped), got %d: %+v", len(ids), ids)
	}
	if ids[0].VIN != "5YJSA1E26MF000001" || ids[0].ID != ids[0].VIN {
		t.Fatalf("unexpected identity: %+v", ids[0])
	}
}
