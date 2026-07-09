# tesla-fleet-stream-v1 / tesla-owner-stream-v1 (streaming sink)

The shared **data:update** streaming sink: it serves the legacy Tesla streaming
WebSocket shape that stock TeslaMate consumes via `TeslaApi.Stream`, reading from
the canonical cache. It is the streaming counterpart of the
[`tesla-fleet-poll-v1`](../fleet/poll/v1/) REST sink.

This one package registers itself in the `streamsink` registry under **both**
stream-family names:

```go
func init() {
    streamsink.Register("tesla-fleet-stream-v1", newSink)
    streamsink.Register("tesla-owner-stream-v1", newSink)
}
```

so it pairs with whichever streaming source is configured (Fleet Telemetry or
Owner), and with the polling-only path (the cache is filled by the poll loop).

## What it does

- Owns its **own listener** (consumer-facing `wss://`, default `:4001`), separate
  from the internal REST sink. TeslaMate dials `wss://<host>:4001`.
- Upgrades `/streaming/{token}` to a WebSocket (gws). The `{token}` segment is
  cosmetic; the sink is a cache, not Tesla, so the token is not validated
  (mirrors the REST sink's `qts-` token handshake).
- Runs the TeslaMate handshake: `data:subscribe_oauth` (or `data:subscribe_all`),
  resolves the `tag` (VIN or integer `vehicle_id` via the deterministic `idmap`)
  to a served vehicle, replies `control:hello`, rejects unknown tags with
  `data:error` + close.
- Per connected consumer, broadcasts **full** `data:update` frames on cache change,
  coalesced to `max_push_hz` (default 1/sec). Every column the cache knows is sent
  on every frame: the cache already holds last-known values, so re-sending an
  unchanged column is free and guarantees a drive boundary never lands on a frame
  missing odometer or location (a NULL there breaks TeslaMate's drive distance /
  `start_drive`). `control:hello` keepalive every 10s.
- Drop-or-close on a slow consumer: a bounded send buffer never blocks the cache
  writer; a consumer that fills it is dropped. `max_consumers` (default 16) bounds
  the connection count.

## Wire shape

The `value` column order MUST match TeslaMate's `TeslaApi.Stream.@columns` exactly
or the frame is silently dropped:

```
{"msg_type":"data:update","tag":"<vid>","value":"<ms-epoch>,speed,odometer,soc,elevation,est_heading,est_lat,est_lng,power,shift_state,range,est_range,heading"}
```

Unsupported columns (`elevation`, `est_range`) are emitted empty, matching
MyTeslaMate. Unit conversions reuse `internal/units` (m/s->mph, m->mi, km->mi);
the `shift_state` enum reuses `tesla-fleet-poll-v1`'s `ShiftState` (DRY).

**Location-gated motion (load-bearing invariant):** `shift_state` (and the
`power=0` real-online marker) are emitted only when the frame carries
`est_lat`/`est_lng`. A `shift_state` of `D` with nil lat/lng makes TeslaMate's
Position insert fail `validate_required`, which crashes `start_drive`'s hard
`{:ok,_}=` match: the drive is abandoned (NULL distance) and fragmented across the
supervisor restart. We hold gear back (blank) until its location lands so gear and
location always arrive in the same frame, mirroring the REST sink's
hold-last-known-location pattern.

**Power column = the "real online" signal (P8c):** TeslaMate reads a blank/nil
`power` as a "fake online" frame and refuses to fetch `vehicle_data`; a numeric
value (even 0) reads as "real online" and lets it refresh poll-only fields (SoC,
`charge_limit_soc`, climate) on a wake. So the encoder emits `power` for ANY
genuinely-online frame: the live charge kW when a session is active, else a plain
`0` whenever `Power == PowerOnline` (parked-but-awake included), still gated on
`hasLoc`. It stays BLANK only when the car is Asleep/Offline/Unknown, so a sleeping
car reads "fake online" and TeslaMate records sleep correctly. This is safe because
the sink is a cache: TRS polls the real car on its own sleep-respecting cadence and
never wakes it (commands are gated off), so TeslaMate physically cannot keep the
real car awake through TRS. The blank-when-parked behavior was therefore never
protecting the battery (only TeslaMate's sleep accounting); emitting `0` for a car
that really is awake is more truthful and just better data quality. (Earlier P8
left parked-awake blank; P8b added the cache-side wake poll; P8c is this
stream-side real-online signal.)

## Configuration

See `config/README.md` -> `stream` block. Minimal:

```yaml
stream:
  sink: tesla-fleet-stream-v1
  sinks:
    tesla-fleet-stream-v1:
      listen_addr: ":4001"
      max_push_hz: 1.0
      # tls: { cert_file: /data/cert.pem, key_file: /data/key.pem }
```

Point TeslaMate at it with `TESLA_WSS_HOST=wss://<host>:4001` (and
`TESLA_WSS_USE_VIN=true` to subscribe by VIN).

## Cost win

The consumer pulls drive data from the cache over WSS instead of polling Tesla's
API: TeslaMate makes **zero** Tesla API calls for the data it reads here. Paired
with a streaming source (Phase 2: Fleet Telemetry), the polling source is demoted
to a slow gap-filler for non-streamed fields.
