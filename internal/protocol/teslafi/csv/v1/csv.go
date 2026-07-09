// Package v1 is the teslafi-csv protocol: a first-class source/sink pair in the
// hub model whose external "wire" is a TeslaFi-format CSV file (the shape
// TeslaMate's official importer ingests, docs.teslamate.org/docs/import/teslafi).
//
//   - the SINK exports canonical snapshots to TeslaFi CSV (an sink.Exporter, not
//     an HTTP surface), so a recorded/synthetic history can be handed to
//     TeslaMate's native importer.
//   - the SOURCE reads a TeslaFi CSV back into canonical snapshots and replays it,
//     so `serve --source teslafi-csv-v1 --sink <any>` can play a recording out
//     through any protocol.
//
// TeslaMate's importer reads each row into a name-keyed map and maps known column
// names into the Tesla API vehicle sub-states, ignoring unknown columns; only the
// `Date` column is special-cased (it sets every sub-state timestamp). So Row
// carries just the columns that drive TeslaMate's panels -- not the full ~150-
// column TeslaFi header -- and the importer fills the rest with defaults.
package v1

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gocarina/gocsv"
)

// dateLayout is the naive-local timestamp format TeslaMate's LineParser accepts
// ("{YYYY}-{M}-{D} {h24}:{m}:{s}"); the import is told the matching timezone.
const dateLayout = "2006-01-02 15:04:05"

// Row is one TeslaFi CSV record. Column names match the TeslaApi vehicle sub-
// state field names so TeslaMate routes them into charge/drive/climate/vehicle
// state. String fields are used where "" must mean "absent" (TeslaMate maps an
// empty cell to nil); numeric fields write a literal value every row.
type Row struct {
	Date        string `csv:"Date"`
	ID          int    `csv:"id"` // Tesla session id; TeslaMate randomizes it but the column must be present to create a car
	VehicleID   int    `csv:"vehicle_id"`
	DisplayName string `csv:"display_name"`
	VehicleName string `csv:"vehicle_name"`
	VIN         string `csv:"vin"`
	State       string `csv:"state"` // online / asleep / offline

	// drive_state
	Latitude   float64 `csv:"latitude"`
	Longitude  float64 `csv:"longitude"`
	Heading    int     `csv:"heading"`
	Speed      string  `csv:"speed"`       // mph; "" when not driving
	ShiftState string  `csv:"shift_state"` // D/R/N/P; "" when parked
	Power      int     `csv:"power"`       // kW (drive power, +/-)
	Odometer   float64 `csv:"odometer"`    // miles

	// charge_state (ranges in miles)
	BatteryLevel         int     `csv:"battery_level"`
	UsableBatteryLevel   int     `csv:"usable_battery_level"`
	ChargeLimitSoc       int     `csv:"charge_limit_soc"`
	BatteryRange         float64 `csv:"battery_range"`
	EstBatteryRange      float64 `csv:"est_battery_range"`
	IdealBatteryRange    float64 `csv:"ideal_battery_range"`
	ChargingState        string  `csv:"charging_state"` // Charging/Complete/Disconnected/Stopped
	ChargeEnergyAdded    float64 `csv:"charge_energy_added"`
	ChargerPower         int     `csv:"charger_power"`
	ChargerVoltage       int     `csv:"charger_voltage"`
	ChargerActualCurrent int     `csv:"charger_actual_current"`
	ChargerPhases        string  `csv:"charger_phases"`       // "" for DC, "1"/"3" for AC
	FastChargerPresent   bool    `csv:"fast_charger_present"` // true => DC
	FastChargerType      string  `csv:"fast_charger_type"`
	TimeToFullCharge     float64 `csv:"time_to_full_charge"` // hours

	// climate_state (Celsius)
	InsideTemp        float64 `csv:"inside_temp"`
	OutsideTemp       float64 `csv:"outside_temp"`
	IsClimateOn       bool    `csv:"is_climate_on"`
	DriverTempSetting float64 `csv:"driver_temp_setting"`

	// vehicle_state
	CarVersion string `csv:"car_version"`
}

// parsedDate parses the Row's Date column in loc.
func (r Row) parsedDate(loc *time.Location) (time.Time, error) {
	return time.ParseInLocation(dateLayout, r.Date, loc)
}

// fileName is the TeslaFi monthly filename for a timestamp: "TeslaFi<M><YYYY>.csv"
// (no zero-padding on the month, matching TeslaFi's own export, e.g. June 2016 ->
// TeslaFi62016.csv).
func fileName(t time.Time) string {
	return fmt.Sprintf("TeslaFi%d%d.csv", int(t.Month()), t.Year())
}

// writeMonthly groups rows by calendar month (parsed in loc) and writes one
// TeslaFi<M><YYYY>.csv per month into dir.
func writeMonthly(dir string, rows []Row, loc *time.Location) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("teslafi-csv: mkdir %s: %w", dir, err)
	}
	byMonth := map[string][]Row{}
	for _, r := range rows {
		t, err := r.parsedDate(loc)
		if err != nil {
			return fmt.Errorf("teslafi-csv: bad Date %q: %w", r.Date, err)
		}
		byMonth[fileName(t)] = append(byMonth[fileName(t)], r)
	}
	for name, monthRows := range byMonth {
		if err := writeFile(filepath.Join(dir, name), monthRows); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, rows []Row) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("teslafi-csv: create %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("teslafi-csv: close %s: %w", path, cerr)
		}
	}()
	if err := gocsv.MarshalFile(&rows, f); err != nil {
		return fmt.Errorf("teslafi-csv: write %s: %w", path, err)
	}
	return nil
}

// readDir loads every TeslaFi*.csv in dir into Rows, sorted by Date (in loc).
func readDir(dir string, loc *time.Location) ([]Row, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "TeslaFi*.csv"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var all []Row
	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("teslafi-csv: open %s: %w", path, err)
		}
		var rows []Row
		err = gocsv.UnmarshalFile(f, &rows)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("teslafi-csv: parse %s: %w", path, err)
		}
		all = append(all, rows...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		ti, _ := all[i].parsedDate(loc)
		tj, _ := all[j].parsedDate(loc)
		return ti.Before(tj)
	})
	return all, nil
}
