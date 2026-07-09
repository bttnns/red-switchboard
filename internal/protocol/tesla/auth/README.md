# tesla/auth

Central owner of Tesla OAuth credentials. Every Tesla consumer reads the same
creds file, so one `TokenManager` per file owns the access token: it serves the
current bearer to all consumers and keeps it live.

## Why central

The four Tesla consumers (`tesla-fleet-poll-v1`, `tesla-owner-poll-v1`,
`tesla-owner-stream-v1`, `tesla-command-v1`) each plug the bearer in differently
(per-request header, WebSocket handshake frame, command SDK) but read the same
creds file. Refreshing per-consumer would mean N independent refreshers racing to
write one file. Instead, `Shared(credsFile, logger)` returns a process-wide
manager **keyed by the creds-file path**: the first consumer to reference a file
creates and starts it (one background refresh goroutine); every later consumer for
that file shares the instance. `tesla.json` is shared by the Fleet poll source and
the command plugin; `tesla-owner.json` by the Owner poll and stream sources.

Plugins self-register and are built from only a YAML node, so there is no shared
DI seam to inject one object into all four. The path-keyed registry is that seam.

## Refresh

Driven both ways, mirroring TeslaMate:

- **Proactive**: a goroutine refreshes at ~75% of the token lifetime (from
  `expires_at`), rescheduling off each refresh's `expires_in`. Unknown lifetime
  (no `expires_at`) refreshes once on startup to discover it.
- **Reactive**: `RefreshAfter401(stale)` refreshes after a consumer hits a 401. It
  is single-flight and compare-and-swap deduped, so concurrent 401s from many
  vehicles trigger exactly one exchange.

Each successful refresh rewrites the creds file (0600) with the new access token,
the (possibly rotated) refresh token, and a fresh `expires_at`.

The creds file is **self-describing**: it carries the OAuth parameters needed to
refresh (`client_id`, optional `token_url`/`scope`), so refresh needs no per-plugin
config. With `client_id` (or `refresh_token`) absent the manager is **read-only**:
it serves the static token and never refreshes, preserving the pre-refresh
behavior. A rejected refresh token (HTTP 400 `invalid_grant`) is terminal: refresh
stops and the consumer surfaces `NeedsReauth` for the operator to re-mint.

See [docs/SETUP.md](../../../../docs/SETUP.md#22-mint-the-oauth-creds-file) for the
creds-file fields.
