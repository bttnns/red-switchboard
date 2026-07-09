# Tesla Fleet sink mapping

The encode step of the Tesla Fleet protocol: it maps a canonical
`vehicle.Snapshot` into the Tesla Fleet API `vehicle_data` shape that stock
TeslaMate (and any Fleet API consumer) reads. This is the reference sink shape for
the hub.

Everything here is a **pure function** of its inputs (no I/O, no clock beyond what
the caller passes), so the field-by-field mapping is exhaustively unit and
snapshot tested in `sink_mapping_test.go`. The sink calls in with the cached
canonical snapshot and the `idmap`-resolved integer id.

## Key files

- `sink_mapping.go`: the canonical -> `vehicle_data` converters, enum lookups, and
  the `VehicleData` builder.
- `wire/wire.go`: the emitted Tesla wire structs.

## What the field decisions actually are

The per-field rationale (which canonical field feeds which Tesla field, why a given
fallback exists, the load-bearing enum strings TeslaMate's FSM branches on) lives
in the top-level [MAPPING.md](../../../../../MAPPING.md). Read that alongside the
code; this package is deliberately mechanical so the reasoning can live there.

Three transform classes to know: metric -> imperial unit conversions (via
`internal/units`, tested), canonical-enum -> exact-Tesla-string lookups (with safe
logged fallbacks), and hold-last-known (reuse the previous payload when an upstream
sentinel dropped a load-bearing field, rather than emit a null that breaks
drive/charge detection).
