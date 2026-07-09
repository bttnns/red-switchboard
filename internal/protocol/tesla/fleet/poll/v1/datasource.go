package v1

import (
	"fmt"

	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
)

// VehicleDataSource supplies the Tesla Fleet API payloads the server serves. The
// canonical-backed sourceAdapter implements it for production; a static fixture
// source satisfies the same seam to test the HTTP surface in isolation.
type VehicleDataSource interface {
	// Products returns the discovery list for GET /api/1/products.
	Products() ([]wire.Product, error)
	// Summary returns the lightweight vehicle object for GET /api/1/vehicles/{id}.
	Summary(id int64) (wire.Summary, error)
	// VehicleData returns the full payload for GET /api/1/vehicles/{id}/vehicle_data.
	VehicleData(id int64) (wire.VehicleData, error)
}

// ErrVehicleNotFound is returned by a source when the requested id is unknown.
var ErrVehicleNotFound = fmt.Errorf("vehicle not found")

// staticSource is a single online vehicle backed by the static wire fixture,
// used to test the HTTP surface in isolation.
type staticSource struct {
	id          int64
	vehicleID   int64
	vin         string
	displayName string
}

// NewStaticSource builds a VehicleDataSource for one fixed online vehicle.
func NewStaticSource(id, vehicleID int64, vin, displayName string) VehicleDataSource {
	return &staticSource{
		id:          id,
		vehicleID:   vehicleID,
		vin:         vin,
		displayName: displayName,
	}
}

func (s *staticSource) Products() ([]wire.Product, error) {
	return []wire.Product{{
		ID:          s.id,
		VehicleID:   s.vehicleID,
		VIN:         s.vin,
		DisplayName: s.displayName,
		State:       "online",
	}}, nil
}

func (s *staticSource) Summary(id int64) (wire.Summary, error) {
	if id != s.id {
		return wire.Summary{}, ErrVehicleNotFound
	}
	return wire.Summary{
		ID:          s.id,
		VehicleID:   s.vehicleID,
		VIN:         s.vin,
		DisplayName: s.displayName,
		State:       "online",
	}, nil
}

func (s *staticSource) VehicleData(id int64) (wire.VehicleData, error) {
	if id != s.id {
		return wire.VehicleData{}, ErrVehicleNotFound
	}
	return wire.NewOnlineVehicleData(s.id, s.vehicleID, s.vin, s.displayName), nil
}
