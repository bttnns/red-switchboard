package v1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testCtx() context.Context { return context.Background() }

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

func TestVehicleStateFlatten(t *testing.T) {
	var raw rawVehicleStateResponse
	if err := json.Unmarshal(readFixture(t, "vehicle_state.json"), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	vs := raw.flatten()

	if !vs.CloudConnectionOnline {
		t.Error("expected CloudConnectionOnline true")
	}
	if vs.Location == nil {
		t.Fatal("expected non-nil Location")
	}
	if vs.Location.Latitude != 37.3349 || vs.Location.Longitude != -122.009 {
		t.Errorf("Location = %+v", vs.Location)
	}
	if vs.GnssSpeed != 13.4 {
		t.Errorf("GnssSpeed = %v, want 13.4", vs.GnssSpeed)
	}
	if vs.VehicleMileage != 24140160 {
		t.Errorf("VehicleMileage = %v (meters), want 24140160", vs.VehicleMileage)
	}
	if vs.DistanceToEmpty != 412 {
		t.Errorf("DistanceToEmpty = %v (km), want 412", vs.DistanceToEmpty)
	}
	if vs.BatteryLevel != 78.5 {
		t.Errorf("BatteryLevel = %v, want 78.5", vs.BatteryLevel)
	}
	if vs.BatteryLimit != 80 {
		t.Errorf("BatteryLimit = %v, want 80", vs.BatteryLimit)
	}
	if vs.BatteryCapacity != 128.9 {
		t.Errorf("BatteryCapacity = %v, want 128.9", vs.BatteryCapacity)
	}
	if vs.PowerState != "go" {
		t.Errorf("PowerState = %q, want go", vs.PowerState)
	}
	if vs.GearStatus != "drive" {
		t.Errorf("GearStatus = %q, want drive", vs.GearStatus)
	}
	if vs.ChargerStatus != "chrgr_sts_not_connected" {
		t.Errorf("ChargerStatus = %q", vs.ChargerStatus)
	}
	if vs.CabinInteriorTemp != 21.5 {
		t.Errorf("CabinInteriorTemp = %v, want 21.5", vs.CabinInteriorTemp)
	}
	if vs.DoorRearRightClosed != "open" {
		t.Errorf("DoorRearRightClosed = %q, want open", vs.DoorRearRightClosed)
	}
	if vs.OtaCurrentVersion != "2026.10.0" {
		t.Errorf("OtaCurrentVersion = %q", vs.OtaCurrentVersion)
	}
	if vs.OtaStatus != "Ready_To_Install" {
		t.Errorf("OtaStatus = %q", vs.OtaStatus)
	}

	// INVALID_SENSOR_STATES sentinels must be dropped (left empty), not surfaced.
	if vs.WindowFrontRightClosed != "" {
		t.Errorf("WindowFrontRightClosed should be empty (signal_not_available), got %q", vs.WindowFrontRightClosed)
	}
	if vs.ClosureTonneauClosed != "closed" {
		t.Errorf("ClosureTonneauClosed = %q, want closed", vs.ClosureTonneauClosed)
	}

	// LastUpdate should be the latest scalar timestamp seen.
	if vs.LastUpdate.IsZero() {
		t.Error("expected non-zero LastUpdate")
	}
}

func TestVehicleStateFlattenNil(t *testing.T) {
	// A null vehicleState must yield a zero-value struct, not a panic.
	var raw rawVehicleStateResponse
	if err := json.Unmarshal([]byte(`{"data":{"vehicleState":null}}`), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	vs := raw.flatten()
	if vs == nil {
		t.Fatal("flatten returned nil")
	}
	if vs.PowerState != "" || vs.BatteryLevel != 0 || vs.Location != nil {
		t.Errorf("expected zero-value struct, got %+v", vs)
	}
}

func TestLiveSessionActiveFlatten(t *testing.T) {
	var raw rawLiveSessionResponse
	if err := json.Unmarshal(readFixture(t, "live_session_active.json"), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.Data.Session == nil {
		t.Fatal("expected active session")
	}
	ls := raw.Data.Session.flatten()

	if ls.Power != 11.2 {
		t.Errorf("Power = %v, want 11.2", ls.Power)
	}
	if ls.Current != 48.0 {
		t.Errorf("Current = %v, want 48", ls.Current)
	}
	if ls.Soc != 78.5 {
		t.Errorf("Soc = %v, want 78.5", ls.Soc)
	}
	// timeRemaining arrives as a String and must parse to int seconds.
	if ls.TimeRemaining != 5400 {
		t.Errorf("TimeRemaining = %v, want 5400", ls.TimeRemaining)
	}
	if ls.TotalChargedEnergy != 17.3 {
		t.Errorf("TotalChargedEnergy = %v, want 17.3", ls.TotalChargedEnergy)
	}
	if ls.VehicleChargerState != "charging_active" {
		t.Errorf("VehicleChargerState = %q", ls.VehicleChargerState)
	}
	if ls.CurrentPrice != 6.42 || ls.CurrentCurrency != "USD" {
		t.Errorf("price = %v %s", ls.CurrentPrice, ls.CurrentCurrency)
	}
	if ls.TimeElapsed != 3600 {
		t.Errorf("TimeElapsed = %v, want 3600", ls.TimeElapsed)
	}
}

func TestLiveSessionNoneIsNil(t *testing.T) {
	// A null getLiveSessionData object signals "no active session".
	var raw rawLiveSessionResponse
	if err := json.Unmarshal(readFixture(t, "live_session_none.json"), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.Data.Session != nil {
		t.Errorf("expected nil session for null object, got %+v", raw.Data.Session)
	}
}

func TestGraphQLErrorParsing(t *testing.T) {
	var env struct {
		Errors []GraphQLError `json:"errors"`
	}
	if err := json.Unmarshal(readFixture(t, "error_unauthenticated.json"), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(env.Errors))
	}
	apiErr := &APIError{
		Code:       env.Errors[0].Extensions.Code,
		Reason:     env.Errors[0].Extensions.Reason,
		HTTPStatus: 200,
		Errors:     env.Errors,
	}
	if !IsUnauthenticated(apiErr) {
		t.Error("expected IsUnauthenticated true for UNAUTHENTICATED code")
	}
	if IsRateLimited(apiErr) {
		t.Error("expected IsRateLimited false")
	}
	// HTTP 401 with no GraphQL errors should also be unauthenticated.
	if !IsUnauthenticated(&APIError{HTTPStatus: 401}) {
		t.Error("expected HTTP 401 to be treated as unauthenticated")
	}
	// RATE_LIMIT branch.
	if !IsRateLimited(&APIError{Code: ErrCodeRateLimit}) {
		t.Error("expected IsRateLimited true for RATE_LIMIT code")
	}
}

func TestCredsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")

	in := &AuthData{
		Token:            "acc",
		RefreshToken:     "ref",
		UserSessionToken: "usess",
		CSRFToken:        "csrf",
		AppSessionToken:  "asess",
		VehicleID:        "guid-123",
		VIN:              "7FCTGAAA0NN000000",
		VehicleName:      "Stormy",
		Username:         "owner@example.com",
	}
	if err := SaveCreds(path, in); err != nil {
		t.Fatalf("SaveCreds: %v", err)
	}
	// File must be base64 (not plaintext JSON).
	raw, _ := os.ReadFile(path)
	if json.Valid(raw) {
		t.Error("creds file should be base64-encoded, not plaintext JSON")
	}

	out, err := LoadCreds(path)
	if err != nil {
		t.Fatalf("LoadCreds: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("roundtrip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestLoadCredsMissingVehicleID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := SaveCreds(path, &AuthData{Token: "x"}); err != nil {
		t.Fatalf("SaveCreds: %v", err)
	}
	if _, err := LoadCreds(path); err == nil {
		t.Error("expected error for creds without vehicle ID")
	}
}

func TestReauthOnUnauthenticated(t *testing.T) {
	// callWithRefresh must refresh the session exactly once on UNAUTHENTICATED then retry.
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := SaveCreds(path, &AuthData{VehicleID: "guid", Username: "u@e.com", UserSessionToken: "old"}); err != nil {
		t.Fatalf("SaveCreds: %v", err)
	}
	c, err := New(path, "", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	refreshes := 0
	c.session = &sessionRefresher{refresh: func(username, credsPath string) error {
		refreshes++
		// Simulate a refreshed session being persisted.
		return SaveCreds(credsPath, &AuthData{VehicleID: "guid", Username: username, UserSessionToken: "new"})
	}}

	calls := 0
	err = c.callWithRefresh(testCtx(), func(_ context.Context, a *AuthData) error {
		calls++
		if calls == 1 {
			if a.UserSessionToken != "old" {
				t.Errorf("first call session = %q, want old", a.UserSessionToken)
			}
			return &APIError{Code: ErrCodeUnauthenticated, HTTPStatus: 200}
		}
		if a.UserSessionToken != "new" {
			t.Errorf("retry session = %q, want new (post-refresh)", a.UserSessionToken)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("callWithRefresh: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + retry)", calls)
	}
}

func TestNoRefreshOnOtherError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := SaveCreds(path, &AuthData{VehicleID: "guid", Username: "u@e.com"}); err != nil {
		t.Fatalf("SaveCreds: %v", err)
	}
	c, err := New(path, "", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refreshes := 0
	c.session = &sessionRefresher{refresh: func(string, string) error { refreshes++; return nil }}

	rateErr := &APIError{Code: ErrCodeRateLimit, HTTPStatus: 200}
	calls := 0
	err = c.callWithRefresh(testCtx(), func(_ context.Context, _ *AuthData) error { calls++; return rateErr })
	if err != rateErr {
		t.Errorf("err = %v, want rate-limit error returned as-is", err)
	}
	if refreshes != 0 {
		t.Errorf("refreshes = %d, want 0 (no refresh on non-UNAUTHENTICATED)", refreshes)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", calls)
	}
}

func TestAccessors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := SaveCreds(path, &AuthData{VehicleID: "g1", VIN: "vin1", VehicleName: "n1"}); err != nil {
		t.Fatalf("SaveCreds: %v", err)
	}
	c, err := New(path, "", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.VehicleGUID() != "g1" {
		t.Errorf("VehicleGUID = %q, want g1", c.VehicleGUID())
	}
	vs := c.Vehicles()
	if len(vs) != 1 || vs[0].GUID != "g1" || vs[0].VIN != "vin1" || vs[0].Name != "n1" {
		t.Errorf("Vehicles() = %+v, want one {g1,vin1,n1}", vs)
	}
}

func TestQueryBuildersAreValidJSON(t *testing.T) {
	// The request bodies must be valid JSON with the gateway/charging
	// operations and the vehicle id threaded into variables.
	vs := vehicleStateQuery("guid-123")
	if !json.Valid([]byte(vs)) {
		t.Fatal("vehicleStateQuery produced invalid JSON")
	}
	var vsBody struct {
		OperationName string            `json:"operationName"`
		Query         string            `json:"query"`
		Variables     map[string]string `json:"variables"`
	}
	if err := json.Unmarshal([]byte(vs), &vsBody); err != nil {
		t.Fatalf("vehicleStateQuery unmarshal: %v", err)
	}
	if vsBody.OperationName != "GetVehicleState" {
		t.Errorf("op = %q", vsBody.OperationName)
	}
	if vsBody.Variables["vehicleID"] != "guid-123" {
		t.Errorf("vehicleID var = %q", vsBody.Variables["vehicleID"])
	}

	ls := liveSessionQuery("guid-123")
	if !json.Valid([]byte(ls)) {
		t.Fatal("liveSessionQuery produced invalid JSON")
	}
	var lsBody struct {
		OperationName string            `json:"operationName"`
		Variables     map[string]string `json:"variables"`
	}
	if err := json.Unmarshal([]byte(ls), &lsBody); err != nil {
		t.Fatalf("liveSessionQuery unmarshal: %v", err)
	}
	if lsBody.OperationName != "getLiveSessionData" {
		t.Errorf("op = %q", lsBody.OperationName)
	}
	if lsBody.Variables["vehicleId"] != "guid-123" {
		t.Errorf("vehicleId var = %q", lsBody.Variables["vehicleId"])
	}
}
