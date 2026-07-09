# tesla-command-v1 (Tesla command plugin)

The **command** plugin: the write seam of the hub. It implements
`commander.Commander` using the vendored `github.com/teslamotors/vehicle-command`
SDK, signing and submitting **Vehicle Command Protocol** messages to vehicles
in-process, with no `tesla-http-proxy` sidecar.

It is the write analogue of the `tesla-fleet-poll-v1` source/sink: one Tesla
account, addressed by VIN, speaking the signed-command wire. It registers itself
in the `commander` registry under:

```go
func init() { commander.Register("tesla-command-v1", newCommander) }
```

## What it is

Tesla's Vehicle Command Protocol is the signed, end-to-end-encrypted successor to
the old REST command path (`POST /command/charge_start` with just a bearer token).
The command is signed with an **EC private key** you hold, encrypted to the
vehicle, and forwarded opaquely by Tesla's server (which cannot read or forge
it). The moving pieces:

- An **EC keypair** (`prime256v1`). You hold the private key; the public key is
  hosted at `https://<app-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem`
  and enrolled in each vehicle's keychain (Tesla's virtual-key flow).
- The **Fleet OAuth access token** (the same one reads use). Its audience selects
  the regional Fleet API host.
- A **session cache** so repeat commands to the same VIN skip the handshake.

This plugin replaces the `tesla-http-proxy` container operators otherwise run
(evcc's `TESLA_HTTP_PROXY_*` pattern): red-switchboard loads the key + token once,
exposes the **same** REST path the proxy serves, and a consumer repoints onto it
with no URL changes. The separate proxy container becomes redundant.

## Gated off-by-default (the read-only guarantee)

`commands.enabled: false` (the config default) is structural: `serve.go` never
opens this plugin and never mounts the command routes, so no write path exists in
the binary. Enabling commands is an explicit operator opt-in. See
`docs/SETUP.md` (Commands) for the enrollment + key-hosting prerequisites.

## How a command flows

1. **Per-VIN lock** (`lockVIN`, mirroring the proxy's `lockVIN`): VCSEC commands
   fail if they arrive out of order, and the SDK's `vehicle.Vehicle` is not safe
   to share across concurrent callers for one VIN.
2. `account.New(token, userAgent)` parses the OAuth token's audience to pick the
   regional Fleet API host.
3. `acct.GetVehicle(ctx, vin, privKey, sessions)` wraps the inet (Fleet API)
   connector with the private key and the shared session cache.
4. `car.Connect` + `car.StartSession` establish the signed-command session (the
   cache makes a repeat command skip the handshake).
5. `proxy.ExtractCommandAction(ctx, name, params)` maps the command name + params
   to a `func(*vehicle.Vehicle) error` the SDK provides. This is the **same** map
   `tesla-http-proxy` uses, so the command surface is identical.
6. Run the action.

Outcomes:
- **Success** -> `Ack{Result: true}` -> route answers `200 {"response":{"result":true}}`.
- **Nominal failure** (the vehicle rejected the command for a known reason, e.g.
  "already charging"; `protocol.IsNominalError`) -> `Ack{Result: false, Reason}`
  with a **nil** Go error -> route answers `200` with the reason (the proxy's
  contract, not 5xx).
- `ErrProtocolNotSupported` / `ErrCommandUseRESTAPI` / `ErrCommandNotImplemented`
  -> nominal failure (200 with reason).
- **Infrastructure error** (auth, connect, signing, network) -> non-nil error ->
  route answers `502` with the Tesla error envelope.

## Configuration

See `config/README.md` -> `commands` block. Minimal:

```yaml
commands:
  enabled: true
  plugin: tesla-command-v1
  key_file: /data/tesla-command.pem   # EC private key (prime256v1)
  creds_file: /data/tesla.json        # Fleet OAuth creds (shared with the source)
  timeout: "30s"
  # cache_file: /data/tesla-command-cache.json  # optional; fewer handshake calls
```

`creds_file` is the **same** file the `tesla-fleet-poll-v1` (or
`tesla-owner-poll-v1`) source reads: one OAuth token serves reads AND writes.

## Routes

`Mount(cmdr, logger, next)` wraps the REST sink handler and serves:

- `POST /api/1/vehicles/{vin}/command/{cmd}` -> the commander; body is JSON params
  (e.g. `{"charge_limit":80}` for `set_charge_limit`).

Every other request delegates to the sink unchanged (all GET routes, `/metrics`,
`/status`, ...), so enabling commands does not alter the read surface.

## Billing

Commands and Wakes are billed **separately** from data polls (see
`docs/SETUP.md`, Cost model):

| Category | Per-unit | Rate limit |
|---|---|---|
| Commands | $0.001 ($1/1k) | 30/min |
| Wakes | $0.020 ($20/1k) | 3/min |

Wakes are the expensive category; Tesla warns that frequently waking vehicles is
improper design. The commander counts `wake_up` separately in its stats, and
commands are serialized per VIN. Wake coalescing (one wake serves multiple
pending commands) is a v1.x consideration; v1 serializes per VIN.

## Metrics

`redswitchboard_commands_total`, `redswitchboard_command_successes_total`,
`redswitchboard_command_nominal_failures_total`,
`redswitchboard_command_infra_errors_total`,
`redswitchboard_command_wakes_total`, `redswitchboard_command_rate_limited_total`
(per the commander's `Stats()`, labeled by `commander`).
