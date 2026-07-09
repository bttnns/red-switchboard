# tesla-owner-stream-v1 (Owner streaming source)

The **Owner streaming** source: the outbound-WSS dialer. For each served vehicle
it watches the cache's derived `Power` state (the poll loop's cheap no-wake
summary) and dials Tesla's legacy Owner streaming endpoint **only while the car is
online**, sending `data:subscribe_oauth` and decoding pushed `data:update` frames
into the canonical cache via `streamsource.RecordSink`. It is the source shape
the [`tesla-fleet-stream-v1`](../../../stream/v1/) sink imitates: same wire shape,
same column order, so the sink encode and this source decode are inverses.

It registers itself in the `streamsource` registry under:

```go
func init() { streamsource.Register("tesla-owner-stream-v1", newSource) }
```

## When to use it

- Higher resolution (~1 frame/sec while awake) than Fleet Telemetry, including
  live `power` and elevation Fleet Telemetry lacks.
- Requires the **legacy Owner API** access token (Fleet-only accounts cannot use
  it; Tesla is deprecating it).
- Lower priority than Fleet Telemetry: Fleet Telemetry is cheaper and brokerless.
  Use this only when Owner API access is available and the higher resolution
  matters.

## What it does

- **State-driven connect/disconnect.** `Run` reads the served identities from the
  cache watcher and starts one supervisor per vehicle. Each supervisor subscribes
  to cache state changes and starts a dialer when `Power == PowerOnline`, stopping
  it when the car sleeps or goes offline. An open Owner stream is never held on a
  sleeping car (which would keep it awake).
- **Escalating reconnect backoff** (`internal/transport/wssutil/backoff.go`): a dialer
  that keeps failing (dial error or immediate close) escalates with jitter and a
  cap, never a tight retry. A session that yielded at least one frame resets the
  backoff so a brief blip after a live stream reconnects promptly. This is the
  discipline that prevents the bug where TeslaMate hammered the endpoint every
  10s on an unrecognized error.
- **Decode** (`source_mapping.go`): the inverse of the sink encode. The
  `data:update` `value` CSV is parsed in TeslaMate's column order and mapped back
  to `vehicle.Snapshot` with imperial->SI conversions (mph->m/s, mi->m, mi->km).
  Empty columns are NOT marked present; the cache's presence-aware merge holds the
  last-known value, so a frame that blanked a column cannot clobber it.
- Only the **streamed live fields** are populated; the poll loop stays the
  authority for the rest.

## Decode notes

- Column order MUST match the sink's `dataColumns`. It is not shared as a
  constant (sink and source are sibling packages); `TestRoundTripEncodeDecode`
  pins the contract by encoding a snapshot with the sink's exported
  `EncodeDataUpdate` and decoding it here.
- `power` column: the Owner stream sends drive motor power (negative while
  driving) or charge power (positive while charging). The canonical model has a
  live-charge-session slot only, so positive power maps to `Live.PowerKw` and
  negative drive power is dropped (no canonical home). This matches the sink
  encode, which emits `Live.PowerKw`, so a charge frame round-trips and a drive
  frame simply omits power.
- The frame's timestamp prefix (ms epoch, falling back to now) is the stream
  frame time: it sets `StreamFields` AND the decoded `Location.TimeStamp`. A
  Go-zero Location time would emit a garbage `gps_as_of` downstream and crash
  TeslaMate's `DateTime.from_unix!`.
- `est_heading` and `heading` carry the same value; the decode prefers `heading`
  and falls back to `est_heading`.
- `elevation` and `est_range` are not in the canonical model and are ignored.
- `control:hello` / `data:error` frames are rejected by the decode (the dialer
  ignores them); a `data:error` may precede Tesla closing the connection, which
  the dialer handles via its reconnect loop.

## Configuration

See `config/README.md` -> `stream` block. Minimal:

```yaml
stream:
  source: tesla-owner-stream-v1
  sources:
    tesla-owner-stream-v1:
      creds_file: /data/tesla-owner.json   # Owner API access token (data:subscribe_oauth token)
      stream_url: wss://streaming.vn.teslamotors.com
      handshake_timeout: "10s"
      reconnect_initial: "1s"
      reconnect_max: "5m"
```

The `creds_file` is the same shape as the `tesla-owner-poll-v1` / `tesla-fleet-poll-v1`
source's; its `access_token` is the `data:subscribe_oauth` token. Pair it with the
`tesla-owner-poll-v1` (or `tesla-fleet-poll-v1`) poll source as a gap-filler for
the non-streamed fields.

## Metrics

`redswitchboard_stream_source_connected`, `redswitchboard_stream_source_frames_total`,
`redswitchboard_stream_source_last_frame_age_seconds` (per the source's `Stats()`).

Streamed numerics are cross-checked against the trusted poll by the cache-side
integrity gate (`internal/cache/integrity.go`); rejected fields hold last-known and
are counted by `redswitchboard_stream_integrity_rejections_total{reason=...}`. See
`ARCHITECTURE.md` for the thresholds and rationale.
