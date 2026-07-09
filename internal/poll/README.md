# internal/poll

The in-memory cache and the background goroutines that fill it: the rate-limit
decoupling at the heart of the hub. A consumer hammers the sink every ~2.5s, but
those reads hit only `Latest()` here, never the upstream source. The poll loop
alone decides how often to call the source, picking an adaptive cadence from the
derived canonical vehicle state (online/driving/charging/asleep) with jitter and
a hard 5s floor.

It is generic over the `source.Source` seam (`Name`/`Vehicles`/`Poll` ->
`vehicle.Snapshot`), so any protocol's source and any test fake are
interchangeable; nothing here imports a protocol package.

## Key files

- `poll.go`: the per-vehicle `Poller` loop, the `Manager` that runs one `Poller`
  per vehicle, the `Intervals` cadence table, the cached snapshot, and `Stats`.

## Resilience

The loop applies exponential backoff (`backoff/v5`, cap ~15m) on errors and tracks
a reauth circuit-breaker, so a persistent auth failure surfaces as `NeedsReauth`
instead of spinning. Error classification is protocol-agnostic via
`source.IsUnauthenticated` / `IsRateLimited`. `Stats()` exposes poll
success/error/rate-limited/change counts plus the current backoff, which the
sink's `/status` and `/stats` endpoints and the Prometheus `/metrics` surface
render.

Downstream, the sink reads this cache through `sink.Provider`; nothing here imports
a sink or a protocol, so the cache stays a pure producer.
