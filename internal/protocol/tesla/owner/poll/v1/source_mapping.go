package v1

// THE SOURCE MAP for tesla-owner-poll-v1 (vendor wire -> canonical).
//
// The Tesla Owner API (owner-api.teslamotors.com) is the predecessor of the
// Tesla Fleet API and returns the same vehicle_data shape for every field
// redswitchboard reads, so this map delegates to tesla-fleet-poll-v1's
// DecodeVehicleData (imperial -> canonical SI). The authoritative, documented
// field-correspondence table (vehicle_data field <-> canonical vehicle.State)
// lives in internal/protocol/tesla/fleet/poll/v1/source_mapping.go; it is shared
// rather than duplicated here on purpose. The Owner-specific differences are NOT
// in the field mapping but in the transport (see source.go): the host, the
// GET /api/1/vehicles list endpoint, and addressing a car by its numeric id.

import (
	teslafleet "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// decodeVehicleData maps an Owner API vehicle_data payload into the canonical
// snapshot by reusing the shared Tesla mapping.
func decodeVehicleData(vd *wire.VehicleData) vehicle.Snapshot {
	return teslafleet.DecodeVehicleData(vd)
}
