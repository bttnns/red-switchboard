# redswitchboard stack runbook

The self-hosted stack that runs **stock, unmodified TeslaMate v4** against a Rivian by pointing
TeslaMate at the `redswitchboard` provider proxy. TeslaMate, Postgres, and Grafana are official pinned
images; only `redswitchboard` (and the dev-only `mock`) are ours.

This is the operational runbook. For what redswitchboard is, the `rivian_auth` login flow, the config
reference, and introspection, see the top-level [README.md](../README.md).

## Services

| Service        | Image                          | Host port | Role |
| -------------- | ------------------------------ | --------- | ---- |
| `teslamate`    | `teslamate/teslamate:4.0.1`    | **4000**  | TeslaMate web UI + logger (stock) |
| `redswitchboard` | built from this repo (`0.1.0`) | _none_    | Rivian -> Tesla Fleet API proxy, in-network `:4000` only |
| `database`     | `postgres:18-trixie`           | _none_    | TeslaMate datastore (volume at `/var/lib/postgresql`) |
| `grafana`      | `teslamate/grafana:4.0.1`      | **3000**  | Dashboards |
| `mosquitto`    | `eclipse-mosquitto:2`          | _none_    | **optional** MQTT, off by default (`DISABLE_MQTT=true`) |
| `mock`   | same image as `redswitchboard`   | **5050**  | **dev override only** fake Rivian API (`compose.dev.yaml`) |

> Both TeslaMate and `redswitchboard` listen on `4000`, but only TeslaMate's is host-published.
> `redswitchboard` is reached by in-network DNS at `http://redswitchboard:4000`; never publish it.

