# FAQ

Practical answers. Design: [ARCHITECTURE.md](ARCHITECTURE.md). Rivian wire protocol:
[docs/RIVIAN-API.md](docs/RIVIAN-API.md).

## My car won't go online / live data looks stuck.

The per-car **streaming toggle**. redswitchboard is poll-only and serves no websocket, so with streaming
**on** a Tesla Fleet API consumer waits on a socket that does not exist. Turn the **streaming API off for each car**
(`use_streaming_api` off) and leave `TESLA_WSS_HOST` unset. Mandatory, not optional.

## How do I re-run MFA / what is `rivian_auth ... --otp`?

Logging in lives in [`rivian_auth`](https://github.com/bttnns/rivian_auth), a separate tool;
redswitchboard only reads the creds file it writes. Most Rivian accounts use MFA, so login is two steps:

```bash
# step 1: triggers the one-time code, writes a temporary <creds>.mfa handoff file
rivian_auth login --username you@example.com --password 'your_password'

# step 2: complete with the code Rivian emailed/texted you
rivian_auth login --username you@example.com --password 'your_password' --otp 123456
```

Step 1 stores the OTP/CSRF tokens in the `.mfa` file (next to the `--out` creds path); step 2 exchanges
your code. The handoff file **expires after an hour**, so re-run step 1 if you wait too long. With a TTY
attached, step 1 can prompt for the code and finish in one go.

## My logs show a repeating `UNAUTHENTICATED`. What do I do?

Mint a fresh creds file with [`rivian_auth`](https://github.com/bttnns/rivian_auth). Rivian has **no
refresh mutation**, so a dead user session can only be replaced by a fresh login. At runtime the rivian
source refreshes its short-lived CSRF/app-session tokens once automatically; if the user session itself
is dead, that cannot recover it. You see the actionable line **once** (circuit breaker), `needs_reauth:
true` appears in `GET /status`, and the service keeps serving last-known cache until fresh creds land.

## Is my car a Tesla now?

No. The consumer's schema requires a non-null `car_type`, so redswitchboard fills it with a **cosmetic
placeholder** (default `model3`) purely to satisfy the schema; nothing about the vehicle changes.
`trim_badging` is filled from the Rivian model (default `R1T`). Both are overridable per VIN.

## Can I develop locally without a real car?

Yes. The boundary fake (`redswitchboard mock`) speaks the exact Rivian wire shapes, so the whole real
pipeline runs with no account and no vehicle. Easiest is the dev override (it wires the mock and a
static dev creds file, no login needed):

```bash
docker compose -f examples/rivian-to-teslamate/compose.yaml -f examples/rivian-to-teslamate/compose.dev.yaml up -d --build

# switch scenarios at runtime (the mock on :5050):
curl -XPOST http://localhost:5050/mock/scenario/driving
#   scenarios: idle | asleep | driving | charging | charging_ac | update
```

Or run directly: `redswitchboard mock --addr :5050`, then `redswitchboard serve` with the rivian source's
`base_url: "http://localhost:5050/api/gql"`.

The mock serves real-time data dated "now" and auto-cycles each car (drive/charge/idle). Efficiency
and Battery Health need days of history with completed charges; to backfill that, generate a TeslaFi
CSV and import it: `redswitchboard mock --since 3d --import-dir ./import --teslamate-compose "..."`
(see [LOCAL-DEV.md](LOCAL-DEV.md)). Add `--serve` to keep feeding live after the import.

## DC fast charge: charge finalization fails / Postgres errors. Why?

The **smallint overflow**, already guarded. The consumer computes AC power as `charger_actual_current *
charger_voltage` into a `smallint` column; real DC numbers (e.g. 220 A * 400 V = 88000) overflow it.
The translator detects DC (power at/above `dcChargeThresholdKw`, 25 kW), marks it fast charging, and
nulls `charger_phases`/`charger_voltage`/`charger_actual_current` (DC energy rides on
`charger_power`). If you hit this, you are running a build without the guard.

## My car woke but `/status` stayed `asleep` and a poll-only field lagged. Why?

By design, then fixed to be brief. A stream frame on a confirmed-asleep car does NOT flip it online: the
merge promotes only Unknown/Offline to Online, so a single stray or buffered frame cannot block
consumer sleep. The poll, not the stream, owns the asleep -> online flip. The catch was that a plain
wake is not a session boundary either, so no poll fired, and a poll-only field changed just before the
wake (e.g. `charge_limit_soc`, climate, locks) lagged up to the slow asleep poll cadence (~30m).

The stream-wake trigger (P8b) closes this: when SUSTAINED frames arrive while the car is asleep (3
frames spanning at least 3 seconds, so a lone buffered frame does not count), redswitchboard fires ONE
debounced `PollNow`. The poll then confirms online and pulls the poll-only fields within seconds. Sleep
still works: a single stray frame never triggers a poll, and the trigger fires once per wake (it
re-arms only after the car is seen asleep again), so a streaming awake car is not re-polled.

There is a second half to this. The consumer only refetches `vehicle_data` (where it reads those
poll-only fields) when the streamed `power` column is numeric: a blank/nil `power` is read as a "fake
online" frame and it refuses to fetch. Originally redswitchboard emitted `power` only when driving or
charging, so a parked-but-awake wake left it BLANK and the consumer never refreshed. P8c fixes the stream
side: the sink now emits `power=0` for ANY genuinely-online frame (parked-awake included, still gated on
a location), and BLANK only when the car is Asleep/Offline/Unknown. So a wake now reads "real online"
and the consumer refreshes SoC/charge_limit/climate; a sleeping car still reads "fake online" so sleep
accounting stays correct. This is safe (it is data quality, not a battery risk) because the cache
decouples the consumer from the real car: redswitchboard polls on its own sleep-respecting cadence and
never wakes the car, so the consumer physically cannot keep it awake through redswitchboard. In short: P8 =
sleep correctness, P8b = the cache-side wake poll, P8c = the stream-side real-online signal.

## Can I run more than one Rivian?

Yes. Auth is account-level (one login captures every vehicle), and redswitchboard serves all of them:
`GET /products` returns one entry per car, each `/vehicle_data` and `/source_extras` resolves
independently, and every car gets its **own poll loop** (independent cadence/backoff). For a mixed
fleet set per-VIN overrides under `sources.rivian-graphql-poll-v1.vehicles` (`car_type`, `model`, `display_name`;
empty fields fall back to the Rivian value, then the global defaults). With the mock, simulate several
(`--vehicles "GUID/VIN/Name/Model,..."`) and target one via `POST /mock/scenario/{name}?vehicle=<guid>`.

## What does `/source_extras` give me?

`GET /api/1/vehicles/{id}/source_extras` returns the **raw canonical snapshot** with no Tesla
equivalent: the neutral `state`, `live` session (when charging), and `fetched_at`, straight from
cache. It is what `redswitchboard cache raw` renders. Companions: `GET /status` (per-vehicle freshness and
poll health) and `GET /stats` (process metrics and the cache decoupling numbers).

## What versions are pinned?

Built and tested against these tags. If you bump the consumer, re-verify the provider contract
(`/token`, `/products`, `/vehicles/{id}`, `vehicle_data`) first.

| Component | Pinned image | Notes |
|---|---|---|
| Consumer | `teslamate/teslamate:4.0.1` | the v4 provider model is what makes this work |
| Postgres | `postgres:18-trixie` | v4 uses `/var/lib/postgresql`, not `/data` |
| Grafana | `teslamate/grafana:4.0.1` | the consumer's own dashboards, unchanged |
| MQTT | `eclipse-mosquitto:2` | optional; `DISABLE_MQTT=true` to drop it |

The load-bearing consumer env: `TESLA_API_HOST` / `TESLA_AUTH_HOST` at `http://redswitchboard:4000`,
`TESLA_AUTH_PATH=/api/oauth2/v3`, and `TOKEN=?token=<value>` matching `sinks.tesla-fleet-poll-v1.provider_token`.
