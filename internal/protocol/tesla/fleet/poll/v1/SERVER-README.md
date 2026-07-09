# Tesla Fleet HTTP surface

The sink's HTTP surface: it serves the minimal slice of the Tesla Fleet API that
stock TeslaMate (and any Fleet API consumer) polls, so the service looks like a
real Fleet API provider, not a shim.

Routing is `chi`; the car is resolved by the `{id}` path segment; data comes from
the cache through the sink adapter (`datasource.go`), backed by the poll cache.

## Endpoints

- Tesla surface: `token`, `products`, per-vehicle `summary`, and `vehicle_data`.
- Introspection: `/status`, `/stats`, `/api/1/vehicles/{id}/source_extras`.

Errors use the real Fleet API envelope (`{response, error, error_description}`)
so consumers branch the same way they would against Tesla.

## Key files

- `server.go`: the chi router and the Tesla handlers (including the `qts-` token).
- `datasource.go`: the cache-backed vehicle-data source the handlers read.
- `introspect.go`: the `/status`, `/stats`, and `source_extras` handlers.

The surface knows only the canonical model and its own wire structs (`wire/`); the
encode lives in `sink_mapping.go`, so the same server is reused by the Owner sink.
