# Real-world deployment notes (Tesla -> TeslaMate)

Field notes from deploying redswitchboard as the Tesla front-end for stock
TeslaMate against a live Tesla account: first as a **poll proxy**, then adding
**Fleet Telemetry streaming**. These are the non-obvious gotchas that only surface
against a real account (e.g. one that also owns a Tesla energy product), plus the
recommended setup. The reference docs are [SETUP.md](SETUP.md) (Tesla sources,
streaming, cost) and [config/README.md](../config/README.md).

> All hostnames, ports, VINs, and IPs below are placeholders. Substitute your own.

---

## Phase 1: poll proxy in front of TeslaMate

Point stock TeslaMate at redswitchboard's Fleet API surface (the third-party
provider block), so it reads the cache instead of polling Tesla:

```env
TESLA_API_HOST=http://redswitchboard:4000
TESLA_AUTH_HOST=http://redswitchboard:4000
TESLA_AUTH_PATH=/api/oauth2/v3
TOKEN=?token=<shared-token>     # literal "?token=" prefix; also hides the sign-in fields
```

Mint the source's `tesla.json` out of band (OAuth `authorization_code` flow against
your Fleet app), then start the proxy. Re-sign-in once in the TeslaMate UI.

### Optional: authenticate the read surface (P14, default-off)

The REST sink is unauthenticated by default. On a shared bridge that is a real
exposure: a compromised co-tenant can read live GPS off `/api/1/.../vehicle_data`.
Set a static bearer to gate the live-data read routes (`/api/1/products`,
`/api/1/vehicles`, `/api/1/vehicles/{id}[/vehicle_data|/source_extras]`, `/status`,
`/stats`). The `/api/oauth2/v3/token` bootstrap stays open by design (it is how a
consumer obtains the bearer).

It is opt-in and must be set on BOTH sides; setting it on only one breaks polling.

1. **redswitchboard side:** set the token in config (keep it secret, never log it):

   ```yaml
   sinks:
     tesla-fleet-poll-v1:
       auth_token: "<a-long-random-string>"
   ```

2. **TeslaMate side:** TeslaMate sends its stored access token as
   `Authorization: Bearer <access>` on every Tesla API call, so the bearer is just
   the access token it holds. Two ways to make that token equal `auth_token`:

   - **`teslamate auth` (no browser):** the access/refresh value it writes is
     `qts-<token>`, so set `auth_token` to that same `qts-<token>` string. Example:
     `redswitchboard teslamate auth --db ... --token s3cr3t` writes `qts-s3cr3t`, so
     set `auth_token: "qts-s3cr3t"` on the sink.
   - **UI sign-in:** the token endpoint returns `auth_token` as the `access_token`,
     so a fresh sign-in (or scheduled refresh) makes TeslaMate hold exactly the
     bearer the reads require. Re-sign-in once in the TeslaMate UI after setting it.

Verify: an unauthenticated `curl` of `/api/1/vehicles/<id>/vehicle_data` returns
`401`; the same request with `-H "Authorization: Bearer <auth_token>"` returns the
data; TeslaMate keeps polling. Leave `auth_token` unset to keep the prior behavior.

### Gotcha 1: an energy product breaks the vehicle listing

`GET /api/1/products` returns a heterogeneous list. A **vehicle's** `id` is a JSON
number, but an **energy site's** (Powerwall/solar) `id` is a JSON **string**. A
strict decode into a single struct with an `int64` id fails the whole list:

```
json: cannot unmarshal string into Go struct field ...id of type int64
```

so an account with any energy product can't list its car and the proxy crash-loops
at startup. Fix: decode `/products` leniently and key the "is this a car?" test off
`vehicle_id` (absent on energy products), not `id`. (Fixed upstream.)

### Gotcha 2: GPS is null unless you request `location_data`

