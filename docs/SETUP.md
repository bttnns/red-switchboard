# Tesla setup, streaming, and cost

Operator guide for everything beyond the flagship Rivian -> TeslaMate path: running a **Tesla source**,
turning on **streaming** (the cost win), enabling the gated **command** write path, and tuning **poll
cadence**. The flagship quick start lives in [README.md](../README.md); package internals live in each
`internal/protocol/.../README.md`; config keys live in [config/README.md](../config/README.md).

Everything here is generic: red-switchboard only ever reads a creds file, and no Tesla password reaches
it. Mint Tesla tokens out of band with [`tesla_auth`](https://github.com/adriankumpf/tesla_auth).

## Contents

1. [Cost model: stream, don't poll](#1-cost-model-stream-dont-poll)
2. [Fleet API source (`tesla-fleet-poll-v1`)](#2-fleet-api-source-tesla-fleet-poll-v1)
3. [Fleet Telemetry streaming source (`tesla-fleet-stream-v1`)](#3-fleet-telemetry-streaming-source-tesla-fleet-stream-v1)
4. [Streaming sink (the cost win)](#4-streaming-sink-the-cost-win)
5. [Commands (gated write path)](#5-commands-gated-write-path)
6. [Poll cadence and timeouts](#6-poll-cadence-and-timeouts)
7. [Sources](#sources)

---

## 1. Cost model: stream, don't poll

The Tesla Fleet API is **pay-per-use**. Verify against Tesla's current
[billing and limits](https://developer.tesla.com/docs/fleet-api/billing-and-limits) page:

| Category | Per-unit | Notes |
|---|---|---|
| Streaming Signals (Fleet Telemetry) | ~$0.0000067/signal (~$0.0067/1k) | sent only on field change; a parked car streams ~nothing |
| Commands | $0.001 ($1/1k) | 30/min rate limit |
| Data (`vehicle_data` poll) | $0.002 ($2/1k) | the expensive one; migrate to telemetry |
| Wakes | $0.020 ($20/1k) | 3/min rate limit; frequent wakes are improper design |

Plus a **$10/month developer credit** and a **billing cap** (default `$0` = block on the first charge).

The rules that keep the bill low:

- **For a regularly-driven car, stream.** Fleet Telemetry is **~300x cheaper** per unit than
  `vehicle_data` and sends only on field change; Tesla's own case studies show **94-97% cost
  reduction** migrating polling to telemetry, and Tesla explicitly recommends it. Setup: section 3.
- **For a mostly-asleep car, slow polling is fine.** The cache makes idle slowness free downstream
  (consumers always read the cache fast). Never wake a sleeping car to poll: a wake is the priciest
  operation. The `poll_overrides` for the Tesla sources are tuned for this (section 6).
- **The flagship win is a non-Tesla source feeding the Tesla streaming sink** (section 4): a Rivian
  poll fills the cache, TeslaMate pulls live drive data over WSS, and **zero** Tesla API calls happen.
- **Never hammer.** The most expensive mistake is a loop: a consumer that ignores an error and
  retries for hours. red-switchboard precludes this structurally (section 2, quota-block backoff).

> ### The critical billing caveat: the cap wipes Fleet Telemetry config
> When the billing limit is exceeded, API usage is suspended **and** Fleet Telemetry configurations
> are **removed and not automatically restored**, even after the cap is raised or the cycle resets.
> Recovering requires **re-pairing** (section 3.4), not just a restart. red-switchboard surfaces this
> as the `redswitchboard_source_telemetry_config_wiped` metric and the `/status` `TelemetryConfigWiped`
> field. **Alert on it flipping to 1.**

---

## 2. Fleet API source (`tesla-fleet-poll-v1`)

red-switchboard reads a Tesla account over the Fleet API and decodes it into the canonical model; the
same protocol also serves the Fleet API surface back to consumers from the cache. Package internals:
`internal/protocol/tesla/fleet/poll/v1/README.md`.

### 2.1 Create the developer application

At [developer.tesla.com](https://developer.tesla.com):

1. Create an application. **Client Details** -> "Authorization Code and Machine-to-Machine" for most
   personal accounts; **M2M only** for business accounts that own vehicles directly.
2. Add **scopes**. `vehicle_data` requires the `vehicle_location` scope, which Tesla grants on request,
   not by default. Without it, `vehicle_data` returns no location and the canonical `Location` stays
   empty. Request `vehicle_location` at registration; do not discover it missing later.
3. Record the `client_id` / `client_secret`. red-switchboard does **not** use these at runtime: they
   belong to the token-minting tool below.

### 2.2 Mint the OAuth creds file

red-switchboard consumes an OAuth **access token** as `Authorization: Bearer <token>`, minted out of
band by [`tesla_auth`](https://github.com/adriankumpf/tesla_auth) (or any equivalent flow). The creds
file is plain JSON, stored verbatim as the token endpoint returns it:

```json
{
  "access_token": "<fleet access token>",
  "refresh_token": "<refresh token>",
  "expires_at": 1735689600,
  "client_id": "<your Fleet app client_id>"
}
```

- `access_token` is the short-lived bearer sent on every request; a missing/empty token fails fast at
  source construction.
- `refresh_token` is exchanged for a new access token (see auto-refresh below).
- `expires_at` (unix seconds) is optional; when present it drives proactive refresh (~75% of the token
  lifetime). Zero/absent means the lifetime is discovered on the first refresh.
- `client_id` is your Fleet app's OAuth client_id (legacy Owner API uses `ownerapi`). It (with
  `refresh_token`) is what enables auto-refresh; **omit it to keep the old behavior** (the token is
  served as-is and never refreshed, so you re-mint out of band before it lapses).
- `token_url` and `scope` are optional overrides (defaults: `https://auth.tesla.com/oauth2/v3/token`
  and `openid email offline_access`). A dev harness can point `token_url` at a mock endpoint.

Put the file where the source reads it (`sources.tesla-fleet-poll-v1.creds_file`, e.g. `/data/tesla.json`).

**Auto-refresh.** When `client_id` and `refresh_token` are present, red-switchboard refreshes the access
token centrally: one manager per creds file (shared by the Fleet poll source, Owner poll/stream, and the
command plugin) refreshes **proactively** before expiry and **reactively** on a 401, then rewrites the
creds file (0600) with the new access token, rotated refresh token, and updated `expires_at`. A revoked
refresh token (HTTP 400 `invalid_grant`) is terminal: refresh stops and the source surfaces
`NeedsReauth`, so you re-mint the creds file out of band.

### 2.3 Regional base URL

The Fleet API is regional. Set `base_url` to your region (default North America):

| Region | `base_url` |
|---|---|
| North America | `https://fleet-api.prd.na.vn.cloud.tesla.com` |
| Europe | `https://fleet-api.prd.eu.vn.cloud.tesla.com` |
| China | `https://fleet-api.prd.cn.vn.cloud.tesla.com` |

### 2.4 Config

```yaml
source: tesla-fleet-poll-v1
sink:   tesla-fleet-poll-v1          # or a streaming sink; see section 4
sources:
  tesla-fleet-poll-v1:
    creds_file: /data/tesla.json
    base_url: https://fleet-api.prd.na.vn.cloud.tesla.com
```

Pair it with the `poll_overrides` from section 6 (slow idle, never wake to poll).

### 2.5 Quota-block backoff (the anti-hammer guard)

On a 403 quota block ("account disabled: EXCEEDED_LIMIT"), red-switchboard applies a near-fixed
`poll.quota_block_floor` backoff (default `1h`), **not** exponential: the block is sustained, so an
exponential-from-`min_interval` cadence would hammer a dead API. A server `Retry-After` (which can be
hours) still wins. A 429 likewise honors `Retry-After` over the exponential backoff. The cache keeps
serving last-known data throughout. See `internal/poll/poll.go` and the source's `source.go`.

### 2.6 Verify

After `redswitchboard serve`:

- `GET /api/1/products` lists the account's vehicles (filtered to those with a `vehicle_id`).
- `GET /api/1/vehicles/{vin}/vehicle_data` returns a full snapshot from the canonical cache.
- `/status` and `/stats` show poll health; `redswitchboard_source_polls_total` rises per poll.

> The REST read surface is unauthenticated by default. To gate the live-data
> routes (so a compromised bridge co-tenant cannot read live GPS) set
> `sinks.tesla-fleet-poll-v1.auth_token`; it is opt-in and must be set on both
> red-switchboard and the consumer. See
> [DEPLOYMENT-NOTES.md](DEPLOYMENT-NOTES.md) for the TeslaMate side.

### 2.7 Rotating the OAuth client (leaked or compromised secret)

The `client_secret` is used only to mint tokens and register the partner (sections 2.2 and 3.1);
red-switchboard never uses it at runtime (the runtime auths with `client_id` + `refresh_token`, and
Tesla's refresh grant takes no secret). If it leaks, rotate it. Tesla's dashboard has no "regenerate
secret", so you **archive the application and create a new one**, which issues a new `client_id` and
forces a re-onboard. The EC keypair and the vehicle's virtual key are NOT affected by a secret leak, so
the pairing is kept (rotate the EC key only if the private key itself leaked).

Order it to contain first, then restore:

1. **Revoke the old app's account access** so active tokens die immediately:
   `https://auth.tesla.com/user/revoke/consent?revoke_client_id=<old_client_id>` (or the Tesla app,
   Security and Privacy, Third-Party Apps). Revoking also **removes the vehicle's
   `fleet_telemetry_config`**, so streaming stops until step 5, and the poll source surfaces
   `NeedsReauth` once the old refresh token is rejected.
2. **Archive the old app and create a new one** with the same app domain, redirect, and scopes
   (including `vehicle_location`). Record the new `client_id` / `client_secret`. Creating the new app
   before step 1 shortens the downtime window.
3. **Re-register the partner** under the new app (section 3.1: the `partner_accounts` call with a fresh
   M2M token). The published public-key URL is unchanged.
4. **Re-mint the creds file** with the new `client_id` (section 2.2): run the authorization-code flow,
   then write the new `access_token` / `refresh_token` / `client_id` into the creds file (0600).
5. **Re-push each vehicle's `fleet_telemetry_config`** (section 3.4) to restore streaming; this is also
   the moment to widen the field list. Wait for `synced=true`.
6. **Restart** red-switchboard and verify: `/status` shows `needs_reauth=false`, no `invalid_grant` in
   the log, and `redswitchboard_stream_source_connected` rises.

Finally, replace the secret in your password manager and, if it was ever committed, scrub it from git
history.

---

## 3. Fleet Telemetry streaming source (`tesla-fleet-stream-v1`)

Vehicles push telemetry to an mTLS WebSocket listener red-switchboard runs **in-process** (no
`fleet-telemetry` broker container, no Kafka, no CGO), decoded from protobuf into the cache. This is
the source-side cost win. Package internals: `internal/protocol/tesla/fleet/stream/v1/README.md`.

> Prerequisite: section 2 first. Fleet Telemetry uses the same developer application, OAuth token, and
> EC keypair as commands. The billing caveat (section 1) applies.

### 3.1 What the operator does (PKI + registration + pairing)

This distills Tesla's [fleet-telemetry](https://github.com/teslamotors/fleet-telemetry) 13-step flow:

1. **Generate an EC keypair** (`prime256v1`). You hold the private key; the public key is published.
   ```sh
   openssl ecparam -name prime256v1 -genkey -noout -out private-key.pem
   openssl ec -in private-key.pem -pubout -out public-key.pem
   ```
   The same `private-key.pem` is reused for commands (section 5).
2. **Host the public key** (PEM, verbatim, no HTML wrapper) over HTTPS at a stable URL on your app
   domain: `https://<application-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem`. Tesla
   fetches it to validate your key; a changed or missing key invalidates existing pairings.
3. **Mint a Partner Authentication Token (PAT)** (an app-scoped OAuth token, `openid` audience) for the
   registration + pairing calls. Short-lived; mint it fresh.
4. **Register the application** (`POST` the Fleet API `register` endpoint with the PAT, the public key,
   and the app domain). Registers your virtual key with Tesla.
5. **Pair the virtual key to each vehicle** (the "share a key" enrollment flow). Per-vehicle: repeat
   for every VIN. This adds your key to the vehicle's keychain so it accepts signed commands and
   pushes telemetry.
6. **Configure each vehicle's `fleet_telemetry_config`** (`POST` per VIN): the **field list** to
   stream (speed, odometer, soc, location, ... the fields `DecodePayload` maps), the **intervals**
   (per-field cadence and `minimum_delta`; capped at "every minute at the minimum", max **3 configs per
   vehicle**), the **listener `host:port`** (matching `stream.sources.tesla-fleet-stream-v1.host`/`.port`),
   and the **CA bundle** the vehicle uses to verify the listener's server cert. Then **wait for
   `synced=true`** per VIN; a vehicle reporting `synced=false` will not push.
7. **Validate the listener cert before pairing** with Tesla's `check_server_cert.sh` against the
   listener's public FQDN:port. A bad cert is the silent failure: vehicles refuse to connect and you
   see no traffic.

### 3.2 What red-switchboard handles in-process

Configure the in-process listener under `stream.sources.tesla-fleet-stream-v1`:

```yaml
stream:
  source: tesla-fleet-stream-v1
  sources:
    tesla-fleet-stream-v1:
      host: telemetry.example.com     # public FQDN vehicles dial (must match fleet_telemetry_config)
      port: 443
      server_cert: /data/telemetry.crt
      server_key:  /data/telemetry.key
      client_ca:   /data/tesla-vehicle-ca.pem   # CA that signed vehicle client certs (mTLS trust root)
      known_vins: ["5YJ3E1EA1KF000001"]         # optional extra deny-by-default allow-list
      idle_timeout: 60s                          # reap a connection that sent no frame this long
      max_conns: 32                              # global concurrent-connection cap (bounds a handshake flood)
      max_conns_per_ip: 4                        # per-remote-IP cap (one peer cannot take the whole budget)
```

red-switchboard then accepts mTLS WebSocket connections (`RequireAndVerifyClientCert` against
`client_ca`), decodes each protobuf `Payload` via the vendored `telemetry` + `protos` packages, merges
only the **present fields** into the per-vehicle cache, denies unknown VINs by default (the cache's
`KnownIDs()` is the trust boundary; `known_vins` is optional extra), and surfaces `synced` +
`TelemetryConfigWiped` in `/status` and the `redswitchboard_stream_source_*` metrics.

### 3.3 The delta-frame invariant

Fleet Telemetry sends fields **asynchronously, not as synchronized snapshots**: a lat/lng frame does
not arrive in lockstep with speed/SOC. The decode sets only the fields a frame carries and marks them
present; the cache merge copies only present fields and holds last-known for the rest. No
"complete-snapshot" assumption may creep into the decode. See `internal/cache/merge.go` and the
source's `source_mapping.go`.

### 3.4 Re-pairing runbook (after a cap-wipe)

When the cap is exceeded, Tesla removes the `fleet_telemetry_config` from affected vehicles and does
not restore it (section 1). To recover:

1. **Do not just restart.** Raising the cap and restarting does not restore the config; vehicles will
   not reconnect on their own.
2. **Re-`POST` the `fleet_telemetry_config`** for each affected VIN (field list, intervals, listener
   host, CA bundle), same as step 3.1.6.
3. **Wait for `synced=true`** again per VIN.
4. Confirm reconnect: `redswitchboard_stream_source_connected` rises and frames resume.

Alert on the `TelemetryConfigWiped` **transition** to 1, not just "is 1 on restart": a restart alone
does not clear the underlying condition.

### 3.5 Verify

- `redswitchboard_stream_source_connected` rises as vehicles connect.
- `redswitchboard_stream_source_frames_total` increments per decoded frame.
- The cache (`vehicle_data` on the REST sink, or a streaming consumer) reflects pushed telemetry at
  ~1/min-or-faster field cadence.

---

## 4. Streaming sink (the cost win)

red-switchboard serves a long-lived WebSocket to a consumer (stock TeslaMate) and pushes cached
canonical snapshots encoded as Tesla's legacy `data:update` wire shape, on cache change. The consumer
pulls live drive data over WSS and makes **zero** billable upstream calls, regardless of the source.
Package internals: `internal/protocol/tesla/stream/v1/README.md`.

There is exactly one streaming-sink implementation; it registers under **both** `tesla-fleet-stream-v1`
and `tesla-owner-stream-v1`, so it pairs with whichever streaming source is configured (and serves a
non-Tesla source, e.g. a Rivian poll, just as well: key the sink with either name).

### 4.1 Point the consumer at red-switchboard

For TeslaMate, set the WSS host and enable streaming per-car:

```env
TESLA_WSS_HOST=wss://<redswitchboard-host>:<port>
TESLA_WSS_USE_VIN=true        # required: red-switchboard tags consumers by VIN (the canonical id)
```

Then in TeslaMate **Settings** -> per-car, enable `use_streaming_api`. TeslaMate dials the WSS host
and runs the `data:subscribe_oauth` handshake. The port is the streaming sink's **own** listener
(`stream.sinks.<name>.listen_addr`), separate from the REST sink's internal port.

On connect the consumer sends a `data:subscribe_oauth` frame with a VIN (the tag) and an OAuth token.
red-switchboard resolves the VIN (or an int64 `vehicle_id` via `idmap`) to the canonical id, validates
it is known (an unknown tag gets `data:error` + close, so a rogue consumer cannot subscribe to a
vehicle red-switchboard does not serve), and treats the OAuth token as **cosmetic** (it serves from
the cache; it never calls Tesla on the consumer's behalf). After the handshake it pushes a
`data:update` frame per cache change, coalesced to `max_push_hz`; a `control:hello` keepalive holds
the connection open between changes.

### 4.2 Config (flagship: non-Tesla source -> Tesla streaming sink)

```yaml
source: rivian-graphql-poll-v1
sink:   tesla-fleet-poll-v1          # REST sink still served for sign-in / vehicle_data
stream:
  sink: tesla-fleet-stream-v1        # the WSS sink to TeslaMate
  asof_file: /data/asof.json         # persist the served-AsOf high-water mark so the monotonic clamp survives a restart (same /data volume as idmap_file)
  sinks:
    tesla-fleet-stream-v1:
      listen_addr: ":4001"           # consumer-facing WSS (its own listener)
      tls:                           # direct wss://; omit to serve ws:// behind a TLS-terminating proxy
        cert_file: /data/stream.crt
        key_file:  /data/stream.key
      max_push_hz: 1.0               # coalesce cache changes to 1 frame/sec per consumer
      max_consumers: 16
      keepalive_interval: 10s
```

This makes TeslaMate pull live Rivian drive data over WSS with **zero** Tesla API calls. A Tesla Fleet
Telemetry source -> this sink gives TeslaMate push-quality data with no polling on either side.

### 4.3 TLS

TeslaMate dials `wss://`, so the sink serves WSS two ways:

- **Direct `wss://`**: set `stream.sinks.<name>.tls.cert_file` / `.key_file` (a real cert for the
  consumer-facing FQDN). red-switchboard calls `ListenAndServeTLS` directly.
- **Behind a TLS-terminating proxy**: omit the `tls` block; red-switchboard serves `ws://` and the
  proxy terminates TLS. Keep the proxy's read timeouts generous (long-lived WS) and do not buffer.

The streaming sink's listener is **separate** from the internal REST sink (which stays internal
plain-HTTP); they run side by side in one binary.

### 4.4 Consumer-side protection and verify

The sink will not crash on a slow or misbehaving consumer: per-consumer write deadlines (a blocked
send past the deadline closes the connection), bounded send buffers (a slow consumer is dropped, never
blocks the cache writer or another consumer), and a `max_consumers` bound. So a stalled consumer
cannot take red-switchboard down (the "hammered for 11 hours" lesson). Verify:

- TeslaMate logs a successful `data:subscribe_oauth`; `redswitchboard_stream_sink_consumers` rises.
- On a cache change, `redswitchboard_stream_sink_frames_pushed_total` increments and the live map
  updates.
- `redswitchboard_stream_sink_frames_dropped_total` stays 0 in steady state (nonzero means a consumer
  is too slow; investigate the consumer).

---

## 5. Commands (gated write path)

The signed-command write path: red-switchboard signs and submits Vehicle Command Protocol messages
in-process via the vendored [`vehicle-command`](https://github.com/teslamotors/vehicle-command) SDK,
with **no `tesla-http-proxy` sidecar**. It exposes the same `POST /api/1/vehicles/{vin}/command/{cmd}`
surface the proxy serves, so a consumer (e.g. evcc) repoints onto red-switchboard with no URL changes.
Package internals: `internal/protocol/tesla/command/v1/README.md`.

### 5.1 The read-only guarantee (read first)

Commands are **gated off by default**. With `commands.enabled: false` (the default), the serve path
**never opens the commander and never mounts the command routes**: no write path exists in the binary.
This is structural, not optional. If you do not need to send commands, leave it unset and skip this
section.

### 5.2 Prerequisites

1. Complete section 2 (Fleet API). The commander reuses the **same** developer application, the
   **same** OAuth creds file (one token for reads and writes), and the **same** EC keypair.
2. Generate the EC keypair if you skipped section 3.1 (`openssl ecparam -name prime256v1 ...`). The
   **private key** is `commands.key_file`; guard it (anyone with it can command your vehicles).
3. Host the public key at `.well-known` (section 3.1.2; same URL).
4. Register the application and **enroll** the public key into each vehicle's keychain (Tesla's virtual-
   key flow). Per-vehicle: a vehicle without the enrolled key rejects signed commands. red-switchboard
   does not register or enroll; it only signs and submits once the key is enrolled.

### 5.3 Config

```yaml
commands:
  enabled: true
  plugin: tesla-command-v1          # the only shipped commander; may omit
  key_file: /data/tesla-command.pem # the EC private key
  creds_file: /data/tesla.json      # the SAME Fleet OAuth creds the source reads
  timeout: "30s"                    # per-command deadline
  # cache_file: /data/tesla-command-cache.json  # optional; reuse handshakes across restarts (fewer billable calls)
```

red-switchboard fails fast at startup if `key_file` or `creds_file` is missing/unreadable, so a
misconfigured command path is caught before serving.

### 5.4 Repoint consumers off `tesla-http-proxy`

The path is identical, so only the host changes:

- **Before:** consumer -> `tesla-http-proxy` container -> `POST /api/1/vehicles/{vin}/command/{cmd}`
- **After:** consumer -> red-switchboard (its REST listener, same host:port as the REST sink, not the
  streaming port) -> the same path.

After consumers are confirmed working, **remove the `tesla-http-proxy` container**: it is redundant,
and red-switchboard's in-process session cache holds handshakes for the process lifetime (fewer
billable handshake calls than a per-invocation proxy).

### 5.5 The command surface

Command names and params map 1:1 to `tesla-http-proxy` via `proxy.ExtractCommandAction`, so the
surface is identical. Examples: `charge_start`, `charge_stop`, `set_charge_limit {"charge_limit": 80}`,
`wake_up`, `auto_conditioning_start`/`_stop`, `door_lock`/`door_unlock`, `set_sentry_mode {"on": true}`.
See `proxy.ExtractCommandAction` for the full list. Outcomes:

- **Success** -> `200 {"response":{"result":true,"reason":""}}`.
- **Nominal failure** (vehicle rejected for a known reason, e.g. "already charging") ->
  `200 {"response":{"result":false,"reason":"..."}}`. A nominal failure is still 200 (matches the
  proxy's contract). Unknown commands / bad params land here too.
- **Infrastructure error** (auth, connect, signing, network) -> `502` with the Tesla error envelope;
  investigate `redswitchboard_command_infra_errors_total`.

### 5.6 Billing and rate limits

Commands and Wakes are billed separately from data polls (section 1): Commands $1/1k @ 30/min, Wakes
$20/1k @ 3/min. Tesla warns that **frequently waking vehicles is improper design**. red-switchboard
counts `wake_up` separately (`redswitchboard_command_wakes_total`), serializes commands per VIN (the
proxy's `lockVIN` discipline; VCSEC commands fail out of order), and surfaces
`redswitchboard_command_rate_limited_total` on a 429. Wake coalescing is a v1.x consideration; for now,
serialize per VIN and have consumers avoid redundant wakes. Other counters:
`redswitchboard_commands_total`, `_command_successes_total`, `_command_nominal_failures_total`.

### 5.7 Verify

```sh
curl -X POST https://<redswitchboard-host>/api/1/vehicles/<VIN>/command/charge_start
# -> {"response":{"result":true,"reason":""}}
```

Confirm the vehicle responds, the REST GET routes are unchanged (the command routes delegate every
non-command request to the sink), and with `commands.enabled: false` the command route 404s.

---

## 6. Poll cadence and timeouts

red-switchboard is a caching proxy: consumers read the sink every few seconds but hit only the
in-memory cache, never the source. So polling the source **slowly never makes consumers lag**, and the
bias is conservative: longer intervals, patient timeouts, jitter + a hard floor, and honoring a
vendor's `Retry-After` over any fixed backoff. Drive the live states (driving/charging) with streaming
where the vendor supports it rather than fast polling. All durations are Go duration strings.

### 6.1 Recommended per-API values

**Tesla Fleet API** (`tesla-fleet-poll-v1`): per-call billing + 429 risk make fast polling unviable.
Prefer Fleet Telemetry (section 3) for live data.

| Setting | Value | Rationale |
|---|---|---|
| HTTP timeout | `30s` | `vehicle_data` on a waking car is slow; avoid premature timeout + retry |
| Driving / charging | `streaming` (fallback `30s` / `60s`) | use telemetry; polling is interim only |
| Online (parked) | `5m` | rarely changes; change-adaptive backoff stretches further |
| Asleep | `30m` | never wake to poll (a wake is the priciest op) |
| `min_interval` | `10s` | hard floor above any realistic cadence |
| `max_backoff` | `30m` | patient ceiling on error/429 backoff |

**Tesla Owner API** (`tesla-owner-poll-v1`, legacy): aggressively rate-limited since mid-2024; an
observed 429 `Retry-After` has reached 16+ hours. **Honor `Retry-After` exactly; retrying sooner can
extend a lockout.** TeslaMate's own per-state cadence (driving 2.5s, charging 5s, online 60s) is the
upstream reference; red-switchboard goes slower for idle since the cache absorbs it.

| Setting | Value |
|---|---|
| HTTP timeout | `30s` |
| Driving / charging | `streaming` (fallback `5s` / `10s`) |
| Online (parked) | `2m` |
| Asleep | `30m` |
| `min_interval` | `5s` |
| `max_backoff` | `15m` (a real `Retry-After` wins) |

**Rivian GraphQL** (`rivian-graphql-poll-v1`, unofficial): Rivian has begun rate-limiting; the warning
sign is "No Cloud Connection" in the owner's real phone app, which degrades the real car. **Use a
separate secondary driver account** and poll gently. The defaults mirror the Home Assistant
integration's conservative cadence (driving 15s, charging 30s, online 30s, idle 15m).

### 6.2 Where these live in config

```yaml
poll:                          # the Rivian-tuned base cadence
  online: "30s"
  driving: "15s"
  charging: "30s"
  asleep: "15m"
  default: "30s"
  stale_after: "30m"           # degrade state to offline past this cache age (phantom-drive guard)
  min_interval: "5s"           # hard floor
  max_backoff: "15m"
  jitter_pct: 0.1              # +-10% so cars never poll in lockstep
  awake_backoff_cap: "5m"      # cap on how far a parked car's cadence stretches

poll_overrides:                # the Tesla sources diverge (slower idle, never wake)
  tesla-fleet-poll-v1:
    online: "5m"
    asleep: "30m"
    min_interval: "10s"
    driving: "30s"             # interim until Fleet Telemetry; prefer streaming
    charging: "60s"
  tesla-owner-poll-v1:
    online: "2m"
    asleep: "30m"
    driving: "5s"
    charging: "10s"
```

The poll loop also honors a server `Retry-After` / `RateLimit-Reset` over its own exponential backoff,
and the Tesla sources do a no-wake state check (the cheap summary endpoint) before fetching
`vehicle_data`, so polling never wakes a sleeping car. HTTP timeouts: global `http.timeout` (default
`30s`) with `http.retries` / `http.retry_wait` / `http.retry_max_wait` for transient retries; a
per-source `timeout` overrides it. Full key reference: [config/README.md](../config/README.md).

---

## Sources

- Tesla Fleet API billing and limits (pricing; cap wipes telemetry config): https://developer.tesla.com/docs/fleet-api/billing-and-limits
- Tesla Fleet API FAQ (429 = faulty app logic; rate-limit headers): https://developer.tesla.com/docs/fleet-api/support/faq
- Pay-per-use pricing breakdown (Data ~650/$1, Wakes ~65/$1, $10 credit): https://teslemetry.com/blog/tesla-fleet-api-pay-per-use
- Tesla fleet-telemetry (13-step flow, `fleet_telemetry_config`, `check_server_cert.sh`, 3 configs/vehicle): https://github.com/teslamotors/fleet-telemetry
- Tesla vehicle-command SDK (enrollment, `proxy.ExtractCommandAction`, command list): https://github.com/teslamotors/vehicle-command
- TeslaMate streaming + poll config (`TESLA_WSS_HOST`, `use_streaming_api`, intervals): https://docs.teslamate.org/docs/configuration/api/
- Owner API 429 / `Retry-After` lockouts (16+ hours observed): https://github.com/teslamate-org/teslamate/issues/3957
- `tesla_auth` token-minting tool: https://github.com/adriankumpf/tesla_auth
