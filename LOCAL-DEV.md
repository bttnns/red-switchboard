# Local development (no car needed)

You can run the entire redswitchboard stack with **no account, no login, and no car**, using the
built-in mock (`redswitchboard mock --protocol <name>`). The mock is protocol-parameterized: a single
scenario engine produces synthetic vehicle data, and the chosen protocol's SINK renders it, so the
fake serves the exact wire shapes that protocol's real cloud returns. Point a source at it and the
real, unmodified pipeline (source client, decode, poll cache, sink) runs end to end. The flagship dev
stack uses `--protocol rivian-graphql-poll-v1` so a Rivian source reads it; you drive synthetic scenarios
(idle, driving, charging, ...) over HTTP and watch real drives and charges appear in a Tesla Fleet API
consumer and Grafana.

This is the recommended way to develop, test, and demo redswitchboard.

## Bring it up

The dev compose override wires the `mock` service in and repoints `redswitchboard` at it. It mounts a
checked-in dev config ([config/dev/redswitchboard.yaml](config/dev/redswitchboard.yaml), `base_url` -> the
mock) and a checked-in dev credentials directory (`config/dev/data/`, mock tokens for each source), so
there is nothing to log into.

```sh
docker compose -f examples/rivian-to-teslamate/compose.yaml -f examples/rivian-to-teslamate/compose.dev.yaml up -d --build
# consumer: http://localhost:4000     Grafana: http://localhost:3000
```

Then sign the consumer in (one click at <http://localhost:4000>) and turn streaming off per car, exactly
as in the [README quick start](README.md#quick-start-rivian-to-tesla-fleet-api-with-docker).

## Drive scenarios

The mock is published on host port **5050**. Switch the simulated vehicle's behavior by POSTing to its
scenario route:

```sh
curl -XPOST localhost:5050/mock/scenario/driving
curl -XPOST localhost:5050/mock/scenario/charging
curl        localhost:5050/mock/scenario           # read the current scenarios
```

| Scenario | What it simulates |
|---|---|
| `idle` | parked, charger disconnected (baseline) |
| `asleep` | asleep |
| `driving` | shift D, varied heavy-footed speed, GPS path, rising odometer, dropping range |
| `charging` | DC fast charge (~175 kW) |
| `charging_ac` | AC charge (~11 kW) |
| `update` | an OTA install that completes |

Add `?vehicle=<guid>` to target one car in a multi-vehicle mock. Hold each active scenario long enough
for the consumer to poll it (every few seconds while active, slower when parked); a charge in particular
must span at least one poll, so hold it ~100s.

By default the mock **auto-cycles** each car (`--cycle`, on unless you pass `--cycle=false`):
drive down to `--soc-min`, idle 5m, charge up to `--soc-max` (alternating `--dc-kw` and `--ac-kw`),
idle 5m, repeat, with an occasional sleep and OTA update. So the live stack always has something
happening now; the scenario POSTs above are for steering it manually.

## Backfill: previous days of history

The live mock only produces data from when it starts, but the derived dashboards (Efficiency,
Battery Health) want days of history with completed drives and charges. Generate that history as a
TeslaFi CSV and hand it to the consumer's official importer:

```sh
redswitchboard mock --since 3d --import-dir ./import \
  --teslamate-compose "docker compose -f examples/rivian-to-teslamate/compose.yaml \
    -f examples/rivian-to-teslamate/compose.dev.yaml"
```

`--since` accepts `3d`, `2w`, `6mo`, `1y` (and Go durations like `72h`). The command runs the
auto-cycle across the window on a fast synthetic clock, writes `TeslaFi<M><YYYY>.csv` into
`--import-dir` (mounted into the consumer's container at `IMPORT_DIR`), then imports it. The consumer's
importer takes one car per run and only enters import mode on (re)start, so for multiple
`--vehicles` the command restarts the `teslamate` service and imports each car in turn (hence `--teslamate-compose`
is the compose prefix, used for both `restart` and `exec`). Run it against a **fresh** DB (the consumer
only imports data earlier than a car's first existing row). Add `--serve` to keep serving live from
now after the import, so you see previous days **and** live activity. Omit `--teslamate-compose`
(single car only) to just write the CSV; the command then prints the import steps to run yourself.

## Run the binary directly (no compose)

```sh
redswitchboard mock --protocol rivian-graphql-poll-v1 --addr :5050   # fake Rivian API on :5050
# then, in another shell, point redswitchboard at it:
redswitchboard serve --config config/dev/redswitchboard.yaml
```

`--protocol` accepts any registered sink (`rivian-graphql-poll-v1`, `tesla-fleet-poll-v1`, `tesla-owner-poll-v1`), so
the mock can speak any protocol's API; a matching source then reads it. The Rivian mock also answers
the CSRF/login/getUserInfo handshake, so even
`rivian_auth login --base-url http://localhost:5050/api/gql ...` works against it. Multiple
simulated vehicles: `redswitchboard mock --vehicles "GUID/VIN/Name/Model,GUID2/VIN2/Name2/Model2"`.

## Inspect a protocol shape without a stack

`redswitchboard show <protocol>` renders the wire shape that protocol's consumer would receive,
defaulting to mock data (no creds, no server needed):

```sh
redswitchboard show tesla-fleet-poll-v1 --scenario driving   # Tesla Fleet vehicle_data, driving
redswitchboard show rivian-graphql-poll-v1                    # Rivian GraphQL response shape
```

Add `--creds <file>` to decode real source data, or `--server <host:port>` to fetch live wire from a
running `serve`.
