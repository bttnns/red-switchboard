# Example stacks

Runnable, end-to-end stacks that show redswitchboard wired up for a real use case. Each example is
self-contained (its own compose files + runbook) and uses the shared `config/` and `container/` at the
repo root.

| Example | What it does |
| ------- | ------------ |
| [`rivian-to-teslamate/`](rivian-to-teslamate/) | The flagship configuration: source `rivian-graphql-poll-v1`, sink `tesla-fleet-poll-v1`, consumed by stock TeslaMate v4 + Grafana. Includes a dev override that runs against the built-in mock (no car, no account). |

redswitchboard is a hub, so an example is just a different `source:` / `sink:` pair in
`config/redswitchboard.yaml` plus the target app's container. Two configurations worth trying beyond
the flagship:

- **Owner API stop-gap**: `source: tesla-fleet-poll-v1`, `sink: tesla-owner-poll-v1`, to keep a legacy Owner-API
  app working after Tesla retires the real Owner API (see the top-level
  [README](../README.md#owner-api-stop-gap)).
- **No-car dev**: `redswitchboard mock --protocol <name>` plus `serve` (see [LOCAL-DEV.md](../LOCAL-DEV.md)).
