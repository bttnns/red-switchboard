# Tesla Fleet wire types

The Tesla Fleet API response types the sink emits: the **output vocabulary**. The
sink encode (`sink_mapping.go`) builds these structs and the server marshals them.

The shapes mirror what TeslaMate decodes in `lib/tesla_api/vehicle/state.ex`.

## Pointers everywhere, on purpose

Fields are pointers (`*string`, `*int`, `*ChargeState`, ...) so an unset value
marshals as JSON `null` rather than a zero value. That matters: a fabricated `0`
or `""` is something TeslaMate would treat as real data, whereas `null` correctly
reads as "unknown". The encode only fills a pointer when it has a genuine value.
Note that the five top-level sub-objects (`charge_state`, `climate_state`, etc.)
must be non-null or TeslaMate discards the whole snapshot.

## Key files

- `wire.go`: `Response`, `Product`, `Summary`, `VehicleData`, and the sub-state structs.
- `fixture.go`: a static fixture used by tests.
