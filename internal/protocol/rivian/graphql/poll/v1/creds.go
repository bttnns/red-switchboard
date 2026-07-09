package v1

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

// Vehicle is one vehicle on the Rivian account, captured from getUserInfo. GUID
// is the id accepted by vehicleState(id:) and getLiveSessionData(vehicleId:).
type Vehicle struct {
	GUID string `json:"id"`
	VIN  string `json:"vin,omitempty"`
	Name string `json:"name,omitempty"`
}

// AuthData represents the authentication data stored in the auth file. The
// session tokens are ACCOUNT-level (one login serves every vehicle on the
// account); Vehicles holds the full account vehicle list. VehicleID/VIN/
// VehicleName are retained for backward compatibility with single-vehicle creds
// files and always mirror the first vehicle.
type AuthData struct {
	Token            string `json:"token"`
	RefreshToken     string `json:"refresh_token"`
	UserSessionToken string `json:"user_session_token"`
	CSRFToken        string `json:"csrf_token"`
	AppSessionToken  string `json:"app_session_token"`
	VehicleID        string `json:"vehicle_id"`
	VIN              string `json:"vin,omitempty"`
	VehicleName      string `json:"vehicle_name,omitempty"`
	// Vehicles is the full account vehicle list. Newer creds files populate it;
	// older single-vehicle files leave it empty and LoadCreds synthesizes a
	// one-element list from VehicleID/VIN/VehicleName.
	Vehicles []Vehicle `json:"vehicles,omitempty"`
	// Username is the Rivian account email, retained so re-auth (which has no
	// refresh mutation and must re-run CSRF -> login) can be attempted.
	Username string `json:"username,omitempty"`
}

// VehicleList returns the account vehicles, synthesizing a one-element list from
// the legacy single-vehicle fields when Vehicles is empty.
func (a *AuthData) VehicleList() []Vehicle {
	if len(a.Vehicles) > 0 {
		return a.Vehicles
	}
	if a.VehicleID != "" {
		return []Vehicle{{GUID: a.VehicleID, VIN: a.VIN, Name: a.VehicleName}}
	}
	return nil
}

// LoadCreds reads and decodes AuthData from a creds file (base64 JSON), the
// format minted out of band by a separate login tool (github.com/bttnns/rivian_auth).
func LoadCreds(path string) (*AuthData, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading creds file: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("error decoding creds file: %w", err)
	}
	var data AuthData
	if err := json.Unmarshal(decoded, &data); err != nil {
		return nil, fmt.Errorf("error parsing creds file: %w", err)
	}
	if len(data.VehicleList()) == 0 {
		return nil, fmt.Errorf("no vehicles found in creds file")
	}
	return &data, nil
}

// SaveCreds encodes and writes AuthData back to a creds file (0600).
func SaveCreds(path string, data *AuthData) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("error marshaling creds: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
		return fmt.Errorf("error writing creds file: %w", err)
	}
	return nil
}
