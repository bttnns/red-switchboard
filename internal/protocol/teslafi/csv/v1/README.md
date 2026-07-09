# teslafi-csv-v1

The TeslaFi CSV protocol package (`internal/protocol/teslafi/csv/v1`), registered
as both a source and a sink. Unlike the live-API protocols, the "wire" here is a
TeslaFi-format CSV file: the monthly `TeslaFi<M><YYYY>.csv` format that TeslaMate's
native importer ingests (see docs.teslamate.org/docs/import/teslafi).

- The **SINK** exports canonical snapshots to TeslaFi CSV. `ExportHistory` writes
  monthly files to `export_dir` so they can be handed to TeslaMate's importer; the
  chi `Handler` also serves current state at `GET /export.csv` so a paired
  teslafi-csv source can fetch it over HTTP.
- The **SOURCE** reads a directory of TeslaFi CSV files (or fetches `/export.csv`
  via HTTP URL) and replays them as canonical snapshots. A time cursor advances from
  the earliest `Date` at `time_scale` speed; `Poll` returns the snapshot at the
  cursor for each vehicle, preserving the original recorded timestamp.

## Key files

- `csv.go`: the `Row` struct (TeslaFi column schema), `dateLayout`, file naming
  (`TeslaFi<M><YYYY>.csv`), and `readDir`/`writeMonthly`.
- `source.go`: `csvSource`: indexes rows by vehicle, advances a time cursor, returns
  the snapshot at the cursor per `Poll`. `newSource` is the `source.Factory`.
- `sink.go`: `csvSink`: `ExportHistory` (batch monthly files) and `GET /export.csv`
  (live current state). `newSink` is the `sink.Factory`; `NewExportSink` builds it
  directly for the `mock --since` generator.
- `source_mapping.go`: CSV `Row` -> `vehicle.Snapshot` (miles/mph to SI, state
  strings to typed enums).
- `sink_mapping.go`: `vehicle.Snapshot` -> CSV `Row` (SI to miles/mph, typed enums
  to TeslaFi state strings).

## Non-obvious mapping details

- `speed` and `shift_state` write as empty strings when parked. TeslaMate detects
  drive rows from `shift_state` in `{D, R, N}`.
- DC fast charge sessions (charger_power >= 25 kW) write `fast_charger_present=true`
  and leave `charger_voltage`/`charger_actual_current` at zero. This avoids
  TeslaMate's smallint overflow: it multiplies `voltage * current` as smallints, which
  overflows at real DC magnitudes (e.g. 400 V * 500 A = 200 000 > 32 767).
- `outside_temp` has no canonical equivalent; the sink writes a mild constant (20 C).
- `id` and `vehicle_id` write as the vehicle's 1-based position in the Provider list.
  TeslaMate randomizes `id` internally but needs the column present to create the car.
