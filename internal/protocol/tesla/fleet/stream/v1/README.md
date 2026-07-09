# tesla-fleet-stream-v1 (Fleet Telemetry streaming source)

The **Fleet Telemetry** streaming source: it serves an in-process mTLS WebSocket
listener that vehicles dial into, decodes the pushed protobuf telemetry, and feeds
the canonical cache via `streamsource.RecordSink`. It is the streaming
counterpart of the [`tesla-fleet-poll-v1`](../fleet/poll/v1/) REST source: where
the poll source pulls `vehicle_data` on a tick, this source receives pushed
telemetry only on field change.

It registers itself in the `streamsource` registry under the one stream-family
source key:

```go
func init() { streamsource.Register("tesla-fleet-stream-v1", newSource) }
```

## What it does

- Binds a **public-facing mTLS listener** (vehicles dial in). mTLS is stdlib
  `crypto/tls` with `RequireAndVerifyClientCert` against the operator's
  `client_ca`, so an unpaired or forged client cert never reaches the read loop.
  The verified client cert's CN is the VIN (normalized by Tesla's
  `messages.CreateIdentityFromCert`, which also enforces Tesla's issuer/CA
  allow-list).
- Upgrades each verified connection to a WebSocket (gws) and runs an event-driven
  read/decode/ack loop. Each frame is deserialized + protobuf-decoded by Tesla's
  broker-free `telemetry.NewRecord` (no broker, no CGO), mapped to a
  `vehicle.Snapshot` (`DecodePayload`), and `Put` into the cache. Every accepted
  record is acked with `record.Ack()` so the vehicle stops retransmitting.
- **Deny-by-default:** a VIN not in the cache's known set is decoded and acked but
  never `Put`, so a rogue vehicle cannot fill the cache. An idle connection is
  reaped after `idle_timeout` (default 60s).
- **Connection caps:** the listener bounds concurrent connections at the raw
  socket (before the TLS handshake), so a pre-auth handshake flood cannot exhaust
  resources. A global cap (`max_conns`, default 32) and a per-IP cap
  (`max_conns_per_ip`, default 4, so one peer cannot consume the whole budget)
  guard the accept loop; an accept over either cap is closed immediately and
  logged. One paired vehicle in production needs a single connection, so the
  defaults are deliberately small.
- Only the **streamed live fields** are populated (Location/Speed/Heading/Gear/
  Odometer/SOC/Range/charge-power/charge-energy); the poll loop stays the authority for the rest
  (charge_state detail, config, OTA, locks). See `internal/cache` for the
  field-merge.

## No broker, no CGO

Only Tesla's broker-free packages are imported: `telemetry` + `protos` +
`messages` (pinned at `v0.9.1`). `server/streaming` + `config` are NOT imported
(they transitively pull `confluent-kafka-go`/CGO + the AWS/GCP/MQTT broker stack).
The ~150-line accept/read/ack loop is served in-tree on gws; the protobuf decode,
serializer, ack frames, and cert-identity allow-list are reused, not re-derived.

## Decode notes (verified against fleet-telemetry v0.9.1)

- `Field_Gear` is a `ShiftState` enum (`ShiftState.String()` is `"ShiftStateP"`,
  not `"P"`), with a bare-string fallback for older firmware. `gearVal` strips the
  `ShiftState` prefix and reuses `fleet/poll/v1.GearFromTesla`.
- Numeric fields arrive across oneof variants (`Int/Long/Float/Double`, sometimes
  `String`); `numVal` coalesces them all (reading only `GetDoubleValue()` would
  silently yield 0).
- Units are the vehicle's display units, not a fixed system. The decode assumes
  **US display units** (miles/mph), matching the REST source; the fixture in
  `source_mapping_test.go` pins this. Re-verify on every pin bump (Tesla renames
  `Field` values across releases).
- `telemetry.NewRecord` must be called with `transmitDecodedRecords=false` so
  `Payload()` stays protobuf bytes (true overwrites them with JSON and breaks
  `proto.Unmarshal`).
- Fleet Telemetry sends **field-level deltas**, not synchronized snapshots. The
  decode sets a `StreamPresent` bitmask of the fields a frame carried; the cache
  merges field-by-field so a lat-only frame cannot zero an earlier speed. This is
  the section-11 async-fields invariant.
- A decoded `Location` carries `TimeStamp = FetchedAt` (frame receive time; Fleet
  Telemetry carries no trustworthy per-fix time). A Go-zero Location time would
  emit a garbage `gps_as_of` downstream and crash TeslaMate's `DateTime.from_unix!`.

