# Architecture

How redswitchboard is built and why. Operational questions: [FAQ.md](FAQ.md). Rivian wire protocol:
[docs/RIVIAN-API.md](docs/RIVIAN-API.md).

redswitchboard is a **hub**: you pick a source protocol and a sink protocol independently, and each
protocol can be either side. A source reads a vendor API and maps it INTO one neutral model; a sink
maps OUT OF that model into some external API's shape. Consumers run stock against the sink (e.g. a
stock Tesla Fleet API consumer pointed at the Tesla Fleet sink); nothing in the consumer is forked or patched.

## A hub, not a pipe

The earlier design hard-wired Rivian straight into Tesla. Now every protocol maps to and from one
canonical model, never vendor-to-vendor, so adding a make is **N+M** mappers (one source map and one
sink map per protocol) instead of N×M direct translators. Each registered protocol is both a source
and a sink; the current set is listed in [README.md](README.md). Valid pairings include Rivian ->
Fleet, Fleet -> Owner (the Owner-API stop-gap), any live source -> teslafi-csv-v1 (backfill the consumer
from a recorded export), and so on.

```
  [ source side ]              [ canonical model ]          [ sink side ]
   any protocol's source  --map-->  internal/vehicle  --map-->  any protocol's sink
   (vendor API in)               (neutral EV state)          (impersonated API out)
              \__ poll cache (protocol-agnostic) __/
```

- **internal/vehicle** is the canonical model and the ONLY interchange format: `Identity`, `State`,
  `LiveSession`, `Snapshot`, in SI units (meters, km, m/s, Celsius, kW) with typed enums (`Power`,
  `Gear`, `ChargerState`, `ChargePlug`). Every protocol maps to/from it; no protocol ever touches
  another protocol's types.
- **internal/plugin/source** is the INPUT seam + registry (the `database/sql` driver pattern): a `Source`
  interface (`Name`/`Vehicles`/`Poll` -> `vehicle.Snapshot`), `Register`/`Open` by name, plugins
  self-register in `init()`.
- **internal/plugin/sink** is the OUTPUT seam + registry: a `Sink` interface (`Name`/`Handler`) over a
  `Provider` (what the cache exposes: `Vehicles`/`Latest`/`Stats`), plus an optional `Sampler`
  capability (`show` uses it to build a representative request).
- **internal/poll** is the rate-decoupling cache, generic over `source.Source`.

## Two layers: protocols and the generic seams

Code splits into a generic hub layer and per-protocol plugin packages.

- **Generic layer.** `internal/plugin/source` and `internal/plugin/sink` hold the registries and interfaces;
  `internal/vehicle` holds the canonical model; `internal/poll` holds the cache. Shared shaping helpers
  live in `internal/units` (unit conversions), `internal/plugin/sink/idmap` (canonical-id -> synthetic int64
  for the Tesla sinks), and `internal/transport/restclient` (the standardized outbound HTTP client).
- **Protocol plugins** live at `internal/protocol/<vendor>/<format>/v<n>/` (package `v1`), one
  self-contained vendor package each: wire structs, decode (source map), encode (sink map), transport,
  and routes. A package registers both its source and its sink in `init()`. Each plugin folder has a
  README documenting its wire format and mapping decisions. The Owner protocol reuses the Fleet
  package's wire/decode/encode and differs only in transport and registry name; the TeslaFi CSV
  protocol is file-based (not HTTP) and doubles as a backfill exporter via `sink.ExportHistory`.

Outbound HTTP standardizes on **go-resty/resty** via `internal/transport/restclient` (one client with the
configured timeout/retries); inbound sink servers use **go-chi/chi** routers.

## Layers and seams

Each layer knows nothing about the layers above it; each seam is a small Go interface. Below shows the
flagship configuration (`rivian-graphql-poll-v1` source, `tesla-fleet-poll-v1` sink), but any source protocol
can occupy the INPUT box and any sink protocol the OUTPUT box.

