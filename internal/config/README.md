# internal/config

Loads the `redswitchboard` YAML config into a typed struct. Read once at startup
by `cmd/redswitchboard serve`, it selects the active sides of the hub (`source:` /
`sink:`) and holds each protocol's own sub-block under `sources.<name>` /
`sinks.<name>` as a raw YAML node the plugin decodes itself, so adding a protocol
needs no change here. It also carries the `poll:` (adaptive cadence + staleness
guard), `http:` (outbound source client), and `server:` (inbound sink server)
tunables.

Defaults are declared as `default:"..."` struct tags and applied with
`creasty/defaults`, so a partial file is always safe: any omitted field keeps its
default. The CLI can override the config path and individual values.

## Key files

- `config.go`: the `Config` / `Poll` / `HTTP` / `Server` structs, the defaults,
  and the loader.

## The annotated reference

Every field is documented inline in the example config, with its meaning, units,
and default: see [config/redswitchboard.yaml](../../config/redswitchboard.yaml) (and
the dev variant under `config/dev/`). Treat that file as the reference; this
package just enforces its types and defaults.
