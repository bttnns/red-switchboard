# redswitchboard

**redswitchboard is a hub. You pick a source side and a sink side from the available protocols, and
each protocol can be either side.** A SOURCE reads some vendor API and maps it into one neutral model;
a SINK serves that neutral model back in some external API's shape. `source:` and `sink:` in the
config pick the two sides independently, so the product is not "Rivian to Tesla", that is just one
configuration of many.

```
   any source protocol        redswitchboard          any sink protocol
   (a real vendor API)   -->   source -> canonical -> sink   (an impersonated API)
   reads live car data         one neutral model      serves it in another API's shape
```

Consumers run stock against the sink (nothing forked or patched), and they are always served from a
cache, so their frequent polling never reaches the upstream source.

> [!WARNING]
> Unaffiliated with Rivian and Tesla. Unofficial, unsupported, use at your own risk. These vendor APIs
> are undocumented and can change at any time, which may break this without notice.

## Protocols

Each protocol registers a source side and a sink side; pick one for each. The name says it all:
`<vendor>-<api>-<transport>-v1`, where transport is `poll` (REST request/response) or `stream`
(WebSocket push). Polling and streaming are independent on each side, so you can **poll one side and
stream the other** (e.g. poll Rivian, stream to a Tesla Fleet API consumer).

| Protocol | Vendor / API | As a source | As a sink |
|---|---|---|---|
| [`rivian-graphql-poll-v1`](internal/protocol/rivian/graphql/poll/v1/README.md) | Rivian, GraphQL | reads live Rivian data | serves the Rivian GraphQL shape |
| [`tesla-fleet-poll-v1`](internal/protocol/tesla/fleet/poll/v1/README.md) | Tesla Fleet API | reads a Tesla via the Fleet API | serves the Tesla Fleet `vehicle_data` shape |
| [`tesla-owner-poll-v1`](internal/protocol/tesla/owner/poll/v1/README.md) | Tesla Owner API (legacy) | reads a Tesla via the Owner API | serves the legacy Owner API `vehicle_data` shape |
| [`tesla-fleet-stream-v1`](internal/protocol/tesla/fleet/stream/v1/README.md) | Tesla Fleet Telemetry | consumes pushed telemetry (mTLS, ~300x cheaper) | pushes `data:update` over WSS to the consumer |
| [`tesla-owner-stream-v1`](internal/protocol/tesla/owner/stream/v1/README.md) | Tesla Owner streaming | dials the legacy Owner stream | pushes `data:update` over WSS to the consumer |
| [`teslafi-csv-v1`](internal/protocol/teslafi/csv/v1/README.md) | TeslaFi CSV export | replays recorded TeslaFi CSV files | writes TeslaFi CSV for the consumer's importer |

The `stream` source and sink are configured under a separate `stream:` block and are purely additive:
with no `stream:` set, serving is the poll-only path. The streaming setup, cost model, and commands
(a gated write path) are covered in [docs/SETUP.md](docs/SETUP.md).

## Use cases

### Log a Rivian in any Tesla Fleet API consumer (flagship, validated path)

