# rivian-graphql-poll-v1

The Rivian GraphQL protocol package (`internal/protocol/rivian/graphql/poll/v1`),
registered as both a source and a sink. As a source it talks to Rivian's cloud and
DECODES the wire into the canonical model; as a sink it ENCODES canonical back into
the Rivian GraphQL shape. Nothing leaks raw GraphQL outside this package; the rest
of the program only ever sees the canonical `vehicle.Snapshot` (and the internal
domain types `VehicleState` / `LiveSessionData`).

This package owns auth, transport, the two GraphQL queries, the header schemes, the
parse step that turns Rivian's `{ timeStamp value }` wire wrappers into flat typed
Go, and the source/sink maps. On `UNAUTHENTICATED` it refreshes its short-lived
session tokens transparently (a fresh CSRF + app-session token; Rivian has no
refresh mutation). Outbound HTTP uses the shared resty client.

## Key files

- `client.go`: the `Client` (gateway + charging transports, session-refresh glue)
  and the public `VehicleState` / `LiveSession` calls the poll loop drives.
- `source_mapping.go` / `sink_mapping.go`: Rivian wire <-> canonical model.
- `plugin.go`: registers the source and sink plugins in `init()`.
- `session.go`: the runtime, credential-free session refresh (mint a fresh CSRF +
  app-session token and persist). Logging in (CSRF -> login -> MFA) lives in the
  separate `rivian_auth` tool (github.com/bttnns/rivian_auth), not here.
- `creds.go`: the base64 creds file format this client reads.
- `transport.go`: the single-endpoint POST plumbing (gzip, timeout, debug).
- `queries.go`: the poll-safe field sets (the sans-TPMS subset that survives HTTP).
- `headers.go`: the A-Sess/U-Sess/Dc-Cid schemes per endpoint.
- `parse.go`: wire -> domain, including the `flexFloat` string/number tolerance.
- `types.go`: `VehicleState` / `LiveSessionData` with units documented inline.
- `constants.go`: `GatewayURL`/`ChargingURL` from a base, so the whole client
  retargets at the mock by changing one config value
  (`sources.rivian-graphql-poll-v1.base_url`).