```
  +---------------------------------------------------------------+
  |  the consumer (e.g. a stock Fleet API client, untouched)      |
  |  points its base URL at the chosen SINK protocol              |
  +---------------------------------------------------------------+
        |  the sink protocol's API shape
        v
  +===============================================================+
  |  SINK  (protocol plugin, sink side)                           |
  |  serves the protocol's HTTP API; reads from the cache and     |
  |  encodes canonical -> the protocol's wire shape               |
  +===============================================================+
        ^  canonical model (read-only, from cache)
  +===============================================================+
  |  CACHE  (rate-decoupling layer)                               |
  |  one goroutine per vehicle: polls the source adaptively and   |
  |  stores the latest canonical snapshot in memory               |
  +===============================================================+
        ^  canonical model (returned by source poll)
  +===============================================================+
  |  SOURCE  (protocol plugin, source side)                       |
  |  fetches the vendor API; decodes wire -> canonical;           |
  |  refreshes short-lived session tokens (never logs in)         |
  +===============================================================+
        |  the vendor's API
        v
  +-------------------------+        +--------------------------+
  |  real vendor API        |   OR   |  mock server             |
  |                         |        |  (protocol-agnostic fake) |
  +-------------------------+        +--------------------------+
```

**SOURCE side** of a protocol package (fetch + refresh + decode). It fetches via a standardized
outbound HTTP client, refreshes its short-lived session tokens on an auth failure (it never logs in;
login is the separate `rivian_auth` / `tesla_auth` tool), and decodes the vendor wire into the
canonical model. Every URL derives from a configurable base URL, so pointing the source at the mock
retargets the whole client at a fake with no code change.

**CACHE: `internal/poll`** (the key design decision). One background goroutine **per vehicle** calls
`source.Poll` on a conservative adaptive cadence and stores a `vehicle.Snapshot`; `Latest()` returns
it with no I/O. Consumers poll the sink at their own cadence, and every read is served from cache,
so **the consumer never triggers a source call**. The loop alone decides when to reach the source,
keying its cadence on the canonical vehicle state (driving fast, parked slowly, asleep very slowly).
An idle parked car stretches further under change-adaptive backoff, snapping back the instant data
moves. Cadence values and backoff caps are all configurable; see [config/README.md](config/README.md).

Error classification (re-auth circuit breaker, rate-limit backoff) is protocol-agnostic: sources
signal auth and rate-limit failures through their error types, which the cache detects without
importing any protocol package.

**SINK side** of a protocol package (server + encode). It serves the protocol's HTTP API, reads the
canonical snapshot from the cache, and encodes it into the protocol's wire shape. The Tesla sinks
also mint stable integer ids (only the Tesla wire shapes require integer ids) and handle the token
handshake. The Owner sink reuses the Fleet sink end to end and differs only in its registered name.

