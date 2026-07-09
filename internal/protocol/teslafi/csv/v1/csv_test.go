package v1

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// sampleSnapshot is a driving-while-charging-complete-ish canonical state used to
// exercise the mapping both ways.
func sampleSnapshot(t time.Time) vehicle.Snapshot {
	return vehicle.Snapshot{
		FetchedAt: t,
		State: &vehicle.State{
			Power:           vehicle.PowerOnline,
			LastUpdate:      t,
			Gear:            vehicle.GearDrive,
			SpeedMps:        28,
			HeadingDeg:      90,
			OdometerMeters:  32_186_880, // 20000 mi
			RangeKm:         322,        // ~200 mi
			BatteryLevelPct: 64,
			BatteryLimitPct: 90,
			Charger:         vehicle.ChargerDisconnect,
			Plug:            vehicle.PlugDisconnected,
			CabinTempC:      21,
			OutsideTempC:    23,
			DriverSetpointC: 21,
			OtaVersion:      "2026.6.1",
			Location:        &vehicle.Location{Latitude: 37.77, Longitude: -122.41, TimeStamp: t},
		},
	}
}

func TestRowRoundTrip(t *testing.T) {
	loc := time.UTC
	when := time.Date(2026, 6, 13, 8, 30, 0, 0, loc)
	in := sampleSnapshot(when)

	row := toRow(in, 1, vehicleIdent{name: "Mock R1T", vin: "VIN123"}, loc)
	got := row.toSnapshot(loc)

	if !got.FetchedAt.Equal(when) {
		t.Errorf("timestamp not preserved: got %v want %v", got.FetchedAt, when)
	}
	if got.State.BatteryLevelPct != in.State.BatteryLevelPct {
		t.Errorf("battery: got %v want %v", got.State.BatteryLevelPct, in.State.BatteryLevelPct)
	}
	if got.State.Gear != vehicle.GearDrive {
		t.Errorf("gear: got %v want Drive", got.State.Gear)
	}
	// Odometer/range survive the SI<->miles round trip within rounding.
	if d := got.State.OdometerMeters - in.State.OdometerMeters; d < -2000 || d > 2000 {
		t.Errorf("odometer drifted too far: got %d want ~%d", got.State.OdometerMeters, in.State.OdometerMeters)
	}
	if got.State.Location == nil || got.State.Location.Latitude == 0 {
		t.Errorf("location lost: %+v", got.State.Location)
	}
	if got.State.OutsideTempC != in.State.OutsideTempC {
		t.Errorf("outside_temp: got %v want %v", got.State.OutsideTempC, in.State.OutsideTempC)
	}
}

func TestChargingRoundTrip(t *testing.T) {
	loc := time.UTC
	when := time.Date(2026, 6, 13, 9, 0, 0, 0, loc)
	snap := vehicle.Snapshot{
		FetchedAt: when,
		State: &vehicle.State{
			Power:           vehicle.PowerOnline,
			LastUpdate:      when,
			Gear:            vehicle.GearPark,
			BatteryLevelPct: 40,
			BatteryLimitPct: 90,
			Charger:         vehicle.ChargerCharging,
			Plug:            vehicle.PlugConnected,
		},
		Live: &vehicle.LiveSession{PowerKw: 200, CurrentA: 500, TotalChargedEnergy: 12.5, TimeRemainingSec: 1800},
	}
	row := toRow(snap, 1, vehicleIdent{}, loc)
	if row.ChargingState != "Charging" {
		t.Fatalf("charging_state: got %q want Charging", row.ChargingState)
	}
	if !row.FastChargerPresent {
		t.Errorf("200kW should be flagged DC fast")
	}
	got := row.toSnapshot(loc)
	if got.Live == nil || got.Live.TotalChargedEnergy != 12.5 {
		t.Errorf("live session lost: %+v", got.Live)
	}
}

func TestWriteReadMonthly(t *testing.T) {
	loc := time.UTC
	dir := t.TempDir()
	rows := []Row{
		toRow(sampleSnapshot(time.Date(2026, 6, 13, 8, 0, 0, 0, loc)), 1, vehicleIdent{name: "A", vin: "V1"}, loc),
		toRow(sampleSnapshot(time.Date(2026, 7, 1, 8, 0, 0, 0, loc)), 1, vehicleIdent{name: "A", vin: "V1"}, loc),
	}
	if err := writeMonthly(dir, rows, loc); err != nil {
		t.Fatalf("writeMonthly: %v", err)
	}
	// Two distinct months -> two files.
	for _, name := range []string{"TeslaFi62026.csv", "TeslaFi72026.csv"} {
		if _, err := filepath.Glob(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	back, err := readDir(dir, loc)
	if err != nil {
		t.Fatalf("readDir: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("readDir rows: got %d want 2", len(back))
	}
	if back[0].VIN != "V1" {
		t.Errorf("vin lost on round trip: %q", back[0].VIN)
	}
}
