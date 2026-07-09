package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rivianSettings is a local mirror of the rivian source's settings sub-block,
// used to decode the raw node in tests without importing the plugin (which would
// create a cycle).
type rivianSettings struct {
	Model    string                       `yaml:"model"`
	BaseURL  string                       `yaml:"base_url"`
	CarType  string                       `yaml:"car_type"`
	Vehicles map[string]map[string]string `yaml:"vehicles"`
}

// TestDefaults documents the built-in configuration we expect when nothing is
// provided.
func TestDefaults(t *testing.T) {
	c := Default()

	assert.Equal(t, ":4000", c.ListenAddr)
	assert.Equal(t, "rivian-graphql-poll-v1", c.Source)
	assert.Equal(t, "tesla-fleet-poll-v1", c.Sink)

	// Poll cadence + the staleness guard (defaults match the Rivian/HA cadence)
	assert.Equal(t, 30*time.Second, c.Poll.Online)
	assert.Equal(t, 60*time.Second, c.Poll.Driving)
	assert.Equal(t, 2*time.Minute, c.Poll.DrivingStreaming)
	assert.Equal(t, 60*time.Second, c.Poll.StreamFreshWithin)
	assert.Equal(t, 30*time.Second, c.Poll.Charging)
	assert.Equal(t, 60*time.Second, c.Poll.ChargingDC)
	assert.Equal(t, 25.0, c.Poll.DCThresholdKw)
	assert.Equal(t, 15*time.Minute, c.Poll.Asleep)
	assert.Equal(t, 30*time.Second, c.Poll.Default)
	assert.Equal(t, 30*time.Minute, c.Poll.StaleAfter)

	// Poll timing tunables
	assert.Equal(t, 5*time.Second, c.Poll.MinInterval)
	assert.Equal(t, 15*time.Minute, c.Poll.MaxBackoff)
	assert.Equal(t, 0.1, c.Poll.JitterPct)
	assert.Equal(t, 5*time.Minute, c.Poll.AwakeBackoffCap)
	assert.Equal(t, time.Hour, c.Poll.QuotaBlockFloor)

	// Outbound HTTP client tunables
	assert.Equal(t, 30*time.Second, c.HTTP.Timeout)
	assert.Equal(t, 2, c.HTTP.Retries)
	assert.Equal(t, 500*time.Millisecond, c.HTTP.RetryWait)
	assert.Equal(t, 5*time.Second, c.HTTP.RetryMaxWait)

	// Inbound HTTP server tunables
	assert.Equal(t, 10*time.Second, c.Server.ReadHeaderTimeout)
	assert.Equal(t, 5*time.Second, c.Server.ShutdownTimeout)

	// Cost-estimate multiplier (Tesla's published vehicle_data price)
	assert.Equal(t, 0.002, c.Metrics.VehicleDataPriceUSD)
}

// TestLoadMissingFileReturnsDefaults: a missing config path is not an error.
func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)
	assert.Equal(t, ":4000", cfg.ListenAddr)
	assert.Equal(t, "rivian-graphql-poll-v1", cfg.Source)
	assert.Equal(t, "tesla-fleet-poll-v1", cfg.Sink)
}

// TestLoadPartialOverlaysDefaults: a partial file overrides only the fields it
// sets; everything else keeps its default, and per-plugin sub-blocks decode.
func TestLoadPartialOverlaysDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redswitchboard.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
listen_addr: ":5000"
source: rivian-graphql-poll-v1
sink: tesla-fleet-poll-v1
sources:
  rivian-graphql-poll-v1:
    model: "R1S"
    base_url: "http://mock:9100/api/gql"
poll:
  driving: "5s"
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, ":5000", cfg.ListenAddr)
	assert.Equal(t, 5*time.Second, cfg.Poll.Driving)

	// Untouched values keep their defaults:
	assert.Equal(t, 30*time.Second, cfg.Poll.Online)
	assert.Equal(t, 30*time.Minute, cfg.Poll.StaleAfter)
	assert.Equal(t, 5*time.Second, cfg.Poll.MinInterval)
	assert.Equal(t, 30*time.Second, cfg.HTTP.Timeout)
	assert.Equal(t, 10*time.Second, cfg.Server.ReadHeaderTimeout)

	// The active source's sub-block decodes from the raw node.
	var s rivianSettings
	require.NotNil(t, cfg.SourceSettings())
	require.NoError(t, cfg.SourceSettings().Decode(&s))
	assert.Equal(t, "R1S", s.Model)
	assert.Equal(t, "http://mock:9100/api/gql", s.BaseURL)
}

// TestLoadOverlaysTunables: the http/server blocks and the new poll tunables
// overlay correctly, and fields the file omits keep their defaults.
func TestLoadOverlaysTunables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redswitchboard.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
poll:
  min_interval: "1s"
  jitter_pct: 0.25
http:
  timeout: "12s"
  retries: 5