The Fleet API omits `drive_state.latitude/longitude` from `vehicle_data` **unless
the request explicitly asks for the `location_data` endpoint**. The
`vehicle_location` scope grants *permission*; the `endpoints` query param *requests*
the data. Without it, location is always null. Beyond losing GPS, this crashes a
stock TeslaMate downstream: it feeds the nil coordinates into its SRTM terrain
elevation lookup and the whole app exits:

```
:gen_statem.call(TeslaMate.Terrain, {:get_elevation, {nil, nil}}, 2000)
:erlang.floor(nil)  ->  1st argument: not a number
```

Fix: request `endpoints=location_data;charge_state;climate_state;drive_state;gui_settings;vehicle_config;vehicle_state`
(URL-encode the `;`) on every `vehicle_data` poll. (Fixed upstream.)

### Cadence

Tesla bills per call and a wake is the priciest op, so idle polling is deliberately
slow (e.g. parked 5m, asleep 30m, never wake to poll). The trade-off in poll-only
mode: a wake/drive can be noticed minutes late. That latency is what streaming
removes. See [SETUP.md](SETUP.md) section 6 for the recommended `poll_overrides`.

---

## Phase 2: Fleet Telemetry streaming

The car opens an outbound mTLS WebSocket to a listener you run and pushes signal
deltas on change (~300x cheaper than polling; live drive quality). You never dial
the car.

### Server certificate: self-sign, do not use Let's Encrypt

Each vehicle's `fleet_telemetry_config` includes a `ca` field: the CA chain that
signed **your listener's** server cert, which the car uses to trust your server.
The car validates your server cert against the CA *you submit*, not a public root
store, so the cert does **not** need to be publicly trusted.

This means a rotating CA is a trap: with Let's Encrypt the chain changes on every
~90-day renewal, and **you must re-push `fleet_telemetry_config` to every vehicle**
each time. Use a **long-lived self-signed cert** (e.g. 10 years) for the listener
FQDN instead, and submit its stable CA once. No renewal churn.

```sh
# self-signed server cert + key for the public listener FQDN, ~10 years
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout telemetry.key -out telemetry.crt -days 3650 -nodes \
  -subj "/CN=telemetry.example.com" \
  -addext "subjectAltName=DNS:telemetry.example.com"
```

Point `stream.sources.tesla-fleet-stream-v1.server_cert`/`server_key` at these, and
submit `telemetry.crt` as the per-vehicle `ca`.

### Client certificate (verifying the car): identity, not a CA bundle

Tesla does **not** publish a downloadable "vehicle CA" to chain-verify the car's
client cert, and Tesla's own reference fleet-telemetry server ships **no**
`client_ca`/`verify_client` setting. It requires the car to present a client cert
and authenticates it by **identity**: the cert's issuer must be in Tesla's known-CA
allow-list and its CN is the VIN (validated by the vendored
`messages.CreateIdentityFromCert`).

redswitchboard does the same: `client_ca` is **optional**. Left empty (the common
case), the listener requires a client cert and authenticates by identity, with
**deny-by-default on unknown VINs** as the trust boundary. Set `client_ca` only if
you actually have a CA to chain-verify against, in which case it upgrades to full
verification.

Both trust-boundary rejections are observable. An accepted connection logs the peer
`RemoteAddr` + VIN (`vehicle connected from ... vin=...`), so connects are
attributable. A rejection increments
`redswitchboard_stream_source_rejects_total{reason}` and is logged once: `reason="identity"`
when the client cert fails Tesla's issuer allow-list / VIN extraction (logged at the
401), and `reason="unknown_vin"` when a verified cert's VIN is not in the known set
(the deny-by-default path; logged once per connection, not per frame). The label is a
bounded reason, never a raw IP or VIN.

### Public reachability

The car dials your listener over the public internet, so:

- The listener FQDN must resolve **publicly** to your WAN IP, and the chosen port
  must be **forwarded** to the host. Tesla **blocks** connections to private/local
  IPs, so a LAN-only or split-horizon setup will not work.
- The port is configurable in `fleet_telemetry_config` (default 443); a custom port
  works on current firmware.
