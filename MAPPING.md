# MAPPING.md: Rivian to Tesla Fleet API field mapping

Field-by-field reference for the flagship path: how a Rivian snapshot (gateway `vehicleState` plus
optional charging `getLiveSessionData`) becomes the Tesla `vehicle_data` shape. Mapping is documented
per direction against the canonical model (`internal/vehicle/vehicle.go`): a source DECODES its wire
into canonical, and a sink ENCODES canonical into its wire, with the Tesla Fleet shape as the reference.
The code is the source of truth: the Rivian source decode in
`internal/protocol/rivian/graphql/poll/v1/source_mapping.go` (wire/units in `types.go`), the Tesla Fleet
sink encode in `internal/protocol/tesla/fleet/poll/v1/sink_mapping.go` (emitted fields in `wire/wire.go`).
If this disagrees with the code, the code wins.

Each table lists the fields redswitchboard actually populates; the many Tesla fields with no Rivian
equivalent are emitted as JSON `null` and summarized in one line per sub-object (the rule is in
[Unmappable fields](#rules-for-unmappable-fields)).

## Legend

- **(field name)**: from a Rivian field (from `rivian.VehicleState` unless prefixed `live.`).
- **`constant`**: a fixed value redswitchboard always emits (Rivian has no equivalent, a Tesla Fleet API consumer needs one).
- **`unset (null)`**: emitted as JSON `null` (Rivian can't provide it; a constant would mislead).
- **`hold-last-known`**: load-bearing; on an empty/junk Rivian reading, reuse the previous translated
  value instead of nulling.
- **`inferred`**: computed from one or more Rivian fields.

Unit conversions (in the Tesla sink encode, helpers in `internal/units`): meters->miles (odometer), km->miles (range),
m/s->mph (speed), Celsius passthrough (temps), seconds->hours (live time remaining), minutes->hours
(charge-time fallback). All timestamps are numeric Unix ms (a null timestamp crashes the consumer).

## Top-level VehicleData

| Tesla field | Rivian source | Notes |
| --- | --- | --- |
| `id` / `vehicle_id` / `vin` / `display_name` | `constant` (identity) | From `IDs`, minted by idmap. |
| `state` | PowerState + CloudConnectionOnline | enum `topState`; online / asleep / offline. |
| `api_version` | `constant` (71) | `undocumentedAPIVersion`. |
| `in_service` | `constant` (false) | |

`option_codes` is `unset (null)`.

`state` enum (`topState`): not cloud-online -> `offline`; `sleep` -> `asleep`; `go`/`ready` ->
`online`; `standby`/`vehicle_reset`/`unreachable`/empty/unknown -> `offline`.

## drive_state

| Tesla field | Rivian source | Conversion | Notes |
| --- | --- | --- | --- |
| `timestamp` | snapshot time | ms epoch | GPS fix time, else LastUpdate, else now. |
| `shift_state` | GearStatus | enum `shiftState`, `hold-last-known` | Empty gear holds prev. |
| `latitude` / `longitude` | Location.Lat/Long | none | `hold-last-known` when Location absent. |
| `gps_as_of` | Location.TimeStamp | ms epoch /1000 | Falls back to now/1000. Every source stamps Location.TimeStamp from its frame time (poll/CSV: source time; the two stream decoders: the per-frame stream time), never Go-zero, which would emit a garbage `gps_as_of`. |
| `heading` | GnssBearing | float to int (deg) | |
| `speed` | GnssSpeed | m/s to mph | |
| `power` | DrivePowerKw | none (kW, signed) | Tesla poll only (decodes `drive_state.power`); feeds the consumer's `drives.power_max`. Negative = regen. nil/unset on the Rivian poll path and on Fleet Telemetry (no drive-power field). |

`unset (null)`: `native_*`, all `active_route_*` (no navigation over polling).

`shift_state` enum: `drive`->`D`; `reverse`->`R`; `neutral`->`N`; `park`/empty/unknown->`P`.

## charge_state

| Tesla field | Rivian source | Conversion | Notes |
| --- | --- | --- | --- |
| `timestamp` | snapshot time | ms epoch | |
| `battery_level` / `usable_battery_level` | BatteryLevel | float to int (%) | Same source. |
| `charge_limit_soc` | BatteryLimit | float to int (%) | Target SOC. |
| `ideal_battery_range` / `est_battery_range` / `battery_range` | DistanceToEmpty | km to miles, `hold-last-known` | Load-bearing; same value; zero holds prev. |
| `charging_state` | ChargerState + ChargerStatus | enum `chargingState` | |
| `charge_port_door_open` | ChargePortState | bool (`== "open"`) | |
| `charger_power` | live.Power (else 0) | kW, rounded to whole | Numeric, never blank; the consumer casts it to a DB `:integer`, so a fractional AC value fails the cast. |
| `charger_actual_current` | live.Current (else 0) | float to int (A) | Unset for DC (see AC/DC). |
| `charge_energy_added` | live.TotalChargedEnergy | none (kWh), `hold-last-known` | Load-bearing; no session: carry prev, else 0. |
| `time_to_full_charge` | live.TimeRemaining (else TimeToEndOfCharge) | s->h (live) / min->h (fallback) | 0 when not charging. |
| `charge_rate` | `constant` (0) | | |
| `fast_charger_present` / `fast_charger_type` / `fast_charger_brand` | `inferred` from live.Power | | DC (>=25kW): true / `Combo` / `Rivian`; else false / null / null. |
| `charger_phases` / `charger_voltage` | `inferred` | | AC: 2 / 240. DC or none: null. |

`unset (null)`: all other `charge_*` / `managed_charging_*` / `scheduled_charging_*` fields,
`charge_miles_added_*`, `conn_charge_cable`, `battery_heater_on`, etc.

`charging_state` enum: ChargerStatus `chrgr_sts_not_connected` -> `Disconnected`; ChargerState
`charging_active`/`charging_connecting` -> `Charging`; `charging_ready` -> `Stopped`; empty ->
`Disconnected`; unknown -> `Stopped`.

**AC/DC inference** (`dcChargeThresholdKw` = 25.0): no session or power <= 0 -> not fast, phases/
voltage/type/brand unset; power in (0, 25) kW -> AC (`charger_phases=2`, `charger_voltage=240`, kept
in smallint range); power >= 25 kW -> DC fast (`fast_charger_present=true`, type `Combo`, brand
`Rivian`; phases/voltage/`charger_actual_current` unset, since real DC current x voltage overflows
the consumer's smallint and crashes finalization).

## climate_state

| Tesla field | Rivian source | Notes |
| --- | --- | --- |
| `timestamp` | snapshot time | ms epoch. |
| `inside_temp` | CabinInteriorTemp | Celsius passthrough. |
| `outside_temp` | OutsideTempC | Celsius; null when 0 (source did not report it, e.g. Rivian). |
| `driver_temp_setting` | CabinClimateDriverTemperature | Celsius; null when 0. |
| `seat_heater_left/right/rear_left/rear_right` | Seat*Heat | enum `seatHeatLevel` 0-3, null unknown. |
| `steering_wheel_heater` | SteeringWheelHeat | bool `isOn`. |
| `is_front_defroster_on` | DefrostDefogStatus | bool `isOn`. |
| `is_preconditioning` / `is_climate_on` | CabinPreconditioningStatus | bool `preconditioningActive`. |

`unset (null)`: all other climate fields (`battery_heater`, `fan_status`,
`*_temp_direction`, the remaining seat heaters, etc.).

`seatHeatLevel`: `Off`->0, `Level_1..3`->1..3, unknown/empty->null. `preconditioningActive`
(case-insensitive): `active`/`complete_maintain`/`initiate`->true, else false. `isOn`: non-empty and
not `Off` (case-insensitive).

## vehicle_state

| Tesla field | Rivian source | Conversion | Notes |
| --- | --- | --- | --- |
| `timestamp` | snapshot time | ms epoch | |
| `vehicle_name` | `constant` (DisplayName) | none | |
| `is_user_present` | PowerState | bool (`== "go"`) | |
| `sentry_mode` | GearGuardVideoStatus | bool `gearGuardArmed` | Rivian's Sentry equivalent. |
| `odometer` | VehicleMileage | meters to miles, `hold-last-known` | Load-bearing; zero holds prev. |
| `locked` | door `*Locked` | bool `allLocked` | True only if all four doors `locked`. |
| `df`/`dr`/`pf`/`pr` | Door*Closed | open/closed to 1/0 | 1 = open. |
| `ft`/`rt` | ClosureFrunkClosed / ClosureLiftgateClosed | open/closed to 1/0 | Frunk / liftgate. |
| `fd_window`/`fp_window`/`rd_window`/`rp_window` | Window*Closed | open/closed to 1/0 | 1 = open. |
| `car_version` | OtaCurrentVersion | `hold-last-known` | Empty holds prev. |
| `software_update` | OTA fields | sub-object | Always non-null (below). |
| `tpms_soft_warning_fl/fr/rl/rr` | TirePressureStatus* | enum `tpmsWarning` | null unknown, false OK, else true. |

`unset (null)`: numeric `tpms_pressure_*` (subscription-only) and all other vehicle_state fields
(`autopark_*`, `homelink_*`, `sun_roof_*`, `valet_*`, `remote_start*`, etc.).

`gearGuardArmed` (case-insensitive): `enabled`/`engaged`->true, else false. `allLocked`: all four door
`*Locked` equal `locked` (any empty/unlocked, or empty set -> false). `tpmsWarning`: empty->null,
`OK`->false, anything else->true. Door/window/closure: the Rivian `*Closed` sensor reports `"open"`
when open; `invertClosed` normalizes empty to closed, then `open`->1, else 0.

### software_update (always non-null)

| Tesla field | Rivian source | Notes |
| --- | --- | --- |
| `status` | OtaStatus | enum `softwareUpdateStatus`; "" when no update. |
| `version` | OtaAvailableVersion | |
| `download_perc` | OtaDownloadProgress | percent. |
| `install_perc` | OtaInstallProgress | float rounded to int. |

`unset (null)`: `expected_duration_sec`, `scheduled_time_ms`.

`softwareUpdateStatus` (case-insensitive): `installing`->`installing`;
`downloading`/`ready_to_download`->`downloading`;
`ready_to_install`/`scheduled_to_install`/`awaiting_install`/`install_countdown`/`preparing`->`available`;
else `""`.

## vehicle_config

| Tesla field | Rivian source | Notes |
| --- | --- | --- |
| `timestamp` | snapshot time | ms epoch. |
| `car_type` | `constant` (cfg.CarType, default `model3`) | Must be non-null or the FSM crashes. |
| `trim_badging` | `constant` (cfg.Model, default `R1T`) | Rivian model (R1T/R1S/...). |
| `rhd` | `constant` (false) | |

`unset (null)`: every other config field (`exterior_color`, `wheel_type`, `has_air_suspension`,
`seat_type`, `sun_roof_installed`, etc.).

## products / summary

`GET /api/1/products` (`Product`) and `GET /api/1/vehicles[/{id}]` (`Summary`) share five fields: the
four identity constants (`id`, `vehicle_id`, `vin`, `display_name`) plus `state` (same `topState`
mapping as top-level). The consumer keeps `/products` entries that carry a `vehicle_id` and treats them
as cars.

## Rules for unmappable fields

### Rivian data Tesla lacks: `rivian_extras`

Rivian data with no Tesla equivalent is not discarded. `GET /api/1/vehicles/{id}/rivian_extras`
returns the raw latest snapshot: `{ "response": { "vehicle_state": ..., "live_session": ...|null,
"fetched_at": ... } }`. Notable Rivian-only fields surfaced there:

- `DriveMode`, `GearGuardLocked` (distinct from door locks), `GnssAltitude`, `CloudLastSync`.
- `BatteryCapacity`: dynamic gross pack kWh. Tesla has no capacity field, so this is a Rivian extra
  only; the consumer derives usable capacity itself from charge sessions (`charge_energy_added` over a
  >10 min charge).
- `ClosureTonneauClosed`, `ClosureTailgateClosed`: R1T bed tonneau and tailgate (Tesla `ft`/`rt` cover
  only frunk and liftgate).
- LiveSessionData economics: `KilometersChargedPerHr`, `RangeAddedThisSession`, `Soc`, `CurrentPrice`,
  `CurrentCurrency`, `TimeElapsed`, `VehicleChargerState`.

### Tesla fields Rivian cannot provide

Emit `null` when unknown; a `constant`/0 only when genuinely constant or zero. The non-null
exceptions: the constants `api_version` (71), `in_service` (false), `car_type`, `trim_badging`, `rhd`,
and the identity fields; and genuine zeros (`charge_rate`, plus `charger_power` /
`charger_actual_current` / `time_to_full_charge` / `charge_energy_added` defaulting to 0 only with no
live session and no prior value).

### The non-null contract (what the consumer requires)

- All five sub-objects always present (a null one makes the consumer discard the snapshot).
- Every sub-object `timestamp` is numeric Unix ms (null crashes the consumer; prefers GPS fix time, else
  `LastUpdate`, else now).
- `drive_state.latitude`/`longitude` are held last-known, never nulled (non-numeric positions are
  dropped).
- `software_update` is always an object with a `status` string (`""` when no update).
- `vehicle_config` always carries at least `car_type` (else `identify/1` crashes).
- Load-bearing numerics are non-null and hold-last-known on junk: the three ranges,
  `charge_energy_added`, `odometer`, `shift_state`.

### Asleep fallback

Before the first poll (`vs == nil`), `asleepFallback` serves a minimal valid payload: it reuses `prev`
if available (last-known, `state="asleep"`), else all five sub-objects with safe defaults (battery 0,
ranges 0, `charge_energy_added` 0, `charging_state="Disconnected"`, `is_climate_on=false`,
`shift_state=null`, `odometer` 0, `locked=true`, `software_update.status=""`, numeric timestamps).
