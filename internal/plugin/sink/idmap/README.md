# internal/sink/idmap

Shared shaping used by the Tesla sinks: it maps a canonical vehicle id (a string,
e.g. a Rivian GUID) to a stable positive `int64`.

Why it exists: the Tesla `vehicle_data` shape (and TeslaMate behind it) stores
`eid` and `vehicle_id` as integer columns, so a string id has to become a
deterministic synthetic integer. The id derives from an FNV-1a hash of the string,
so it is stable across restarts even with no cache file, and an optional JSON file
persists the mapping for stability and inspection.

Note `eid != vid`: the `vehicle_id` comes from a salted key (`id + ":vid"`),
because TeslaMate stores both as distinct unique ints. Only the Tesla sinks use
this; other protocols address vehicles by their native id.

## Key files

- `idmap.go`: the persisted, concurrency-safe `Map` and its `New` / lookup methods.
