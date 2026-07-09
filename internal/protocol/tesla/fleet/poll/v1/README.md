# tesla-fleet-poll-v1

The Tesla Fleet API protocol package (`internal/protocol/tesla/fleet/poll/v1`),
registered as both a source and a sink. As a sink it serves the Tesla Fleet API
shape stock TeslaMate (and any Fleet API consumer) reads, encoding from the
canonical model; as a source it reads a Tesla via the Fleet API and decodes into
canonical. This is the reference Tesla shape; the Owner protocol reuses its
wire/decode/encode.

The sink reads the **latest cache** through `sink.Provider` (never the upstream),
resolves the canonical id to a stable integer via `internal/plugin/sink/idmap`, and
encodes. The previous encoded payload is retained so a single bad snapshot holds
last-known rather than emitting a broken one.

## Key files

- `sink.go` / `adapter.go`: the sink plugin and the cache -> Fleet adapter.
- `sink_mapping.go`: canonical -> `vehicle_data` encode (see
  [MAPPING-README.md](MAPPING-README.md)).
- `server.go`: the chi HTTP surface and the `qts-` token handshake. Per-request
  logging is failure-only; the request count lives in `/stats` and the
  `redswitchboard_http_requests_total` metric (a hammering consumer once logged
  ~7.5M success lines and tripped journald rate-limiting).
- `source.go` / `source_mapping.go` / `source_creds.go`: the Fleet API source.
- `wire/`: the Tesla wire structs.

## Numeric charger_power invariant

`charge_state.charger_power` is emitted as a whole, non-null number (0 when zero
or no live session), never blank. TeslaMate casts it to a DB `:integer`, and a
fractional AC value (e.g. 7.7 kW) fails that cast with "Invalid charge data:
charger_power is invalid", dropping the charge row and fragmenting the session, so
`sink_mapping.go` rounds `live.PowerKw` to match the real Tesla API's integer kW.

## Spinning-consumer fast path

A `vehicle_data` read re-encodes all five sub-objects from the latest snapshot.
When a consumer reads at an unchanged `AsOf` (the served snapshot timestamp has
not advanced since the last translation), the encoded body cannot have changed,
so the adapter serves the cached payload instead of re-encoding. This bounds the
cost of a stale-fetch spin (a consumer once hit `vehicle_data` at ~250 req/s on a
frozen `AsOf`) to an `AsOf` compare per read, with exactly one cached payload per
vehicle. The `unchanged_reads` counter in `/stats` makes the spin observable.
Staleness degradation still runs on the cheap path (it keys on the wall clock,
not `AsOf`), and a zero (un-merged) `AsOf` is never cached, so a pre-merge
vehicle never freezes on its first body.

## Staleness degradation

`staleAfter` (from `poll.stale_after`) is the cache age past which the top-level
state degrades to `offline`, so a source outage mid-drive cannot produce a phantom
infinite drive (it parks the car instead).

The sink also exposes `/status`, `/stats`, and `/api/1/vehicles/{id}/source_extras`,
which power the inspect CLI; `source_extras` surfaces the raw source-only fields
that have no Tesla equivalent.

## Read-surface auth (P14, default-off)

Set `sinks.tesla-fleet-poll-v1.auth_token` to require `Authorization: Bearer
<token>` on the live-data read routes (`/api/1/products`, `/api/1/vehicles`,
`/api/1/vehicles/{id}[/vehicle_data|/source_extras]`, `/status`, `/stats`), so a
compromised bridge co-tenant cannot read live GPS. The compare is constant-time
(`crypto/subtle`). UNSET (the default) leaves the surface open, exactly as before,
so this is purely opt-in. The `/api/oauth2/v3/token` bootstrap stays open (it is
how a consumer obtains the bearer) and, when a token is set, returns it as the
`access_token`. The token must be set on BOTH this sink AND the consumer; enabling
it on one side only breaks polling. See `docs/DEPLOYMENT-NOTES.md` for the
TeslaMate side.