server:
  shutdown_timeout: "9s"
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	// Overridden values:
	assert.Equal(t, 1*time.Second, cfg.Poll.MinInterval)
	assert.Equal(t, 0.25, cfg.Poll.JitterPct)
	assert.Equal(t, 12*time.Second, cfg.HTTP.Timeout)
	assert.Equal(t, 5, cfg.HTTP.Retries)
	assert.Equal(t, 9*time.Second, cfg.Server.ShutdownTimeout)

	// Omitted values keep their defaults:
	assert.Equal(t, 15*time.Minute, cfg.Poll.MaxBackoff)
	assert.Equal(t, 5*time.Minute, cfg.Poll.AwakeBackoffCap)
	assert.Equal(t, 500*time.Millisecond, cfg.HTTP.RetryWait)
	assert.Equal(t, 5*time.Second, cfg.HTTP.RetryMaxWait)
	assert.Equal(t, 10*time.Second, cfg.Server.ReadHeaderTimeout)
}

// TestPollFor: per-source poll_overrides overlay the global poll block; only the
// keys the override sets change, the rest keep the global value, and a source with
// no override gets the global block unchanged.
func TestPollFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redswitchboard.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
poll:
  driving: "15s"
  charging: "30s"
  min_interval: "5s"
poll_overrides:
  tesla-fleet-poll-v1:
    driving: "5s"
    charging: "10m"
    charging_dc: "60s"
    dc_threshold_kw: 30
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	// The overridden source gets its faster active intervals...
	tesla := cfg.PollFor("tesla-fleet-poll-v1")
	assert.Equal(t, 5*time.Second, tesla.Driving)
	assert.Equal(t, 10*time.Minute, tesla.Charging)
	assert.Equal(t, 60*time.Second, tesla.ChargingDC)
	assert.Equal(t, 30.0, tesla.DCThresholdKw)
	// ...while keys the override omits fall back to the global block.
	assert.Equal(t, 5*time.Second, tesla.MinInterval)
	assert.Equal(t, 15*time.Minute, tesla.Asleep)

	// A source with no override gets the global block unchanged.
	riv := cfg.PollFor("rivian-graphql-poll-v1")
	assert.Equal(t, 15*time.Second, riv.Driving)
	assert.Equal(t, 30*time.Second, riv.Charging)
}

// TestPerVehicleOverrides: a mixed fleet can carry per-VIN identity overrides in
// the source sub-block.
func TestPerVehicleOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redswitchboard.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
sources:
  rivian-graphql-poll-v1:
    vehicles:
      7FCTGAAA111111111:
        model: "R1T"
        display_name: "Truck"
      7FCTGBBB222222222:
        model: "R1S"
        display_name: "SUV"
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	var s rivianSettings
	require.NoError(t, cfg.SourceSettings().Decode(&s))
	require.Len(t, s.Vehicles, 2)
	assert.Equal(t, "Truck", s.Vehicles["7FCTGAAA111111111"]["display_name"])
	assert.Equal(t, "R1S", s.Vehicles["7FCTGBBB222222222"]["model"])
}

// TestCommandsBlockRoundTrips asserts the commands: block decodes and that
// CommandSettings() returns a node the commander plugin can Decode into its own
// Settings (enabled + key_file + creds_file + timeout), and is nil when disabled.
func TestCommandsBlockRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redswitchboard.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
commands:
  enabled: true
  plugin: tesla-command-v1
  key_file: /data/key.pem
  creds_file: /data/tesla.json
  timeout: "45s"
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.Commands.Enabled)
	assert.Equal(t, "tesla-command-v1", cfg.Commands.Plugin)
	assert.Equal(t, "/data/key.pem", cfg.Commands.KeyFile)
	assert.Equal(t, 45*time.Second, cfg.Commands.Timeout)

	// CommandSettings returns a node carrying the plugin's keys.
	node := cfg.CommandSettings()
	require.NotNil(t, node)
	var s struct {
		Enabled   bool          `yaml:"enabled"`
		KeyFile   string        `yaml:"key_file"`
		CredsFile string        `yaml:"creds_file"`
		Timeout   time.Duration `yaml:"timeout"`
	}
	require.NoError(t, node.Decode(&s))
	assert.True(t, s.Enabled)
	assert.Equal(t, "/data/key.pem", s.KeyFile)
	assert.Equal(t, 45*time.Second, s.Timeout)
}

// TestCommandsDisabledYieldsNilSettings asserts the read-only default: with
// commands omitted (or enabled: false), CommandSettings is nil so serve.go never
// opens a commander and no write path is mounted.
func TestCommandsDisabledYieldsNilSettings(t *testing.T) {
	// Omitted entirely (the read-only default).
	cfg, err := Load(filepath.Join(t.TempDir(), "no-such.yaml"))
	require.NoError(t, err)
	assert.False(t, cfg.Commands.Enabled)
	assert.Nil(t, cfg.CommandSettings())

	// Explicitly disabled.
	path := filepath.Join(t.TempDir(), "redswitchboard.yaml")
	require.NoError(t, os.WriteFile(path, []byte("commands:\n  enabled: false\n"), 0o600))
	cfg, err = Load(path)
	require.NoError(t, err)
	assert.False(t, cfg.Commands.Enabled)
	assert.Nil(t, cfg.CommandSettings())
}
