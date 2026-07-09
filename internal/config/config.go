// Package config loads the redswitchboard YAML configuration into a typed struct
// with sane defaults. The top level selects the active input/output plugins
// (source:/sink:) and carries per-plugin sub-blocks (sources.<name>/sinks.<name>)
// as raw YAML nodes that each plugin decodes itself, so adding a make or an
// output API needs no change here. Defaults are declared as `default:"..."`
// struct tags and applied with creasty/defaults, so a partial config file is
// always safe. The CLI may override the config path and individual values.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/creasty/defaults"
	"gopkg.in/yaml.v3"
)

// Poll holds the adaptive poll intervals per derived vehicle state, plus the
// staleness guard. The defaults below match the Rivian source (the Home Assistant
// rivian integration's cadence), since that is the default source; the Tesla
// sources use faster active-state intervals via poll_overrides. They are
// deliberately CONSERVATIVE on idle; the sink serves from cache, fully decoupled
// from how often consumers poll us.
type Poll struct {
	Online  time.Duration `yaml:"online" default:"30s"`
	Driving time.Duration `yaml:"driving" default:"60s"`
	// DrivingStreaming is the (slower) drive cadence used while a telemetry stream
	// is fresh: the stream fills live position between polls, so the poll only needs
	// to refresh poll-owned fields. When the stream stalls the loop snaps back to
	// Driving, so the worst-case data gap on a stall is one DrivingStreaming tick.
	// Zero disables the backoff (always Driving). Requires a stream cache wired.
	DrivingStreaming time.Duration `yaml:"driving_streaming" default:"2m"`
	// StreamFreshWithin is how recently a stream frame must have arrived for the
	// backoff to engage. Older than this counts as stalled -> fast Driving cadence.
	StreamFreshWithin time.Duration `yaml:"stream_fresh_within" default:"60s"`
	Charging          time.Duration `yaml:"charging" default:"30s"`
	Asleep            time.Duration `yaml:"asleep" default:"15m"`
	Default           time.Duration `yaml:"default" default:"30s"`
	// ChargingDC is the cadence while DC fast charging (high power, short session
	// where SOC moves fast and a missed stop is costly). Charging above stays the
	// AC/default cadence: AC is slow and flat, so it needs little resolution.
	ChargingDC time.Duration `yaml:"charging_dc" default:"60s"`
	// DCThresholdKw is the delivered power (kW) at or above which a session is
	// treated as DC for cadence when the source gives no fast-charger flag. AC tops
	// out ~22kW (EU 3-phase / NA dual onboard); 25 sits just above that.
	DCThresholdKw float64 `yaml:"dc_threshold_kw" default:"25"`
	// StaleAfter is the cache age past which the sink degrades the top-level
	// vehicle state to "offline", so a source outage mid-drive cannot produce a
	// phantom infinite drive.
	StaleAfter time.Duration `yaml:"stale_after" default:"30m"`
	// MinInterval is the hard floor on the poll cadence (and the minimum error
	// backoff): the loop never calls the source faster than this, whatever the
	// per-state intervals say.
	MinInterval time.Duration `yaml:"min_interval" default:"5s"`
	// MaxBackoff caps the exponential backoff applied on repeated source errors
	// or rate limits.
	MaxBackoff time.Duration `yaml:"max_backoff" default:"15m"`
	// JitterPct is the +-randomization (as a fraction, e.g. 0.1 = +-10%) applied
	// to each scheduled poll so multiple vehicles do not poll in lockstep.
	JitterPct float64 `yaml:"jitter_pct" default:"0.1"`
	// AwakeBackoffCap bounds how far an awake-but-parked car's cadence may stretch
	// under change-adaptive backoff, capping worst-case latency to notice a
	// drive/charge start. Driving and charging cadences are never stretched.
	AwakeBackoffCap time.Duration `yaml:"awake_backoff_cap" default:"5m"`
	// QuotaBlockFloor is the near-fixed backoff applied on a billing/quota block
	// (a 403 "account disabled: EXCEEDED_LIMIT"-style cap that clears only when
	// the limit is raised or the cycle resets). NOT exponential: the block is
	// sustained, so an exponential-from-min_interval cadence would hammer a dead
	// API. A server Retry-After (which can be hours) still wins when present.
	QuotaBlockFloor time.Duration `yaml:"quota_block_floor" default:"1h"`
}

// HTTP holds the outbound source HTTP client tunables, applied process-wide to
// every source's resty client at startup. A per-source `timeout` in that
// source's sub-block still takes precedence over Timeout here.
type HTTP struct {
	// Timeout is the per-request backstop a source client applies in addition to
	// any context deadline, used when a source does not set its own timeout.
	Timeout time.Duration `yaml:"timeout" default:"30s"`
	// Retries is the number of automatic retries on transient failures (network
	// errors and the 429/5xx responses resty retries by default).
	Retries int `yaml:"retries" default:"2"`
	// RetryWait is the base wait between retries (grows with backoff).
	RetryWait time.Duration `yaml:"retry_wait" default:"500ms"`
	// RetryMaxWait caps the wait between retries.
	RetryMaxWait time.Duration `yaml:"retry_max_wait" default:"5s"`
}