## Configuration

See `config/README.md` -> `stream` block. Minimal:

```yaml
stream:
  source: tesla-fleet-stream-v1
  sources:
    tesla-fleet-stream-v1:
      host: "0.0.0.0"
      port: 443
      path: "/"
      server_cert: /data/telemetry.crt
      server_key:  /data/telemetry.key
      client_ca:   /data/tesla-vehicle-ca.pem
      # known_vins: ["5YJSA11111111111"]   # optional extra deny-by-default list
      idle_timeout: "60s"
      # max_conns: 32          # global concurrent-connection cap (default 32)
      # max_conns_per_ip: 4    # per-remote-IP cap (default 4, <=0 disables)
```

Operator prerequisites (mTLS server cert for the public FQDN, the Tesla vehicle
CA bundle, partner registration, per-vehicle `fleet_telemetry_config` pairing)
live in `docs/SETUP.md` (Fleet Telemetry streaming source).

## Integrity gate (cache-side)

A streamed frame is lower-trust than the authenticated poll, so the cache
cross-checks each frame's numerics against the last-known value before adopting
them (`internal/cache/integrity.go`). A field that is physically impossible or a
clear regression (odometer backwards / impossible jump, GPS teleport, SOC outside
`[0,100]`, speed beyond a sane ceiling) is rejected FIELD-BY-FIELD: that field
holds last-known while the rest of the frame merges. The thresholds are generous
(a fast highway drive, a DC charge ramp, and a post-tunnel GPS jump all pass); see
`ARCHITECTURE.md` for the exact ceilings and rationale. The gate lives in the
vehicle-only cache, not in this decoder, so it covers every streaming source.

## Metrics

All counting stays in the source's `Stats()` snapshot (this package is
Prometheus-free); the metrics package reads the snapshot at scrape time and emits
const metrics. The streaming-source surface groups into stream-health, security,
and cost.

**Stream health:**

- `redswitchboard_stream_source_connected` (gauge): currently connected sessions.
- `redswitchboard_stream_source_connects_total`: total sessions opened (a
  reconnect storm inflates this).
- `redswitchboard_stream_source_disconnects_total`: total sessions closed. With
  `connects_total` this is the connection churn rate; `connects - disconnects`
  tracks the live count.
- `redswitchboard_stream_source_idle_timeouts_total`: sessions reaped because they
  sent no frame within `idle_timeout` (a stalled or half-open stream). A subset of
  `disconnects_total`; a clean vehicle-initiated close is not counted here.
- `redswitchboard_stream_source_frames_total`: total decoded+accepted frames.
- `redswitchboard_stream_source_last_frame_age_seconds` (gauge): seconds since the
  last decoded frame (a rising value means the stream went quiet).
- `redswitchboard_stream_source_frame_gap_seconds` (histogram): inter-frame gap
  distribution.
- `redswitchboard_stream_source_field_frames_total{field}`: frames carrying each
  streamed canonical field (confirm SOC / charge_power actually arrive).

**Security (trust boundary):**

- `redswitchboard_stream_source_rejects_total{reason}` counts trust-boundary
  rejections: `reason="identity"` (client cert failed Tesla's issuer allow-list /
  VIN extraction, rejected at the 401) and `reason="unknown_vin"` (a verified
  cert's VIN is not in the known set, the deny-by-default path). The label is a
  bounded reason, never a raw IP or VIN.
- `redswitchboard_stream_integrity_rejections_total{reason="odometer_regress|gps_teleport|soc_range|speed_range"}`
  counts streamed fields the cache integrity gate dropped (held last-known), by
  reason. Bounded cardinality (the reason set is fixed; never labeled by VIN/IP).

**Cost proxy:** the billed-poll counter `redswitchboard_source_vehicle_data_fetches_total{source,vin}`
(emitted by the poll source, not this one) is the Tesla `$` driver. Streaming is
free, so a healthy stream should drive this counter DOWN: rising stream frames with
flat vehicle_data fetches is the goal.

## Logging

Each connection lifecycle event logs the peer `RemoteAddr` so connects are
attributable: an accepted connection logs `vehicle connected from <ip> vin=<VIN>`;
an identity rejection logs `rejected connection from <ip> (identity: ...)`; an
unknown-VIN rejection logs once per connection (not per frame). VIN and RemoteAddr
are safe to log; no secret values are ever logged.
