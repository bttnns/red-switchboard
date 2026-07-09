package v1

import (
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink/idmap"
	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1/wire"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubProvider is a static sink.Provider: it reports a fixed vehicle list and an
// empty snapshot per vehicle (the translator then serves its valid asleep
// fallback, which is all these resolution tests need).
type stubProvider struct {
	vehicles []vehicle.Identity
}

func (p stubProvider) Vehicles() []vehicle.Identity   { return p.vehicles }
func (p stubProvider) Latest(string) vehicle.Snapshot { return vehicle.Snapshot{} }
func (p stubProvider) Stats(string) poll.Stats        { return poll.Stats{} }

// TestMultiVehicleResolution verifies the sink serves N vehicles: Products
// returns one entry per car with distinct ids, Summary/VehicleData resolve by id,
// per-vehicle config (trim_badging) is honored, and an unknown id is rejected.
func TestMultiVehicleResolution(t *testing.T) {
	ids, err := idmap.New("")
	require.NoError(t, err)

	prov := stubProvider{vehicles: []vehicle.Identity{
		{ID: "guid-a", VIN: "VIN_A", DisplayName: "Truck", Model: "R1T"},
		{ID: "guid-b", VIN: "VIN_B", DisplayName: "SUV", Model: "R1S"},
	}}
	s, err := newSourceAdapter(prov, ids, 30*time.Minute, func(string) string { return "model3" }, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, s.VehicleCount())

	products, err := s.Products()
	require.NoError(t, err)
	require.Len(t, products, 2)
	assert.NotEqual(t, products[0].ID, products[1].ID, "vehicles must have distinct ids")
	assert.NotEqual(t, products[0].VehicleID, products[0].ID, "eid != vid")

	byName := map[string]wire.Product{products[0].DisplayName: products[0], products[1].DisplayName: products[1]}
	truck, suv := byName["Truck"], byName["SUV"]
	require.NotZero(t, truck.ID)
	require.NotZero(t, suv.ID)

	td, err := s.VehicleData(truck.ID)
	require.NoError(t, err)
	require.NotNil(t, td.VehicleConfig)
	assert.Equal(t, "R1T", *td.VehicleConfig.TrimBadging)

	sd, err := s.VehicleData(suv.ID)
	require.NoError(t, err)
	require.NotNil(t, sd.VehicleConfig)
	assert.Equal(t, "R1S", *sd.VehicleConfig.TrimBadging)

	// Unknown id is rejected.
	_, err = s.VehicleData(999999)
	assert.ErrorIs(t, err, ErrVehicleNotFound)
	_, err = s.Summary(999999)
	assert.ErrorIs(t, err, ErrVehicleNotFound)

	// Status reports both vehicles.
	assert.Len(t, s.Status(), 2)
}

// TestDegradeIfStale is the phantom-drive guard: once the cache is older than
// staleAfter, the top-level state degrades so TeslaMate stops believing stale
// motion, while the sub-objects stay intact. A car last seen driving degrades to
// "offline" (drive-timeout fires); a settled car degrades to "asleep".
func TestDegradeIfStale(t *testing.T) {
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s := &sourceAdapter{
		staleAfter: 30 * time.Minute,
		now:        func() time.Time { return base },
	}

	driving := wire.VehicleData{State: "online", DriveState: &wire.DriveState{ShiftState: ptrStr("D")}}
	parked := wire.VehicleData{State: "online", DriveState: &wire.DriveState{ShiftState: ptrStr("P")}}

	fresh := s.degradeIfStale(driving, base.Add(-5*time.Minute))
	assert.Equal(t, "online", fresh.State, "a fresh cache must not be degraded")

	stale := s.degradeIfStale(driving, base.Add(-45*time.Minute))
	assert.Equal(t, "offline", stale.State, "a stale driving car must degrade to offline")
	assert.NotNil(t, stale.DriveState, "sub-objects must be preserved when degrading")

	settled := s.degradeIfStale(parked, base.Add(-45*time.Minute))
	assert.Equal(t, "asleep", settled.State, "a stale settled car must degrade to asleep")

	never := s.degradeIfStale(driving, time.Time{})
	assert.Equal(t, "online", never.State, "zero FetchedAt must not be degraded")

	s.staleAfter = 0
	off := s.degradeIfStale(driving, base.Add(-99*time.Hour))
	assert.Equal(t, "online", off.State, "staleAfter=0 disables the guard")
}

// TestTeslaCarTypeFromVIN_NoRivianFallback is the regression for the "Model 3 R1T"
// bug. The car_type resolver was only wired for the Rivian source, so a Tesla had no
// resolver; whenever no vehicle_config was cached (right after a restart, or while
// the car was asleep) the sink fell back to the literal car_type "model3" + trim
// "R1T", mislabeling the Model S as a Rivian. The fix derives car_type from the VIN
// (authoritative, restart-proof) and drops the "R1T" literal. A 5YJSA... VIN must
// resolve to Model S ("models") with no resolver and no snapshot, and the Rivian
// trim must never appear.
func TestTeslaCarTypeFromVIN_NoRivianFallback(t *testing.T) {
	ids, err := idmap.New("")
	require.NoError(t, err)

	prov := stubProvider{vehicles: []vehicle.Identity{
		{ID: "guid-s", VIN: "5YJSA1E5XSF550001", DisplayName: "Model S"},
	}}
	// nil resolver is the Tesla source path: no car_type resolver is wired for Tesla.
	s, err := newSourceAdapter(prov, ids, 30*time.Minute, nil, nil)
	require.NoError(t, err)

	products, err := s.Products()
	require.NoError(t, err)
	require.Len(t, products, 1)

	vd, err := s.VehicleData(products[0].ID)
	require.NoError(t, err)
	require.NotNil(t, vd.VehicleConfig)
	require.NotNil(t, vd.VehicleConfig.CarType)
	assert.Equal(t, "models", *vd.VehicleConfig.CarType, "a 5YJSA VIN must map to Model S")
	if vd.VehicleConfig.TrimBadging != nil {
		assert.NotEqual(t, "R1T", *vd.VehicleConfig.TrimBadging, "must never emit the Rivian R1T trim for a Tesla")
	}
}

// TestCarTypeFromVIN pins the VIN line-digit mapping (char 4: S/3/X/Y) gated on a
// Tesla WMI. The Rivian R1S case is the important one: an R1S VIN also carries 'S'
// at char 4, so the WMI gate (not the line digit alone) is what stops it being
// mis-read as a Tesla Model S. A non-Tesla VIN returns "" so a wired resolver or
// placeholder still applies.
func TestCarTypeFromVIN(t *testing.T) {
	cases := map[string]string{
		"5YJSA1E5XSF550001": "models",     // Fremont Model S
		"5XJSA1E5XSF551299": "models",     // rare secondary US WMI, Model S
		"5YJ3E1EA1KF000001": "model3",     // Fremont Model 3
		"5YJXCAE2XGF000001": "modelx",     // Fremont Model X
		"5YJYGDEE1LF000002": "modely",     // Fremont Model Y
		"5YJRA1E40A1000001": "roadster",   // Roadster (char 4 R)
		"7G2CEHED9RA000004": "cybertruck", // 7G2 truck WMI, Cybertruck (char 4 C)
		"7G2TSE1A0PF000005": "semi",       // 7G2 truck WMI, Semi (char 4 T)
		"7SAAGAAA1SF000006": "cybercab",   // Cybercab (char 4 A)
		"7SAYGDEE1PF000003": "modely",     // Gigafactory Texas Model Y (non-5YJ Tesla WMI)
		"3MWSA1E5XSF000099": "",           // 3MW is BMW de Mexico, NOT Tesla: must stay unmapped
		"7PDSGAAA3NN000001": "",           // Rivian R1S: 'S' at char 4 but NOT a Tesla WMI
		"7FCTGAAA0NN000002": "",           // Rivian R1T
		"1HGCM82633A000000": "",           // non-Tesla VIN
		"5YJ":               "",           // too short
		"":                  "",
	}
	for vin, want := range cases {
		assert.Equalf(t, want, carTypeFromVIN(vin), "carTypeFromVIN(%q)", vin)
	}
}

// TestVehicleConfigFakeTrimForUnsupportedModels: TeslaMate renders only S/3/X/Y, so a
// Cybertruck/Roadster/Semi/Cybercab car_type is remapped to a fake "model3" with the
// model name surfaced as the trim badge (it would otherwise show a blank model). A
// supported car_type passes through untouched.
func TestVehicleConfigFakeTrimForUnsupportedModels(t *testing.T) {
	unsupported := map[string]string{
		"cybertruck": "Cybertruck",
		"roadster":   "Roadster",
		"semi":       "Semi",
		"cybercab":   "Cybercab",
	}
	for ct, wantTrim := range unsupported {
		vc := vehicleConfig(nil, nil, Cfg{CarType: ct}, 0)
		require.NotNil(t, vc.CarType)
		assert.Equalf(t, "model3", *vc.CarType, "%s must ride as a fake model3", ct)
		require.NotNilf(t, vc.TrimBadging, "%s must surface its name as the trim", ct)
		assert.Equal(t, wantTrim, *vc.TrimBadging)
	}

	// Any car_type TeslaMate understands passes through UNCHANGED with its real trim,
	// including the refresh codenames it maps to S/X. Regression: the fake-trim once
	// over-matched and clobbered a real "lychee"/"models2" Model S to "model3".
	for _, ct := range []string{"models", "models2", "lychee", "tamarind", "model3", "modelx", "modely"} {
		vc := vehicleConfig(nil, &vehicle.State{CarType: ct, TrimBadging: "100D"}, Cfg{}, 0)
		require.NotNilf(t, vc.CarType, "%s", ct)
		assert.Equalf(t, ct, *vc.CarType, "%s must pass through untouched", ct)
		require.NotNilf(t, vc.TrimBadging, "%s", ct)
		assert.Equalf(t, "100D", *vc.TrimBadging, "%s keeps its real trim", ct)
	}
}

// TestStaleGateUsesStreamFreshness: the stale guard keys on AsOf (freshest of poll
// FetchedAt and stream StreamFields), so a live telemetry stream keeps a vehicle
// "online" even when polling has been dead longer than staleAfter. Regression for
// the overnight freeze: a dead poll degraded a still-streaming charge to offline,
// freezing TeslaMate's SOC mid-session.
func TestStaleGateUsesStreamFreshness(t *testing.T) {
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s := &sourceAdapter{staleAfter: 30 * time.Minute, now: func() time.Time { return base }}

	// Poll died 45m ago, but a stream frame landed 10s ago.
	streaming := vehicle.Snapshot{
		FetchedAt:    base.Add(-45 * time.Minute),
		StreamFields: base.Add(-10 * time.Second),
	}
	assert.False(t, s.isStale(streaming.AsOfTime()), "a fresh stream must keep the vehicle online despite a stale poll")

	// Both poll and stream stale: now genuinely dark, degrade.
	dark := vehicle.Snapshot{
		FetchedAt:    base.Add(-45 * time.Minute),
		StreamFields: base.Add(-40 * time.Minute),
	}
	assert.True(t, s.isStale(dark.AsOfTime()), "both surfaces stale must degrade to offline")
}