// Server holds the inbound HTTP server tunables for the sink's API surface.
type Server struct {
	// ReadHeaderTimeout bounds how long the server waits for request headers,
	// guarding against slow-client (Slowloris-style) stalls.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" default:"10s"`
	// ShutdownTimeout bounds graceful shutdown on SIGINT/SIGTERM before the
	// server is forced closed.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" default:"5s"`
}

// Config is the full parsed redswitchboard configuration. Source and Sink select
// the active plugins; Sources and Sinks hold each plugin's own sub-block as a
// raw node, decoded by the plugin via its registered factory.
type Config struct {
	ListenAddr string               `yaml:"listen_addr" default:":4000"`
	Source     string               `yaml:"source" default:"rivian-graphql-poll-v1"`
	Sink       string               `yaml:"sink" default:"tesla-fleet-poll-v1"`
	Sources    map[string]yaml.Node `yaml:"sources"`
	Sinks      map[string]yaml.Node `yaml:"sinks"`
	Poll       Poll                 `yaml:"poll"`
	// PollOverrides carries optional per-source poll blocks (poll_overrides.<source>)
	// overlaid on the global Poll, so each make can set its own cadence (e.g. Rivian
	// matched to Home Assistant, Tesla with faster active intervals). Stored as raw
	// nodes by value for the same reason as Sources/Sinks.
	PollOverrides map[string]yaml.Node `yaml:"poll_overrides"`
	HTTP          HTTP                 `yaml:"http"`
	Server        Server               `yaml:"server"`
	// Stream configures the optional streaming path: a streaming source (push in)
	// and/or a streaming sink (push out to consumers over WSS). When both Source
	// and Sink here are empty, the serve path is byte-for-byte the polling-only path
	// (no Merger, no extra listener). Each plugin's own sub-block lives under
	// Sources/Sinks (by value, same convention as Sources/Sinks above).
	Stream Stream `yaml:"stream"`

	// Commands configures the optional write path: sending signed Vehicle Command
	// Protocol messages to vehicles in-process via the vendored vehicle-command
	// SDK (no tesla-http-proxy sidecar). Gated off-by-default: when Enabled is
	// false (the default) the command REST routes are not mounted and no write
	// path exists in the binary (the structural read-only guarantee).
	Commands Commands `yaml:"commands"`

	// Metrics configures the /metrics surface. Currently just the cost-estimate
	// multiplier; purely additive, sensible defaults mean it can be omitted.
	Metrics Metrics `yaml:"metrics"`
}

// Metrics holds /metrics tuning knobs.
type Metrics struct {
	// VehicleDataPriceUSD is the per-call USD price of a billed Tesla vehicle_data
	// fetch, used as the cost-estimate multiplier. Default is Tesla's published
	// $0.002/call (docs/SETUP.md cost model); configurable because the rate is
	// account/cloud-specific and Tesla can revise it.
	VehicleDataPriceUSD float64 `yaml:"vehicle_data_price_usd" default:"0.002"`
}

// Stream holds the optional streaming source/sink selection plus their per-plugin
// sub-blocks. It is purely additive: zero values mean no streaming.
type Stream struct {
	Source  string               `yaml:"source"`  // active streaming source plugin (e.g. tesla-fleet-stream-v1)
	Sink    string               `yaml:"sink"`    // active streaming sink plugin (e.g. tesla-fleet-stream-v1)
	Sources map[string]yaml.Node `yaml:"sources"` // stream.sources.<name>
	Sinks   map[string]yaml.Node `yaml:"sinks"`   // stream.sinks.<name>
	// AsOfFile persists the last-served AsOf high-water mark per vehicle so the
	// merge cache's monotonic AsOf clamp survives a restart (preventing the
	// TeslaMate stale-fetch discard storm a backwards-moving timestamp triggers).
	// Empty means in-memory only. Lives in the same /data volume as idmap_file.
	AsOfFile string `yaml:"asof_file"`
	// ReplayFile backs a bounded on-disk ring of the most recent merged snapshots
	// per vehicle so a TRS or TeslaMate restart mid-drive does not drop the frames
	// that landed during the outage: on reconnect the ring is replayed to the
	// consumer in order. Complements asof_file (which restores the AsOf clamp; this
	// restores the recent telemetry history). Empty (the default) disables it with
	// zero overhead. Same /data volume as idmap_file.
	ReplayFile string `yaml:"replay_file"`
	// ReplayDepth bounds the per-vehicle replay ring (oldest evicted past it) so
	// on-disk growth is capped. 0 uses the built-in default (a few minutes of 1Hz
	// drive frames). Ignored when ReplayFile is empty.
	ReplayDepth int `yaml:"replay_depth"`
}