- **Do not** put an L7 reverse proxy (e.g. nginx `proxy_pass`) in front of the
  listener: terminating TLS strips the client cert, and the VIN identity is lost.
  The raw TLS must reach the listener (direct, or L4/SNI passthrough only).
- Because raw TLS reaches the listener directly (no proxy can front it), a pre-auth
  TCP/TLS **handshake flood** would otherwise be unbounded. The listener caps
  concurrent connections at the raw socket, before the TLS handshake: a **global**
  cap (`max_conns`, default 32) and a **per-IP** cap (`max_conns_per_ip`, default 4,
  so one peer cannot consume the whole budget). One paired vehicle needs a single
  connection, so the defaults are deliberately small; raise them only if you pair
  many vehicles or front the listener with a shared-IP NAT. An accept over either
  cap is closed immediately and logged.

### Per-vehicle config must be SIGNED (the JWS gotcha)

`fleet_telemetry_config` carries the field list, the listener `hostname`/`port`,
the `ca` (your server cert chain), and per-field intervals. On a 2021+ vehicle a
**plain OAuth `POST` is rejected**:

```
"This endpoint must be called through the Vehicle Command HTTP Proxy."
```

The config must be sent as a **JWS signed by your partner fleet key** and validated
against your registered partner public key, so it can't be a bare `curl`. Route it
through Tesla's `vehicle-command` HTTP proxy configured with **your fleet private
key** (the same key behind the public key at your partner domain's `.well-known`,
which must already be paired to the vehicle -- confirm `key_paired: true`). The
proxy's `handleFleetTelemetryConfig` signs the body and forwards it to
`/api/1/vehicles/fleet_telemetry_config_jws`.

A one-shot is enough (no long-running sidecar): stand up `tesla-http-proxy` with the
fleet key, `POST` the config through it, tear it down. The proxy needs egress to
Tesla. A successful response is `{"response":{"updated_vehicles":1}}`; then poll
`GET /api/1/vehicles/{vin}/fleet_telemetry_config` until `synced:true` with your
`config` echoed back, and watch `redswitchboard_stream_source_connected`/`_frames_total`
rise as the car connects. Keep intervals conservative (`interval_seconds` is the
minimum gap between samples of a field; a parked car streams ~nothing). After a
billing cap-wipe you re-run this same signed `POST` to recover -- a restart does not.

### Billing caveat: the cap wipes telemetry config

If the billing cap is exceeded, Tesla **removes** `fleet_telemetry_config` from the
vehicles and does **not** restore it on its own (a restart won't fix it; you must
re-push per VIN). Alert on the `telemetry_config_wiped` signal, not just "is it set
on restart".

### What streaming covers (and what it doesn't)

Fleet Telemetry is **not drive-only**. The car streams whatever `Field`s the applied
`fleet_telemetry_config` requests, as **deltas**, whenever it is **awake**. The
applied set (see `scripts/apply-telemetry-signed.sh`; `interval_seconds` is the MIN gap
between samples of a field):

- **Driving:** Location / VehicleSpeed / Gear @10s, Odometer / RatedRange @60s (fast,
  high-volume -- this is where the timestamp bug below first manifested).
- **Charging:** Soc @60s, ACChargingPower @60s, DCChargingPower @30s (lower volume).
- **Parked but awake:** occasional frames as fields change.
- **Asleep:** nothing. The car sleeps to protect its 12V battery and the proxy never
  wakes it. The poll path (asleep cadence) covers sleep, and the cache degrades a
  stalled stream to offline past `poll.stale_after`.

After (re)applying the config, confirm the new fields actually arrive via
`redswitchboard_stream_source_field_frames_total{field="range"|"charge_power"}`.

The decoder also maps `InsideTemp` / `OutsideTemp` to the cached cabin/outside temps
(J1a), so a parked-but-awake car streams climate into the REST cache without a poll.
These two are NOT in the default applied set above: add `Field_InsideTemp` and
`Field_OutsideTemp` to `scripts/apply-telemetry-signed.sh` and re-apply, then confirm via
`redswitchboard_stream_source_field_frames_total{field="cabin_temp"|"outside_temp"}`.

