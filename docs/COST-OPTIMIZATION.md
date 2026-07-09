# Cost optimization: poll -> stream

Goal: cut billed Tesla `vehicle_data` (DATA) calls by moving fields onto free
Fleet Telemetry streaming, without losing any data TeslaMate persists.

## Mental model (why this is the whole game)

There are two poll loops; only one costs money:

- TeslaMate -> TRS: **free** (local REST/WS). TeslaMate's own cadence tricks act
  against TRS, not Tesla, so they do not affect the bill.
- TRS -> Tesla: **billed**. `vehicle_data` (DATA) is the only metered call; the
  `/vehicles/{id}` summary (online/asleep/offline) is free and never wakes the car.
  Fleet Telemetry STREAMING is billed per signal but is $0 under the account credit
  at current volume.

So the lever is: how often TRS billed-polls Tesla, and how many of the fields that
force those polls could instead arrive for free over streaming.

## Baseline (the clean reference)

Anchor all analysis on **today + every future drive/charge**. Pre-2026-06-23 data
may carry already-fixed bugs (e.g. the NULL-drive crash fixed in #51, odometer
regress in #52), so it is NOT a valid baseline.

Reference numbers from 2026-06-23 (two back-to-back drives, post #51/#52):

- 48 billed `vehicle_data`/40h; **37 in the drive window** ($0.07), 11 elsewhere.
- 688 stream signals = **$0** (confirmed against Tesla's billing ledger).
- Cost model validated: Tesla ~$0.0019/DATA-call vs TRS estimate $0.002 (~6% high).
- Driving polls landing at **~2 min** spacing, not the intended 3 min.
- **55 stream connect/disconnect events in one 57-min drive** (car-initiated; TRS
  `idle_timeouts_total=0`). This churn fires a boundary -> PollNow each reconnect,
  manufacturing a billed poll every ~2 min. Driving polls are ~60% of the bill.

## Field verdicts

Legend: OK = optimal, leave it | CFG = config-only (decoder exists; add the field
to the per-VIN `fleet_telemetry_config`, no code) | DEC = needs a TRS decoder |
POLL = poll-only forever (no Tesla stream field).

### Drive / motion
- lat/long, speed, shift_state, odometer, heading -- OK (streamed).
- drive power -- POLL. No Fleet Telemetry field exists (confirmed against the full
  239-field proto). Wire Fleet API `drive_state.power` into a canonical slot and
  emit it (today nil at `poll/v1/sink_mapping.go:197`). Deriving from Pack V*I is
  not worth it.

### Charge
- soc/ranges/charger_power -- OK (streamed).
- charging_state, charger_voltage (today synthesized 240/nil), charger_actual_current,
  charge_limit_soc, time_to_full_charge, battery_heater_on -- CFG (decoders exist).
- charge_energy_added (TeslaMate-REQUIRED) -- DEC: add `AC/DCChargingEnergyIn`, else
  keep a charge-boundary poll. Do not regress it.
- charge_rate -- DEC (today hardcoded 0; `ChargeRateMilePerHour`). Low value.
- charger_phases / fast_charger_* -- OK (inferred from kW).
- charge_port_door_open -- POLL, minor.

### Climate
- inside_temp / outside_temp -- CFG (decoders exist).
- fan_status, seat heaters, temp setpoints, defroster, steering-wheel heater -- DEC,
  low value. Skip; keep on the slow parked poll.

### Vehicle state
- tpms_pressure x4 -- OK (streamed; the ONLY source, REST returns 0).
- locked, sentry_mode -- CFG (decoders exist).
- tpms_soft_warning, doors/windows, car_version/software_update -- DEC, low value. Skip.
- is_user_present -- POLL (no stream field).

### Config (identity)
- car_type / trim / color / wheel -- POLL once, static. Correct as-is.

## Phases (one PR per phase; owner reviews/merges/deploys between each)

### Phase 1 -- stream stability (biggest lever, no field changes)
- [ ] Capture reconnect count + driving-poll cadence on the next real drive.
- [ ] Root-cause the ~2-min churn: Tesla telemetry-config TTL, mTLS listener
      keepalive/`max_conns`, or car-side connection lifetime.
- [ ] Cut the reconnect -> boundary -> PollNow path (or widen `stream_fresh_within`)
      so a reconnect cannot manufacture a billed poll.
- [ ] Gate: next drive shows ~1 reconnect, driving polls hold at 3 min, drive-window
      DATA down ~33%+.

### Phase 2 -- config-only stream widening (free, no code)
- [ ] Add to `fleet_telemetry_config`: DetailedChargeState, ChargerVoltage,
      ChargeAmps, ChargeLimitSoc, TimeToFullCharge, BatteryHeaterOn, InsideTemp,
      OutsideTemp, Locked, SentryMode.
- [ ] Confirm each arrives (`stream_source_field_frames_total`) and lands in TeslaMate.
- [ ] Watch signal volume stays in the free tier.
- [ ] Gate: charging + parked-awake billed polls trend toward zero.

### Phase 3 -- charge completeness (decoder code)
- [ ] Decode `AC/DCChargingEnergyIn` for charge_energy_added (required field).
- [ ] Optional: charge_rate (`ChargeRateMilePerHour`).
- [ ] Gate: next full charge streams end-to-end with correct kWh.

### Phase 4 -- drive power (poll-path feature)
- [ ] Add canonical drive-power slot (distinct from `LiveSession.PowerKw`).
- [ ] Populate from Fleet API `drive_state.power` on the poll path.
- [ ] Emit it at `poll/v1/sink_mapping.go:197`.
- [ ] Gate: next drive populates `drives.power_max` in TeslaMate.

### Deferred / skip
- Low-value DEC fields (seat heaters, setpoints, fan, doors/windows,
  tpms_soft_warning, software_update/car_version).
- Drive-power-over-stream derivation (Pack V*I).
- is_user_present, charge_port_door_open, vehicle_config -- poll-only, correct as-is.

## Validation

Every phase validates against today + the next real drive/charge, not prior days.
The TeslaMate DB check script (the one that audits NULL locations, odometer
monotonicity, charge rows) is the per-drive gate.