// Commands holds the optional signed-command path. It is purely additive:
// Enabled=false (the default) means the serve path mounts no command routes.
// The per-plugin keys (key_file/creds_file/cache_file/timeout) are decoded by
// the commander plugin from the raw node returned by CommandSettings.
type Commands struct {
	Enabled   bool          `yaml:"enabled"`    // default false; off-by-default is the read-only guarantee
	Plugin    string        `yaml:"plugin"`     // commander plugin name; default tesla-command-v1 (see DefaultCommandPlugin)
	KeyFile   string        `yaml:"key_file"`   // EC private key for Vehicle Command Protocol signing
	CredsFile string        `yaml:"creds_file"` // Fleet OAuth creds file (shared with the source)
	CacheFile string        `yaml:"cache_file"` // optional session-cache path (fewer handshake calls)
	Timeout   time.Duration `yaml:"timeout"`    // per-command timeout
}

// DefaultCommandPlugin is the commander plugin used when commands.enabled is
// true but plugin is unset. Defined here (not as a `default:` tag) because the
// plugin package must not be imported by config.go.
const DefaultCommandPlugin = "tesla-command-v1"

// CommandSettings returns the raw settings node for the commander plugin (built
// from the decoded Commands block) when commands are enabled, or nil otherwise.
// Building the node from the typed struct keeps config.go plugin-agnostic (it
// does not import the tesla-command-v1 plugin's Settings) while still handing
// the plugin a node it can Decode into its own Settings.
func (c Config) CommandSettings() *yaml.Node {
	if !c.Commands.Enabled {
		return nil
	}
	n, err := yaml.Marshal(c.Commands)
	if err != nil {
		return nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(n, &node); err != nil {
		return nil
	}
	return &node
}

// PollFor returns the poll cadence for the named source: the global Poll block
// with any per-source override (poll_overrides.<source>) overlaid on top. Only the
// keys the override sets change; the rest keep the global value.
func (c Config) PollFor(source string) Poll {
	p := c.Poll
	if n, ok := c.PollOverrides[source]; ok {
		_ = n.Decode(&p) // overlay only the keys present in the override node
	}
	return p
}

// SourceSettings returns the raw settings node for the active source (or nil if
// the file omitted it; the plugin then applies its own defaults). Values are kept
// as map[string]yaml.Node (not *yaml.Node): yaml.v3 does not populate pointer-map
// values, leaving them empty, so we store nodes by value and hand back a pointer.
func (c Config) SourceSettings() *yaml.Node { return c.SourceSettingsFor(c.Source) }

// SinkSettings returns the raw settings node for the active sink (or nil).
func (c Config) SinkSettings() *yaml.Node { return c.SinkSettingsFor(c.Sink) }

// SourceSettingsFor returns the raw settings node for a named source (nil if the
// file omitted it). Used by commands that target a source other than the active
// one (e.g. `auth <source>`).
func (c Config) SourceSettingsFor(name string) *yaml.Node {
	if n, ok := c.Sources[name]; ok {
		return &n
	}
	return nil
}

// SinkSettingsFor returns the raw settings node for a named sink (nil if absent).
func (c Config) SinkSettingsFor(name string) *yaml.Node {
	if n, ok := c.Sinks[name]; ok {
		return &n
	}
	return nil
}

// StreamSourceSettings returns the raw settings node for the active streaming
// source (or nil). Same by-value map convention as SourceSettings.
func (c Config) StreamSourceSettings() *yaml.Node { return c.StreamSourceSettingsFor(c.Stream.Source) }

// StreamSinkSettings returns the raw settings node for the active streaming
// sink (or nil).
func (c Config) StreamSinkSettings() *yaml.Node { return c.StreamSinkSettingsFor(c.Stream.Sink) }

// StreamSourceSettingsFor returns the raw settings node for a named streaming
// source (nil if absent).
func (c Config) StreamSourceSettingsFor(name string) *yaml.Node {
	if name == "" {
		return nil
	}
	if n, ok := c.Stream.Sources[name]; ok {
		return &n
	}
	return nil
}

// StreamSinkSettingsFor returns the raw settings node for a named streaming
// sink (nil if absent).
func (c Config) StreamSinkSettingsFor(name string) *yaml.Node {
	if name == "" {
		return nil
	}
	if n, ok := c.Stream.Sinks[name]; ok {
		return &n
	}
	return nil
}

// Default returns the built-in defaults (every field at its `default:` tag).
func Default() Config {
	var c Config
	_ = defaults.Set(&c)
	return c
}

// Load reads and parses the YAML config at path. Defaults are applied FIRST, then
// the file is overlaid on top, so a partial file overrides only the keys it sets
// and everything else keeps its default. This order matters: applying creasty
// defaults AFTER unmarshal walks (and corrupts) the raw `*yaml.Node` sub-blocks,
// so we never run defaults over a populated config. A missing file is not an
// error: the defaults are returned.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(raw, &cfg); err != nil {
				return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// Missing file: fall through to defaults only.
		default:
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
	}

	return cfg, nil
}