The decoder also maps `TpmsPressure{Fl,Fr,Rl,Rr}` (bar) to the cached numeric tire
pressures (J1b): these are subscription-only over REST, so streaming is the only path
that fills `tpms_pressure_*` in the cache. Add `Field_TpmsPressureFl`,
`Field_TpmsPressureFr`, `Field_TpmsPressureRl`, `Field_TpmsPressureRr` to
`scripts/apply-telemetry-signed.sh` and re-apply, then confirm via
`redswitchboard_stream_source_field_frames_total{field="tpms_fl"|"tpms_fr"|"tpms_rl"|"tpms_rr"}`.

The decoder also maps the charge-detail and lock/sentry state (J1c): `ChargeLimitSoc`
(% target SOC), `TimeToFullCharge` (hours -> minutes), `ChargerVoltage` (V),
`ChargeAmps` (A, the live session current), `DetailedChargeState` (charger/plug enum),
`BatteryHeaterOn`, `Locked` (all four door locks), and `SentryMode` (Gear Guard
status). These are poll-owned today and go stale during a Tesla poll outage, so
streaming keeps them fresh on a parked-but-awake car. Add `Field_ChargeLimitSoc`,
`Field_TimeToFullCharge`, `Field_ChargerVoltage`, `Field_ChargeAmps`,
`Field_DetailedChargeState`, `Field_BatteryHeaterOn`, `Field_Locked`,
`Field_SentryMode` to `scripts/apply-telemetry-signed.sh` and re-apply, then confirm via
`redswitchboard_stream_source_field_frames_total{field="charge_limit_soc"|"time_to_full_charge"|"charger_voltage"|"charge_amps"|"detailed_charge_state"|"battery_heater_on"|"locked"|"sentry_mode"}`.

Everything else (config, OTA, doors/windows) stays poll-owned and is merged
field-by-field, so a delta frame never zeroes a field it omitted.

---

## Streaming sink to TeslaMate

The WSS sink to TeslaMate is internal (consumer-facing), separate from the inbound
mTLS source listener. Point TeslaMate at it and enable per-car streaming:

```env
TESLA_WSS_HOST=wss://redswitchboard:<sink-port>
TESLA_WSS_USE_VIN=true
```

Poll and stream are independent per side, so you can run the poll proxy and the
streaming path together: poll fills the cache for sign-in / `vehicle_data`, while
the stream sink pushes live drive data over WSS with zero extra Tesla calls.

If teslamate rejects the sink cert (`CLIENT ALERT: Fatal - Bad Certificate`), its
stream client is verifying the wss cert. For an internal/trusted hop, serve the sink
as plain `ws://` (omit the `tls:` block) and set `TESLA_WSS_HOST=ws://...` to match;
otherwise give the sink a cert teslamate's trust store accepts.

### Mid-drive crash: poll and stream clocks must agree (`Snapshot.AsOf`)

**Symptom (live):** during a drive TeslaMate logged tens of thousands of
`Discarded stale fetch result` warnings and its vehicle state machine crashed with
`(MatchError) ... end_date must be after start_date` (the `positive_duration`
constraint), so drives never closed (NULL distance/duration) and duplicated.

**Cause:** running poll + stream together, the two consumer-facing surfaces stamped
time from different clocks.

- The streaming `data:update` prefix used `max(FetchedAt, StreamFields)` -- fresh on
  every telemetry frame.
- The REST `vehicle_data` sub-object timestamps used `vs.LastUpdate` (a poll-owned
  field, **not** a streamed field), which **freezes between polls** during a drive
  (poll cadence is 30s while driving). The streamed `Location` carries no fix time,
  so the REST path could not borrow the stream's clock.

TeslaMate reads **both** surfaces. So every poll came back "older" than the live
stream -> discarded as stale; and at a drive transition it opened a state on one
clock and closed it on the other -> `end_date < start_date` -> crash.

