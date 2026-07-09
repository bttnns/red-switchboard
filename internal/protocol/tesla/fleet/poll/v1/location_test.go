package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPollRequestsLocationData is a regression test: the Fleet API only returns
// drive_state.latitude/longitude when the vehicle_data request explicitly asks
// for the location_data endpoint (the vehicle_location scope grants permission,
// the endpoints param requests the data). Without it GPS is always null, which
// also crashed downstream consumers (TeslaMate's terrain lookup) on nil coords.
func TestPollRequestsLocationData(t *testing.T) {
	const vin = "TESTVIN0000000001"
	var dataEndpoints string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/vehicle_data") {
			dataEndpoints = r.URL.Query().Get("endpoints")
			_, _ = w.Write([]byte(`{"response":{"drive_state":{"latitude":29.69,"longitude":-95.39,"shift_state":"P"}}}`))
			return
		}
		// summary state-check: report online so Poll proceeds to vehicle_data.
		_, _ = w.Write([]byte(`{"response":{"state":"online","vin":"` + vin + `"}}`))
	}))
	defer srv.Close()

	src := newTestFleetSource(t, srv.URL)
	snap, err := src.Poll(context.Background(), vin)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !strings.Contains(dataEndpoints, "location_data") {
		t.Fatalf("vehicle_data request must include the location_data endpoint, got endpoints=%q", dataEndpoints)
	}
	if snap.State == nil || snap.State.Location == nil {
		t.Fatalf("expected decoded location from the response, got state=%+v", snap.State)
	}
}