Source `rivian-graphql-poll-v1`, sink `tesla-fleet-poll-v1`, consumed by a stock
[Tesla Fleet API consumer](https://github.com/teslamate-org/teslamate). Your Rivian's live data is
translated into the Tesla Fleet `vehicle_data` shape in real time, and the consumer logs it as if it
were a Tesla: full drive/charge history, trips, geofences, maps, and every Grafana dashboard.

Any other Tesla Fleet API consumer works the same way: redswitchboard fills the third-party provider
slot of Teslemetry, Tessie, and any tool
with a custom Fleet API base URL (e.g. [TeslaLogger](https://github.com/bassmaster187/TeslaLogger)).

### Buffer and rate-limit a metered vehicle API (don't get charged a fortune)

The Tesla Fleet API is pay-per-use, and the costly mistake is a consumer that polls hard (or loops on
an error) and runs up the bill. redswitchboard sits in front as a buffer: it polls the source on a
gentle, change-adaptive cadence and serves every consumer read from the cache, so **no consumer
request ever reaches Tesla**. A consumer hammering `vehicle_data` every two seconds costs nothing
extra. It also classifies quota/rate-limit errors and backs off (honoring `Retry-After`, which can be
hours) instead of retrying into a runaway bill. Cost model and billing-cap caveats: [docs/SETUP.md](docs/SETUP.md).

### Translate polling to streaming (or streaming to polling), on either side

Because poll and stream are independent per side, redswitchboard converts between them through the
canonical cache:

- **Poll source -> stream sink** (the headline cost win): poll a Rivian (free), and serve the consumer
  a live `data:update` WebSocket from the cache. The consumer gets push-quality drive data over WSS
  while making **zero** Tesla API calls.
- **Stream source -> poll sink**: consume Tesla Fleet Telemetry (pushed, ~300x cheaper than
  `vehicle_data` polling) and serve a stock REST `vehicle_data` API from it, so a poll-only consumer
  rides on cheap streamed data with no polling upstream.

Both directions are just a `source:` / `stream.sink:` (or `stream.source:` / `sink:`) pairing in the
config. Setup: [docs/SETUP.md](docs/SETUP.md).

### Owner API stop-gap

Tesla is deprecating the Owner API, but plenty of existing services still speak it. Run
redswitchboard with Fleet as the source and Owner as the sink, and your legacy app keeps working
unchanged:

```yaml
source: tesla-fleet-poll-v1   # read live data from Tesla's new Fleet API
sink:   tesla-owner-poll-v1   # serve that data back in the old Owner API shape
```

Point your existing Owner-API app's base URL at redswitchboard. It sees the familiar shape, now
backed by live Fleet data.

### Backfill a consumer from a TeslaFi export

If you have existing TeslaFi history, `teslafi-csv-v1` bridges the import:

```yaml
source: teslafi-csv-v1   # read the recorded CSV files
sink:   teslafi-csv-v1   # write TeslaFi CSV for the consumer's native importer
```

The sink writes monthly `TeslaFi<M><YYYY>.csv` files to `export_dir`; point the consumer's `IMPORT_DIR`
at that path and run the built-in TeslaFi importer. You can also drive it with any live source to
produce a fresh TeslaFi export as you go. Details: [teslafi-csv-v1 README](internal/protocol/teslafi/csv/v1/README.md).

## Other things you can do

- **More than one car**: one source login serves every vehicle on the account, each logged
  independently.
- **Develop with no car**: run the whole stack against the built-in protocol-parameterized mock. See
  [LOCAL-DEV.md](LOCAL-DEV.md).

## How it works

`redswitchboard serve` polls the source on a gentle cadence and caches the neutral snapshot. The
sink answers requests from that cache, so the consumer's frequent polling never reaches the source.
It converts units and holds last-known values on bad readings so a junk sensor value never corrupts
an in-progress drive or charge. No request from a consumer ever reaches the upstream vendor's
servers. Details: [ARCHITECTURE.md](ARCHITECTURE.md).

## Commands

`redswitchboard` is a single binary:

- **Run:** `redswitchboard serve` runs the pipeline; `redswitchboard mock --protocol <name>` runs a
  fake upstream speaking any protocol (with `mock fleet-push` / `mock owner-stream` doubles for the
  streaming paths). See [LOCAL-DEV.md](LOCAL-DEV.md).
- **Monitor:** `redswitchboard status` / `stats` / `cache` query a running server (see
  [Observability](#observability)).
- **Preview:** `redswitchboard show <protocol>` renders any protocol's API shape without a full
  stack (see [Preview protocol output](#preview-protocol-output)).
- **Consumer:** `redswitchboard teslamate auth` writes the consumer's token row so it polls
  redswitchboard with no browser sign-in; `teslamate check` asserts an expected state landed in its DB.
- **Discover:** `redswitchboard sources` / `sinks` / `config print` / `version`.

Logging in to a vendor is intentionally NOT part of this binary: source credentials are minted by
separate, single-purpose tools and redswitchboard only reads the creds file. See
[Credentials](#credentials). (`teslamate auth` is the exception only in that it writes the *consumer's*
token, not a vendor credential.)

## Credentials

redswitchboard reads a creds file per source; it never logs in itself. Mint the creds with the
matching tool, then point the source at the file:

- **Rivian:** [`rivian_auth`](https://github.com/bttnns/rivian_auth) (CSRF, password login, MFA).
- **Tesla:** [`tesla_auth`](https://github.com/adriankumpf/tesla_auth) (OAuth sign-in), for either
  Tesla source.

At runtime a source refreshes only its short-lived session tokens (Rivian: fresh CSRF + app-session
token; Tesla: OAuth access token via the stored refresh token). A fully revoked session needs a new
creds file. No password ever reaches redswitchboard.

## Quick start: Rivian to Tesla Fleet API with Docker

The validated path: source `rivian-graphql-poll-v1`, sink `tesla-fleet-poll-v1`, a stock consumer + Grafana.
No-car dev: [LOCAL-DEV.md](LOCAL-DEV.md). For Podman or Apple `container` runtime variants, see
[examples/rivian-to-teslamate/README.md](examples/rivian-to-teslamate/README.md).

**1. Configure.**

```sh
cp .env.example .env
$EDITOR .env   # set ENCRYPTION_KEY, TM_DB_PASS, GRAFANA_PASS, and matching TOKEN / REDSWITCHBOARD_TOKEN
```

The `TOKEN` and `REDSWITCHBOARD_TOKEN` values must match; default `local` works for a private stack.
See [config/README.md](config/README.md) for all configuration options.

**2. Log into Rivian.** [`rivian_auth`](https://github.com/bttnns/rivian_auth) mints the creds file;
your password never touches redswitchboard. Most accounts need MFA (two steps):

```sh
go install github.com/bttnns/rivian_auth@latest

# step 1: send the one-time code
rivian_auth login --username you@example.com --password 'your_password' \
    --out ./data/rivian/rivian.json

# step 2: complete with the code (email or SMS)
rivian_auth login --username you@example.com --password 'your_password' --otp 123456 \
    --out ./data/rivian/rivian.json
```

Non-MFA accounts finish in one step. Re-run if the session is ever revoked; redswitchboard refreshes
short-lived tokens itself at runtime.

**3. Bring up the stack.**

```sh
docker compose -f examples/rivian-to-teslamate/compose.yaml up -d
docker compose -f examples/rivian-to-teslamate/compose.yaml logs -f redswitchboard teslamate
```

**4. Sign in.** Open the consumer at <http://localhost:4000> and click **Sign in** (one click; the
token refresh is redirected to redswitchboard). Repeat on every restart.

**5. Turn streaming off.** In the consumer, **Settings** -> your car -> **streaming API** off. Mandatory:
redswitchboard is poll-based, and with streaming on the consumer hangs on a WebSocket it does not serve.

**6. Watch the data.** Drives, charges, and positions log immediately; Grafana is at
<http://localhost:3000>. Efficiency and Battery Health dashboards need several charge cycles; to fill
history fast with no car, use the [dev stack](LOCAL-DEV.md).

## Observability

`serve` exposes three surfaces for monitoring what the hub is doing.

### Prometheus `/metrics`

A Prometheus text endpoint at `http://<listen_addr>/metrics` (default `http://localhost:4000/metrics`),
with no extra dependency. Only `serve` exposes it; `mock` does not.

**Sink HTTP** (per `{method, route}`, paths normalized with `{id}` for numeric segments):
- `redswitchboard_http_requests_total{method,route,status}`
- `redswitchboard_http_request_duration_seconds_{sum,count,max}{method,route}`

**Source polls** (per `{source, vin}`):
- Counters: `redswitchboard_source_polls_total`, `_poll_errors_total`, `_poll_changes_total`,
  `_rate_limited_total`
- Gauges: `_poll_backoff_seconds`, `_consecutive_failures`, `_needs_reauth`

**Runtime:**
- `redswitchboard_goroutines`, `_heap_alloc_bytes`, `_uptime_seconds`, `_vehicles_known`

Honest limits: latency is exposed as sum/count/max only (no histogram buckets, so no true p95/p99);
logs are unstructured.

### CLI: `status`, `stats`, `cache`

Read-only commands that query a running server over HTTP. No source client, no creds needed. Run from
another terminal while `serve` is up, or exec into the container:

```sh
docker compose -f examples/rivian-to-teslamate/compose.yaml exec redswitchboard \
    redswitchboard status
```

```sh
redswitchboard status [--addr localhost:4000] [--format table|json] [--watch]
redswitchboard stats  [--addr localhost:4000] [--format table|json] [--watch]
redswitchboard cache show [--addr localhost:4000] [--id <id>]   # served snapshot (vehicle_data)
redswitchboard cache raw  [--addr localhost:4000] [--id <id>]   # raw source-native extras
```

- **`status`**: per vehicle: name, VIN, state, cache age, stale flag, poll counts, last error.
- **`stats`**: uptime, vehicle count, source poll/error/rate-limit counts, how many polls saw new
  data, consumer reads served, derived `reads_per_poll` / `reads_per_change` / `change_ratio`, and
  memory. Shows why we poll slowly: data changes far less often than consumers read.
- **`cache show`**: the exact `vehicle_data` payload your consumer receives.
- **`cache raw`**: the source-native extras JSON (fields the source has that have no sink equivalent).

`--watch` re-polls on a configurable interval.

### Logs

Per-request lines (method, path, status, duration), poll-loop lifecycle and errors, and resty
source-client debug when enabled.

## Preview protocol output

`redswitchboard show <protocol>` prints the exact wire response a consumer would receive for that
protocol, without needing a full running stack. Useful for debugging mappings or verifying a new
protocol's output before wiring it up.

Data source precedence:

1. `--server <host:port>`: fetch the live wire from a running server.
2. `--creds <file>`: open the real source API, decode its data, re-render through the sink.
3. Default: render synthetic mock data (no creds or server needed).

```sh
redswitchboard show tesla-fleet-poll-v1                        # mock data, no creds needed
redswitchboard show tesla-owner-poll-v1 --scenario driving     # mock, driving scenario
redswitchboard show rivian-graphql-poll-v1 --creds ./data/rivian/rivian.json   # real source data
redswitchboard show tesla-fleet-poll-v1 --server localhost:4000                # live from a server
```

Flags: `--server <addr>`, `--creds <file>`, `--vehicle <canonical-id>`,
`--scenario idle|driving|charging`. A one-line note on stderr reports which mode was used.

## Configuration

Full reference with all keys, defaults, and notes: [config/README.md](config/README.md).
The annotated example file is at [config/redswitchboard.yaml](config/redswitchboard.yaml).

## More documentation

- [docs/SETUP.md](docs/SETUP.md): Tesla sources, streaming, commands, the cost model, and poll cadence.
- [ARCHITECTURE.md](ARCHITECTURE.md): the hub design and the non-obvious decisions.
- [LOCAL-DEV.md](LOCAL-DEV.md): run the whole stack with no car, via the boundary fake.
- [MAPPING.md](MAPPING.md): the field-by-field mapping against the canonical model.
- [FAQ.md](FAQ.md): common questions and gotchas.
- [docs/RIVIAN-API.md](docs/RIVIAN-API.md): the undocumented Rivian GraphQL API as we use it.
- [examples/rivian-to-teslamate/README.md](examples/rivian-to-teslamate/README.md): the
  cross-runtime runbook (Docker, Podman, Apple `container`) and teardown.
- [config/README.md](config/README.md): the full configuration reference.

## Security

- **Use a secondary Rivian account** and share your vehicle with it, to limit exposure.
- **The creds file is `0600`**; keep it that way, and never commit it, `.env`, or any secrets.