**Fix:** one canonical timestamp, `Snapshot.AsOf`, stamped once in
`cache.Merger.commit()` as `max(prev.AsOf, FetchedAt, StreamFields)`. The
`max(prev.AsOf, ...)` makes it **monotonic non-decreasing per vehicle**, so no
surface can emit a backwards timestamp even if a stale poll lands after a fresh
stream frame. Both sinks now emit `AsOf` (`Snapshot.AsOfTime()` falls back to the
component max for un-merged snapshots, e.g. unit tests / the first frame). Guarded by
`TestMergeAsOfIsMonotonicAcrossSinks`.

---

## Poll cadence (AC/DC + stream-aware) and observability

The poll loop now adapts beyond plain per-state cadence:

- **AC vs DC charging.** A DC fast-charge session (the source's
  `fast_charger_present` flag, or delivered power `>= dc_threshold_kw`, default 25kW)
  polls at `charging_dc` (e.g. 60s) because SOC moves fast and a missed stop is
  costly; an AC session stays at the slower `charging` cadence (e.g. 10m) because it
  is slow and flat. Streamed sessions carry power but no flag, so the threshold
  covers them.
- **Stream-aware driving backoff.** While a telemetry stream is fresh
  (`stream_fresh_within`), the drive poll backs off to `driving_streaming` (e.g. 2m):
  the stream fills live position between polls, so the poll only refreshes poll-owned
  fields. On a stall the loop snaps back to the fast `driving` cadence (default 60s)
  within one tick, so the worst-case data gap is one backoff interval. This relies on
  the `Snapshot.AsOf` fix so the slower poll never hands TeslaMate a stale-vs-stream
  crossing timestamp. Gated on a stream cache being wired; the poll-only path is
  unchanged.

Metrics that make this legible (per source+vehicle on `/metrics`):

- `redswitchboard_source_vehicle_data_fetches_total` -- the **billed** call (online
  polls that fetch `vehicle_data`); free summary polls are excluded, so this is the
  cost truth. `redswitchboard_source_state_polls_total{state}` breaks polls down by
  derived state; `redswitchboard_source_state{state}` is the current state.
- `redswitchboard_source_scheduled_interval_seconds` (next cadence),
  `redswitchboard_source_last_poll_timestamp_seconds` (previous poll; +interval =
  next-poll ETA), `redswitchboard_source_stream_backoff_active` (drive backoff on).
- Stream health: `redswitchboard_stream_source_frame_gap_seconds` (inter-frame gap
  histogram -- sizes a future backoff), `..._field_frames_total{field}` (which
  signals actually arrive), `..._connects_total` (reconnect storms),
  `..._last_frame_age_seconds` (time since last frame). Frames/s and polls/s are
  `rate(...)` of the `_total` counters.

## Known issues (dig into later)

- **`http_requests_total{route=".../vehicle_data"}` reads in the millions, and that
  is REAL, not a counter bug.** Verified live: `/metrics` and the Tesla sink's own
  `/stats` agree (e.g. ~11.26M reads / ~538 reads/sec). These are **free cache
  reads** -- TeslaMate hot-looping the REST sink (most likely while the `AsOf` fix
  is not yet deployed and/or streaming is off, so TeslaMate falls back to REST and
  spins). They cost **nothing** at Tesla. The cost truth is
  `redswitchboard_source_vehicle_data_fetches_total` (the billed online fetches; e.g.
  ~267 over hours). If the read rate stays pathological after deploying the `AsOf`
  fix and re-enabling streaming, investigate the TeslaMate side (vehicle GenServer
  restart/fallback loop) separately.

---

## Cost (observed)

Behind the proxy, TeslaMate's Tesla **DATA** spend dropped ~95% (direct polling
~$3-5/day -> ~$0.10-0.30/day): consumers read the cache and only the poll source
reaches Tesla, on a cadence that backs off hard when idle (30m asleep, never wakes a
sleeping car). **STREAMING** events are billed at $0.