`compose.yaml` puts TeslaMate in third-party-provider mode (TeslaMate's own
[Fleet API guide](https://docs.teslamate.org/docs/configuration/api/)):

```
TESLA_API_HOST=http://redswitchboard:4000
TESLA_AUTH_HOST=http://redswitchboard:4000
TESLA_AUTH_PATH=/api/oauth2/v3
TOKEN=?token=local        # literal "?token=" prefix
```

Because `TOKEN` is set, the sign-in token fields are hidden (one-click sign-in). `TESLA_WSS_HOST` is
unset on purpose: streaming is turned off per car in the UI, not via env.

## 1. Configure

From the **repo root** (where `.env` lives):

```sh
cp .env.example .env
$EDITOR .env          # set ENCRYPTION_KEY, TM_DB_PASS, GRAFANA_PASS, etc.
```

Keep these in sync (default `local` works out of the box): `.env` `TOKEN=?token=<value>`, `.env`
`REDSWITCHBOARD_TOKEN=<value>`, and `config/redswitchboard.yaml` `provider_token: "<value>"`.

## 2. Bring up the stack

### Docker

```sh
docker compose -f examples/rivian-to-teslamate/compose.yaml up -d
docker compose -f examples/rivian-to-teslamate/compose.yaml logs -f redswitchboard teslamate
```

The proxy image builds automatically from `container/redswitchboard/Containerfile` (context = repo root; the
image is the single `redswitchboard` binary, with the `serve` and `mock` run subcommands plus the
inspect/discover commands). Logging in lives in the separate `rivian_auth` / `tesla_auth` tools.

### Podman

`podman compose` (Podman 4.4+/5.x) reads the same file:

```sh
podman compose -f examples/rivian-to-teslamate/compose.yaml up -d
```

Gotchas: under rootless Podman, if the `redswitchboard` bind mounts (`../config/...`, `../data/rivian`)
hit permission errors, append `:U` (e.g. `- ../data/rivian:/data:U`). Legacy `podman-compose` (the
Python tool) does not understand `depends_on: { condition: service_healthy }`; replace those with a
plain list and start `database` first.

### Apple `container` (macOS, no Compose)

Apple's `container` has [no Compose support](https://github.com/apple/container/issues/55), so run
each service by hand on a shared network (DNS resolves service names). From the **repo root**:

```sh
# 0. shared network
container network create tm

# 1. build the proxy image (context = repo root)
container build -t redswitchboard:0.1.0 -f container/redswitchboard/Containerfile .

# 2. Postgres (v4 mounts the data volume at /var/lib/postgresql)
container run -d --name database --network tm \
  -e POSTGRES_USER=teslamate -e POSTGRES_PASSWORD=please-change-me -e POSTGRES_DB=teslamate \
  -v teslamate-db:/var/lib/postgresql postgres:18-trixie

# 3. redswitchboard proxy (unpublished; in-network :4000 only)
container run -d --name redswitchboard --network tm \
  -e REDSWITCHBOARD_TOKEN=local \
  -v "$PWD/config/redswitchboard.yaml:/config/redswitchboard.yaml:ro" \
  -v "$PWD/data/rivian:/data" \
  redswitchboard:0.1.0 serve --config /config/redswitchboard.yaml

# 4. stock TeslaMate (QUOTE the TOKEN env: zsh globs the '?' otherwise)
container run -d --name teslamate --network tm -p 4000:4000 \
  -e ENCRYPTION_KEY=please-change-me \
  -e DATABASE_USER=teslamate -e DATABASE_PASS=please-change-me \
  -e DATABASE_NAME=teslamate -e DATABASE_HOST=database -e DISABLE_MQTT=true \
  -e TESLA_API_HOST=http://redswitchboard:4000 -e TESLA_AUTH_HOST=http://redswitchboard:4000 \
  -e TESLA_AUTH_PATH=/api/oauth2/v3 -e 'TOKEN=?token=local' \
  teslamate/teslamate:4.0.1

# 5. Grafana
container run -d --name grafana --network tm -p 3000:3000 \
  -e DATABASE_USER=teslamate -e DATABASE_PASS=please-change-me \
  -e DATABASE_NAME=teslamate -e DATABASE_HOST=database \
  -e GF_SECURITY_ADMIN_USER=admin -e GF_SECURITY_ADMIN_PASSWORD=admin \
  -v teslamate-grafana-data:/var/lib/grafana teslamate/grafana:4.0.1
```

Manage with `container ls` / `logs <name>` / `rm -f <name>`. (The single-quotes on
`-e 'TOKEN=?token=local'` matter only on this raw shell path; compose handles it via `.env`.)

## 3. Operational steps (all runtimes)

1. Bring the stack up (above).
2. **Log into Rivian** with [`rivian_auth`](https://github.com/bttnns/rivian_auth), a separate tool;
   redswitchboard never sees your password. It writes the creds file the proxy reads (the rivian
   source's `creds_file`, default `/data/rivian.json`, mounted from `./data/rivian`):

   ```sh
   go install github.com/bttnns/rivian_auth@latest
   rivian_auth login --username you@example.com --password 'yourpass' \
     --out ./data/rivian/rivian.json
   # re-run adding --otp 123456 to finish MFA.
   ```

3. Open the TeslaMate UI at <http://localhost:4000> and click **Sign in** (one-time; it re-signs on
   restart).
4. **Disable streaming per car**: TeslaMate Settings -> your car -> streaming OFF
   (`use_streaming_api=false`). The proxy is poll-only; leaving it on hangs on a missing websocket.
5. Data flows. Charts in Grafana at <http://localhost:3000> (`admin` / your `.env` password).

## 4. Local-dev override (no car, no login)

`compose.dev.yaml` adds `mock` (host-published `:5050`) and repoints `redswitchboard` at the dev
config (`config/dev/redswitchboard.yaml`, `base_url` = the fake) and a checked-in dev creds directory
(`config/dev/data/`, mock tokens for each source, no `rivian_auth` login needed).

```sh
docker compose -f examples/rivian-to-teslamate/compose.yaml -f examples/rivian-to-teslamate/compose.dev.yaml up -d --build
curl -XPOST localhost:5050/mock/scenario/driving   # idle|asleep|driving|charging|charging_ac|update
curl        localhost:5050/mock/scenario           # read current scenarios
```

The mock auto-cycles each car (drive -> idle -> charge -> idle), so the live stack always has
current activity. To backfill **previous days** of history (what the Efficiency / Battery Health
dashboards need), generate a TeslaFi CSV from the host and let TeslaMate's importer ingest it:

```sh
redswitchboard mock --since 3d --import-dir ./import \
  --teslamate-compose "docker compose -f examples/rivian-to-teslamate/compose.yaml \
    -f examples/rivian-to-teslamate/compose.dev.yaml"
```

Run it against a fresh DB; add `--serve` to keep feeding live afterward. See
[LOCAL-DEV.md](../../LOCAL-DEV.md) for `--since` units and the auto-cycle knobs.

## 5. Teardown

```sh
docker compose -f examples/rivian-to-teslamate/compose.yaml down       # keep data
docker compose -f examples/rivian-to-teslamate/compose.yaml down -v    # WIPE data (drops named volumes)
```

Add `-f examples/rivian-to-teslamate/compose.dev.yaml` if you used the dev override. For Apple `container`, `container rm
-f <name>` each service.

## Pinned versions

`teslamate/teslamate:4.0.1`, `postgres:18-trixie`, `teslamate/grafana:4.0.1`, `eclipse-mosquitto:2`
(optional), `redswitchboard:0.1.0`. Re-verify the provider contract before bumping the TeslaMate tag.
