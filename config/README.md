# config

The `redswitchboard` YAML configuration files. `source:` / `sink:` pick the two
sides of the hub (each is a protocol that can be either side); per-protocol
sub-blocks live under `sources:` / `sinks:`, and the `poll:` / `http:` / `server:`
blocks tune the cache, the outbound client, and the inbound server. The binary
reads one file on `serve`; the loader and the per-field defaults live in
`internal/config`.

## Files

- `redswitchboard.yaml`: the **example / production** config. Every field is
  documented inline (meaning, units, default), so this doubles as the config
  reference. Mounted read-only at `/config/redswitchboard.yaml` (see
  `examples/rivian-to-teslamate/compose.yaml`).
- `dev/redswitchboard.yaml`: the **dev** variant, used by `examples/rivian-to-teslamate/compose.dev.yaml`.
- `dev/rivian.creds`: the static, checked-in dev creds (mock tokens) whose vehicle
  GUID matches `mock`'s default `--vehicles`.

## prod vs dev: it's `base_url`

For the flagship dev stack (Rivian source), the one load-bearing difference is
`sources.rivian-graphql-poll-v1.base_url`:

- **prod** points at the real Rivian cloud (`https://rivian.com/api/gql`) and
  needs a real creds file from [`rivian_auth`](https://github.com/bttnns/rivian_auth).
- **dev** points at the `mock` fake (`http://mock:5050/api/gql`) and
  uses the checked-in mock creds, so the whole stack runs with no car and no
  Rivian login. The dev poll intervals are also shortened so scenario changes show
  up quickly.

Every field has a built-in default, so a partial file is always safe (omitted
fields keep their default). Secrets (the provider token) stay in `.env`, not here.

## Reference

`config/redswitchboard.yaml` is the annotated authoritative source for all keys and
their defaults. The tables below are a convenience snapshot and may lag the code;
when in doubt, check the YAML file. All keys are optional; a partial file is safe.
Durations are Go duration strings (`5s`, `2m`).

### Top-level and per-protocol keys

| Key | Default | Notes |
|---|---|---|
| `listen_addr` | `:4000` | Sink API listen address (in-network only; keep `:4000` for the Tesla sinks). |
| `source` | `rivian-graphql-poll-v1` | Active input source plugin. |
| `sink` | `tesla-fleet-poll-v1` | Active output sink plugin. |
| `sources.rivian-graphql-poll-v1.creds_file` | `/data/rivian.json` | Path to the creds file minted by [rivian_auth](https://github.com/bttnns/rivian_auth) (mode 0600). |
| `sources.rivian-graphql-poll-v1.base_url` | `https://rivian.com/api/gql` | Rivian API root; point at the mock for dev. |
| `sources.rivian-graphql-poll-v1.car_type` | `model3` | **Cosmetic** Tesla code a Tesla Fleet API consumer requires; does not make the car a Tesla. Valid: `model3`/`models`/`modelx`/`modely`/`cybertruck`. |
| `sources.rivian-graphql-poll-v1.model` | `R1T` | The consumer's `trim_badging`; `R1T`/`R1S`/`R2`/`R3`. Auto-derived when unset. |
| `sources.rivian-graphql-poll-v1.display_name` | (unset) | Name override; else the Rivian account name, then `Rivian`. |
| `sources.rivian-graphql-poll-v1.timeout` | `30s` | Per-request Rivian HTTP timeout. |
| `sources.rivian-graphql-poll-v1.vehicles` | (unset) | Per-VIN overrides (`car_type`/`model`/`display_name`) for a mixed fleet. |
| `sinks.tesla-fleet-poll-v1.provider_token` | `local` | Token the consumer sends as `?token=...`; must match `.env`. |
| `sinks.tesla-fleet-poll-v1.idmap_file` | (unset) | Optional path to persist the id -> integer id map (else in-memory). |
| `sinks.tesla-fleet-poll-v1.auth_token` | (unset) | Optional static bearer gating the live-data read routes (`/api/1/products`, `/api/1/vehicles`, `/api/1/vehicles/{id}[/vehicle_data\|/source_extras]`, `/status`, `/stats`). UNSET (default) = unauthenticated, exactly as before. When set, consumers must send `Authorization: Bearer <auth_token>`; the `/api/oauth2/v3/token` bootstrap stays open and returns this value as the `access_token`. Must be set on BOTH this sink AND the consumer; enabling it on one side only breaks polling. Secret: never log it. See `docs/DEPLOYMENT-NOTES.md`. |

### `poll` block

Controls how often redswitchboard refreshes its cached snapshot per vehicle state.
Consumers are always served from cache, so slow polling never makes them lag. The
loop adds +-10% jitter, a 5s floor, exponential backoff on errors (cap ~15m), and
stretches idle parked cars further (capped at `awake_backoff_cap`).

| Key | Default | State / meaning |
|---|---|---|
| `poll.online` | `30s` | awake, parked |
| `poll.driving` | `60s` | shift in D/R/N (fast fallback; the stream fills live position between polls) |
| `poll.driving_streaming` | `2m` | drive cadence while a telemetry stream is fresh; snaps back to `driving` on a stall. `0` disables (needs a stream cache) |
| `poll.stream_fresh_within` | `60s` | how recently a stream frame must have arrived for the drive backoff to engage |
| `poll.charging` | `30s` | active charge (AC / default) |
| `poll.charging_dc` | `60s` | active charge while DC fast charging (SOC moves fast); `0` collapses to `charging` |
| `poll.dc_threshold_kw` | `25` | delivered power (kW) at/above which a charge counts as DC when the source gives no fast-charger flag |
| `poll.asleep` | `15m` | asleep |
| `poll.default` | `30s` | unclassified |
| `poll.stale_after` | `30m` | cache age past which state degrades to `offline` (phantom-drive guard; `0` disables) |
| `poll.min_interval` | `5s` | hard floor on cadence and minimum error backoff |
| `poll.max_backoff` | `15m` | cap on exponential backoff after repeated errors / rate limits |
| `poll.jitter_pct` | `0.1` | +-randomization per scheduled poll (0.1 = +-10%) so cars never poll in lockstep |
| `poll.awake_backoff_cap` | `5m` | cap on how far an awake-but-parked car's cadence stretches under change-adaptive backoff (driving/charging never stretch) |
| `poll.quota_block_floor` | `1h` | near-fixed backoff on a billing/quota block (403 account disabled); NOT exponential. A server Retry-After still wins |

### `http` block

The outbound source HTTP client, applied process-wide at startup. A per-source
`timeout` (under `sources.<name>`) overrides `http.timeout`.

| Key | Default | Notes |
|---|---|---|
| `http.timeout` | `30s` | per-request backstop when a source sets no timeout |
| `http.retries` | `2` | automatic retries on transient failures (network errors, 429/5xx) |
| `http.retry_wait` | `500ms` | base wait between retries (grows with backoff) |
| `http.retry_max_wait` | `5s` | cap on the retry wait |

### `server` block

The inbound sink HTTP server.

| Key | Default | Notes |
|---|---|---|
| `server.read_header_timeout` | `10s` | bound on waiting for request headers (Slowloris guard) |
| `server.shutdown_timeout` | `5s` | bound on graceful shutdown on SIGINT/SIGTERM |

### `metrics` block (optional)

Tunes the `/metrics` surface. Purely additive; the default matches Tesla's published
price so it can be omitted.

| Key | Default | Notes |
|---|---|---|
| `metrics.vehicle_data_price_usd` | `0.002` | Per-call USD price of a billed Tesla `vehicle_data` fetch, the cost-estimate multiplier. Drives `redswitchboard_source_estimated_cost_usd_total` (sum of paid fetches x price; `rate()` gives the $/day burn) and `redswitchboard_source_vehicle_data_price_usd`. Set to your account/cloud rate if it differs (see docs/SETUP.md cost model). `<=0` disables the cost series. |

### `stream` block (optional)

The streaming path is purely additive: when both `stream.source` and `stream.sink`
are empty (the default), serving is the polling-only path unchanged. Set `stream.sink`
to `tesla-fleet-stream-v1` to serve the consumer's `data:update` WebSocket from the
cache (the cost win: consumers pull drive data over WSS instead of polling Tesla).
Set `stream.source` to a streaming source (Fleet Telemetry, Phase 2) to fill the
cache from pushed telemetry. The streaming sink owns its own listener
(consumer-facing `wss://`) separate from the internal REST sink.

| Key | Default | Notes |
|---|---|---|
| `stream.source` | (unset) | Active streaming source plugin (`tesla-fleet-stream-v1` for Fleet Telemetry, `tesla-owner-stream-v1` for Owner streaming). |
| `stream.sink` | (unset) | Active streaming sink plugin (`tesla-fleet-stream-v1`). |
| `stream.asof_file` | (unset) | Persist the last-served AsOf high-water mark per vehicle so the merge cache's monotonic AsOf clamp survives a restart (prevents the consumer's stale-fetch storm a backwards timestamp triggers). Same `/data` volume as `idmap_file`; empty = in-memory only. |
| `stream.replay_file` | (unset) | Bounded on-disk ring of recent merged snapshots per vehicle so a TRS or consumer restart mid-drive does not drop in-flight frames: on reconnect the ring is replayed to the consumer in order. Complements `asof_file` (which restores the AsOf clamp; this restores the recent telemetry history). Same `/data` volume as `idmap_file`; empty = disabled (zero overhead). |
| `stream.replay_depth` | `256` | Per-vehicle replay ring bound (oldest evicted past it), capping on-disk growth. Ignored when `replay_file` is empty. Default covers a few minutes of 1 Hz drive frames. |
| `stream.sources.<name>.host` / `.port` / `.path` | `0.0.0.0` / `443` / `/` | Fleet Telemetry mTLS listener bind address + WS path vehicles dial. |
| `stream.sources.<name>.server_cert` / `.server_key` | (required) | mTLS server cert/key for the public FQDN vehicles dial. |
| `stream.sources.<name>.client_ca` | (required) | CA bundle that signed the vehicle client certs (mTLS trust root). |
| `stream.sources.<name>.known_vins` | (unset) | Optional extra deny-by-default VIN allow-list (the cache also denies unknown ids). |
| `stream.sources.<name>.idle_timeout` | `60s` | Reap a connection that has sent no frame for this long. |
| `stream.sources.<name>.max_conns` | `32` | Global concurrent-connection cap on the public mTLS listener (bounds a pre-auth handshake flood; rejected before TLS). |
| `stream.sources.<name>.max_conns_per_ip` | `4` | Per-remote-IP concurrent-connection cap, so one peer cannot consume the whole `max_conns` budget. `<=0` disables it. |
| `stream.sinks.<name>.listen_addr` | `:4001` | Consumer-facing WSS listen address (its own listener). |
| `stream.sinks.<name>.max_push_hz` | `1.0` | Per-consumer push cap; cache changes coalesce to this many frames/sec. |
| `stream.sinks.<name>.max_consumers` | `16` | Bound on concurrent consumer connections; beyond it a new one is rejected. |
| `stream.sinks.<name>.tls.cert_file` / `.key_file` | (unset) | Direct `wss://`; omit to serve `ws://` behind a TLS-terminating proxy. |
| `stream.sinks.<name>.idmap_file` | (unset) | Persist the int64 vehicle_id <-> canonical id map (else in-memory, still deterministic). |

### `commands` block (optional)

The signed-command write path (Phase 4). Gated OFF by default: when `enabled` is
false (the default), the command REST routes are not mounted and no write path
exists in the binary (the read-only guarantee). When enabled, red-switchboard
signs and submits Vehicle Command Protocol messages in-process via the vendored
`vehicle-command` SDK (no `tesla-http-proxy` sidecar), exposing the same
`POST /api/1/vehicles/{vin}/command/{cmd}` surface the proxy serves.

| Key | Default | Notes |
|---|---|---|
| `commands.enabled` | `false` | Off-by-default is the read-only guarantee; the command routes 404 when false. |
| `commands.plugin` | `tesla-command-v1` | Commander plugin name; the shipped Tesla plugin is the only one. |
| `commands.key_file` | (required when enabled) | EC private key (`prime256v1`) for Vehicle Command Protocol signing; its public key is hosted at `.well-known` and enrolled in each vehicle's keychain. |
| `commands.creds_file` | (required when enabled) | Fleet OAuth creds file (the SAME file the source reads; one token for reads AND writes). When the file carries a `client_id` + `refresh_token`, the access token auto-refreshes centrally (see [docs/SETUP.md](../docs/SETUP.md#22-mint-the-oauth-creds-file)). |
| `commands.cache_file` | (unset) | Optional session-cache path; a restart reuses cached handshakes (fewer billable calls). |
| `commands.timeout` | `30s` | Per-command deadline (context timeout around the SDK call). |
