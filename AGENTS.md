# AGENTS.md

Agent instructions for the red-switchboard project.

## Key docs

Consult as needed:

- `README.md`: user-facing overview, protocols table, use cases, quick start
- `ARCHITECTURE.md`: hub design and non-obvious decisions (incl. the streaming seams)
- `docs/SETUP.md`: Tesla sources, streaming, commands, cost model, poll cadence
- `MAPPING.md`: field-by-field canonical model mapping
- `FAQ.md`: gotchas and known edge cases
- `LOCAL-DEV.md`: no-car dev workflow
- `docs/RIVIAN-API.md`: the undocumented Rivian GraphQL API as we use it
- `config/README.md`: full configuration reference
- Each protocol folder under `internal/protocol/` has its own `README.md`

## Stack

Go, single binary. Dependencies and versions: `go.mod`. Metrics use the official
Prometheus Go client (`github.com/prometheus/client_golang`): Counter/Histogram
Vecs for HTTP, custom Collectors that read the poll loop's existing counters at
scrape time (no double counting), plus the standard Go/process collectors.

## Architecture

A hub: every protocol maps to/from one canonical model (`internal/vehicle`). No protocol touches
another's types. The poll cache decouples consumer poll rate from source poll rate. Protocols
self-register as plugins via `init()`. Full design: `ARCHITECTURE.md`.

## Code principles

- KISS and DRY. Simplest correct solution. No abstractions for hypothetical future use.
- No defensive code for things the architecture makes impossible. Validate only at real boundaries
  (user config, external API responses, file reads).
- No comments that describe what the code does. Only comment the WHY (hidden constraint, vendor
  quirk, non-obvious invariant). One short line max.
- Match scope to the task. Do not clean up surrounding code unless it is directly relevant.

## Dependencies

All new modules need explicit approval before being added. If a new dependency is the right call,
suggest it with a brief rationale. Prefer popular, narrow-scope, well-maintained modules over large
frameworks. Never add a dependency silently.