**mock** (`internal/mock`) is a protocol-agnostic simulator: a scenario engine produces canonical
snapshots (idle / asleep / driving / charging / charging_ac / update), and the CHOSEN protocol's sink
renders them, so the fake speaks that protocol's API with no car and no account. `redswitchboard mock
--protocol <name>` stands it up; a source (real or another redswitchboard) points its `base_url` at it.
A small `/mock/scenario` control surface drives the scenarios at runtime.

## Streaming: two seams and a merged cache

Streaming is a different transport than the request/response REST the hub speaks by default, so it gets
the same "any source -> any sink" treatment through **two new seams**, not extensions of the existing
ones:

- **`internal/plugin/streamsource`** (push in): a `StreamSource.Run(ctx, RecordSink)` owns a
  process-lifetime listener or dialer and writes canonical snapshots into the cache asynchronously, not
  on a poll tick. Two sub-shapes fit behind one interface: an **inbound listener** (Fleet Telemetry
  mTLS push) and an **outbound dialer** (Owner streaming, one connection per awake vehicle). Inbound vs
  outbound is a plugin property, not a seam split, so a future make's push source drops in unchanged.
- **`internal/plugin/streamsink`** (push out): a `StreamSink` holds a long-lived WebSocket per consumer
  and pushes on cache change, encoded in the consumer's wire shape. There is exactly one streaming-sink
  implementation (`tesla/stream/v1`, the consumer's `data:update`) registered under **both**
  `tesla-fleet-stream-v1` and `tesla-owner-stream-v1`, so it pairs with whichever streaming source runs.

Why separate seams: `source.Poll` is pull/on-demand (a Fleet Telemetry vehicle dials in; there is
nothing to "poll"), and `sink.Handler` answers one request and returns (a streaming sink runs a
per-connection broadcaster for the connection's lifetime). Forcing either into the existing interface
would smuggle a long-lived connection into a per-call contract. Both seams stay **vendor-agnostic**
(they import only `internal/vehicle` + the cache glue, never any `internal/protocol/*`), exactly like
the REST seams, which is what keeps streaming N+M across makes.

**The merged cache (`internal/cache`).** A streaming source adds a second writer per vehicle (the poll
loop is the first), and the sink needs change-notification either way. The cache is **field-merged with
per-field provenance**: a `Merger` per vehicle is the single writer both the poll loop (via
`poll.Cache.MergePoll`) and the stream source (via `RecordSink.Put` -> `MergeStream`) write through.
The stream owns the live drive fields (location, speed, heading, gear, odometer, SOC, range, charge
power) while fresh (tracked by `Snapshot.StreamFields`); the poll loop owns everything else
(charge-state detail, config, OTA, liveness) and never regresses a streamed field while the stream is
fresher. Fleet Telemetry sends field-level **deltas**, not synchronized snapshots, so the merge is
presence-gated (a lat-only frame must not zero speed). The poll loop keeps its own last-poll snapshot
for backoff/stats; the `Merger` owns the served snapshot and a non-blocking subscriber fan-out (a slow
consumer is dropped, never blocks the writer). Dependency direction stays acyclic: the `Merger`
(`merge.go`) imports only `internal/vehicle`, and while the `Service` wiring also imports `internal/poll`
and `internal/plugin/sink`, `internal/poll` never imports `internal/cache`; the poll loop calls in
through a tiny `poll.Cache` interface.

**Stream-triggered poll at a session boundary (debounced).** A drive or charge boundary the stream
reveals (gear crossing parked<->driving, charge power dropping to 0) calls `PollNow` so the consumer gets
the terminal state and closes the session promptly instead of waiting for the next slow poll
(`internal/cache/trigger.go`). The drive boundary is **debounced per vehicle**: a parked<->driving flip
that reverses within a short window (`driveFlapWindow`, 5s) is treated as a gear flap (a P->R->D parking
shuffle, a momentary selector blip) and does not fork the drive into multiple boundaries; a genuine
boundary spaced beyond the window still fires. Charge end is not debounced (a 0kW reading is
unambiguous). The debounce state lives in the `Merger`, so it stays vehicle-only like the rest of the
cache.

**Stream-wake poll (debounced, asleep -> online).** A plain wake is not a session boundary, and the
merge deliberately keeps a confirmed-asleep car asleep on a stream frame (only Unknown/Offline are
promoted to Online, so a single stray frame cannot block the consumer's sleep). That left a gap: a car that
woke and started streaming stayed `asleep` in `/status` with no poll fired, so a just-changed poll-only
field (e.g. `charge_limit_soc`) lagged up to the slow asleep cadence. The `wakeDebouncer` (also in
`internal/cache/trigger.go`) closes it: while the cache holds `PowerSleep`, it counts the incoming
stream frames and fires `PollNow` ONCE the run reaches `wakeFrameThreshold` (3) frames spanning at
least `wakeSustainWindow` (3s), so a lone buffered/stray frame (or a same-instant buffer flush) cannot
trigger a poll and defeat sleep. It is edge-triggered: it latches after firing so a still-streaming
awake car does not re-poll, and re-arms only when the car is observed asleep again. The poll, not the
merge, performs the actual asleep -> online flip and pulls the poll-only fields (charge_limit, climate,
locks); the trigger just makes it happen within seconds instead of at the next slow poll. The state
lives in the `Merger`, vehicle-only.

**Power column = the "real online" signal (P8c).** The consumer classifies a streamed
`data:update` frame as "fake online" when the `power` column is blank/nil and "real
online" when it is numeric (even 0); on "fake online" it refuses to fetch
`vehicle_data`, so poll-only fields (SoC, `charge_limit_soc`, climate) never
refresh. The stream sink therefore emits `power` for ANY genuinely-online frame:
the live charge kW when a session is active, else a plain `0` whenever
`Power == PowerOnline` (parked-but-awake included, still gated on a location), and
BLANK only when Asleep/Offline/Unknown so a truly sleeping car reads "fake online"
and the consumer records sleep. The key insight is that the cache decouples the consumer
from the real car: TRS polls on its own sleep-respecting cadence and never wakes the
car (commands are gated off), so the consumer cannot keep the real car awake through
TRS. The old blank-when-parked behavior (P8) was thus never protecting the battery,
only the consumer's sleep accounting; emitting `0` for a car that really is awake is
more truthful and just better data quality. This composes with the cache-side
P8b wake poll: P8 = sleep correctness, P8b = the cache promoting asleep -> online and
pulling poll-only fields, P8c = the stream-side real-online signal that makes
the consumer act on the refreshed cache. Encoded in
`internal/protocol/tesla/stream/v1/stream_encode.go`.

**Monotonic served AsOf (and why it persists).** Both sinks emit `snap.AsOf`, the one canonical
served time, computed in `Merger.commit` as `maxTime(prev.AsOf, FetchedAt, StreamFields)`. A
backwards-moving served timestamp triggers the consumer's stale-fetch discard storm (it discards a frame
older than the last accepted, then refetches with zero delay, a 250 req/s spin), so the clamp never
lets the served time regress. The high-water mark is per vehicle and was in-memory only, so a restart
mid-drive forgot it: a stream time ahead of the next poll time could then drive AsOf backwards and
re-trigger the storm. The `asofStore` (in `internal/cache`) closes that hole: it persists the
per-vehicle high-water mark to a JSON file (the `stream.asof_file` knob, in the same `/data` volume as
`idmap_file`), seeds each `Merger`'s starting AsOf on construction, and writes advances back throttled
off the per-frame hot path (flushed on shutdown). It only ever RAISES the floor; live data always
wins, so a persisted mark can never move served time backwards relative to fresh data. The store
mirrors `idmap`: missing file starts clean, a corrupt file is a real boundary error.

**Durable replay buffer (restart-safe in-flight telemetry).** The `asofStore` keeps timestamps from
regressing across a restart, but it does not preserve the recent telemetry HISTORY: a TRS or consumer
restart mid-drive still loses the frames that landed during the gap before the consumer's first live
frame. The `replayBuffer` (in `internal/cache`, the `stream.replay_file` + `stream.replay_depth` knobs,
same `/data` volume as `idmap_file`) closes that gap with a bounded on-disk ring of recent merged
snapshots per vehicle. The `Service` appends each merged poll/stream snapshot to the ring; on a fresh
consumer subscribe the streaming sink type-asserts its `CacheWatcher` to the optional `CacheReplayer`
and re-emits the buffered snapshots, oldest first as full frames, BEFORE the live loop, so the
reconnecting consumer re-receives the in-flight drive instead of a cold cache with a hole. It mirrors
`asofStore`'s file-IO contract (missing file starts clean, corrupt file is a real boundary error,
throttled writes flushed on shutdown) and adds an atomic write (temp file + rename) so a crash
mid-write cannot corrupt it. The ring is bounded (oldest evicted past `replay_depth`, default 256) so
on-disk growth is capped, and it is off by default (empty `replay_file` = no capture, zero overhead).

**Stream-integrity gate (cross-check streamed numerics against the trusted poll).** The authenticated
poll is the trusted reference; a stream frame is lower-trust (the listener has no per-vehicle CA yet).
Before `MergeStream` adopts a frame, `internal/cache/integrity.go` cross-checks each streamed numeric
against the last-known value (poll- or stream-derived) and clears the presence bit of any field that is
**physically impossible or a clear regression**, so `copyStreamed` holds last-known for that one field
while the rest of the frame merges normally (per-field rejection, not whole-frame drop). The checks are
deliberately GENEROUS so a legitimate fast highway drive, a DC charge SOC ramp, a 0->full charge, and a
post-tunnel GPS jump all pass untouched: SOC must be in `[0,100]`; instantaneous speed must be under
150 m/s (540 km/h, far above a Plaid's ~322 km/h top speed); an odometer must not step backwards (beyond
a 2 m rounding slack) nor imply a ground speed over 150 m/s for the elapsed time; and a GPS move larger
than 100 m must not imply a velocity over 150 m/s (smaller moves are parked jitter and are never
checked). The elapsed-time divisor is floored at 1 s so two frames sharing a receive timestamp cannot
manufacture an infinite velocity. The thresholds are named constants in `integrity.go`, each with a
one-line WHY; there is no config knob (the ceilings are physical, not deployment-specific). Every
rejection bumps a bounded-cardinality counter labeled by reason
(`redswitchboard_stream_integrity_rejections_total{reason="odometer_regress|gps_teleport|soc_range|speed_range"}`,
never by VIN/IP) and logs one WARN line (vehicle id only, no values). The counter lives on the cache
(`Service.IntegrityStats`) and is read at scrape time by a custom collector, keeping `internal/cache`
Prometheus-free like the rest of the package. The first-ever frame (no prior poll) is still range-gated
but cannot trip a regression check (there is no baseline), and a rejected first reading is left unknown
rather than served.

## The CLI: one binary, grouped commands

`redswitchboard` is a single binary that blank-imports the protocol plugins it ships so they
self-register, then runs the command tree:

- **Run:** `serve` runs the pipeline; `mock --protocol <name>` runs a protocol-agnostic fake upstream,
  with `mock fleet-push` (push a Fleet Telemetry frame) and `mock owner-stream` (a fake Owner stream
  server) as the streaming-path doubles.
- **Monitor:** `status`, `stats`, `cache [show|raw]` query a running server read-only.
- **Preview:** `show <protocol>` renders any protocol's wire shape, picking data by precedence:
  live server > real source creds > synthetic mock.
- **Consumer:** `teslamate auth` writes the encrypted token row into the consumer's Postgres (no UI
  sign-in); `teslamate check` asserts an expected state in its DB. Used by the compose integration test.
- **Discover:** `sources`, `sinks`, `config print`, `version`.

## Configuration

One YAML file: top-level `source:` / `sink:` selectors plus per-protocol sub-blocks that each plugin
decodes itself, so adding a protocol needs no change to the config loader. Every field has a built-in
default so a partial file is always safe. The `poll:` block tunes the adaptive cadence; `http:` tunes
the outbound source client; `server:` tunes the inbound sink server. Full reference:
[config/README.md](config/README.md).

## Observability

`serve` (not `mock`) wires a protocol-agnostic Prometheus surface at `/metrics` (`internal/metrics`),
built on the official Prometheus Go client (`github.com/prometheus/client_golang`) with the standard
Go and process collectors registered. A serve-level middleware counts each sink request per
(method, normalized route, status) and observes a duration histogram; the source-side per-vehicle
counters/gauges are read from the live poll stats at scrape time via a custom Collector (no double
counting). The human-facing views are the `status` / `stats` JSON endpoints and the logs. `mock`
exposes no metrics, and logs are unstructured: these are intentional simplifications.

## Key decisions

**Provider proxy, not a fork.** The consumer supports pointing at a third-party provider host; we
impersonate one. The stock image keeps its schema, dashboards, and MQTT untouched.

**Polling, streaming, and commands.** The hub has three input shapes (a REST poll source, an
inbound-push streaming source, an outbound-dial streaming source), two output shapes (a REST sink
and a streaming WSS sink), and a gated write path (signed commands). Streaming is bidirectional
and optional: when no `stream.source`/`stream.sink` is configured, the pipeline is poll-based and
reads only the cached snapshot, byte-for-byte the v1 path. The streaming sink serves the consumer's
`data:update` wire from the cache so consumers pull live data over WSS with zero upstream calls; a
streaming source (Fleet Telemetry mTLS push, or Owner streaming dial) fills the cache from pushed
telemetry. Commands (Phase 4) are gated off-by-default (`commands.enabled: false` is structural:
no write path exists in the binary unless explicitly enabled).

**Synthetic integer ids (`idmap`, eid != vid).** The consumer requires unique integer ids; canonical
ids are strings (Rivian GUIDs). `idmap` mints a stable integer from each canonical id and persists
the mapping so ids survive restarts. Only the Tesla sinks use this, because only the Tesla wire
shapes require integer ids.

**The token handshake (no Tesla server is ever contacted).** The consumer's only auth call is a refresh
POST to `/token`, redirected to us. `handleToken` always returns a `qts-`-prefixed token, which makes
the consumer skip JWT/region parsing. The real Rivian login is handled out of band by the separate
`rivian_auth` tool.

**vehicle_data non-null invariants.** The consumer runs a strict FSM over `vehicle_data` and crashes on
bad shape. The translator guarantees all five sub-objects present and non-null (even before the first
poll), `vehicle_config` with a `car_type`, a numeric timestamp on every sub-object, a charge recorded
only when its load-bearing numerics are non-null, and the load-bearing enums emitted exactly.

**Hold-last-known.** A source sometimes drops a sentinel to zero/empty. The translator reuses the
previously translated payload for load-bearing fields that arrived empty (shift_state, GPS, range,
odometer, version, cumulative charge energy), so a transient bad reading never corrupts a drive or
charge.

## Resilience

- **Exponential backoff.** On a poll error the next delay grows exponentially up to a configured cap;
  a success resets it. An error never drops the cached snapshot, only its freshness.
- **Session-refresh circuit breaker.** A source refreshes its short-lived session tokens once on an
  auth failure; if the poll still fails, the session is dead, `poll` logs it once, sets
  `needs_reauth`, and keeps serving cache. The flag clears on the next success.
- **Staleness degradation (`poll.stale_after`).** Past the configured age the top-level `state` is
  forced `offline`, preventing a source outage mid-drive from logging a phantom infinite drive.

## Where to look

| Concern | Location |
|---|---|
| Canonical model | `internal/vehicle/` |
| Source plugin registry and interface (REST poll) | `internal/plugin/source/` |
| Sink plugin registry, interface, and Provider (REST) | `internal/plugin/sink/` |
| Streaming-source seam + registry (push in) | `internal/plugin/streamsource/` |
| Streaming-sink seam + registry (push out WSS) | `internal/plugin/streamsink/` |
| Commander seam + registry (signed-command write path, gated) | `internal/plugin/commander/` |
| Cache merge (poll + stream two-writer) and subscriber fan-out | `internal/cache/` |
| Poll cache and backoff | `internal/poll/` |
| Streaming transport helpers (gws, reconnect backoff) | `internal/transport/wssutil/` |
| Synthetic integer ids (Tesla sinks) | `internal/plugin/sink/idmap/` |
| Unit conversions | `internal/units/` |
| Protocol-agnostic mock engine | `internal/mock/` |
| Protocol plugins (one sub-package each) | `internal/protocol/` |
| Prometheus metrics | `internal/metrics/` |
| Config loading and defaults | `internal/config/` |
| CLI command tree | `internal/cli/` |
