// Package v1 is the Tesla command plugin: it implements commander.Commander
// using the vendored github.com/teslamotors/vehicle-command SDK, signing and
// submitting Vehicle Command Protocol messages in-process (no tesla-http-proxy
// container). It is the write analogue of the tesla-fleet-poll-v1 source/sink:
// one Tesla account, addressed by VIN, speaking the signed-command wire.
//
// It is GATED OFF BY DEFAULT. serve.go opens this plugin and mounts its REST
// routes only when commands.enabled is true; with the default false the command
// routes 404 and no write path exists in the binary (the read-only guarantee).
//
// The command surface matches the tesla-http-proxy exactly: command name and
// params are mapped to a vehicle.Vehicle action via proxy.ExtractCommandAction,
// so a consumer (e.g. evcc) repointed off its own tesla-http-proxy container
// onto red-switchboard's command endpoint keeps working with no name changes.
package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/teslamotors/vehicle-command/pkg/account"
	"github.com/teslamotors/vehicle-command/pkg/cache"
	"github.com/teslamotors/vehicle-command/pkg/protocol"
	"github.com/teslamotors/vehicle-command/pkg/proxy"

	"github.com/bttnns/red-switchboard/internal/metrics"
	"github.com/bttnns/red-switchboard/internal/plugin/commander"
	teslaauth "github.com/bttnns/red-switchboard/internal/protocol/tesla/auth"
	"gopkg.in/yaml.v3"
)

// PluginName is the registry key for the Tesla command plugin.
const PluginName = "tesla-command-v1"

func init() { commander.Register(PluginName, newCommander) }

// Settings is the commands: config sub-block (decoded from the raw commands
// node by the factory). CredsFile is the same Fleet OAuth creds file the source
// reads; one token serves reads AND writes. KeyFile is the EC private key whose
// public key is hosted at .well-known and enrolled in each vehicle's keychain.
type Settings struct {
	Enabled   bool          `yaml:"enabled"`
	KeyFile   string        `yaml:"key_file"`
	CredsFile string        `yaml:"creds_file"`
	CacheFile string        `yaml:"cache_file"` // optional session-cache path (fewer handshake calls)
	Timeout   time.Duration `yaml:"timeout"`
}

func (s *Settings) applyDefaults() {
	if s.Timeout == 0 {
		s.Timeout = 30 * time.Second
	}
}

// userAgent identifies red-switchboard to the Fleet API in the vehicle-command
// SDK's account.UserAgent. Tesla asks proxy operators to set a recognizable UA.
const userAgent = "redswitchboard"

// teslaCommander is the commander.Commander implementation backed by the
// vehicle-command SDK. The private key is loaded once at construction (fail-fast);
// the OAuth token is served per command by the central TokenManager (shared with
// the Fleet source) so a long-lived process commands with a refreshed token. The
// SessionCache is shared across commands so a repeat command to the same VIN skips
// the handshake round-trip.
type teslaCommander struct {
	settings Settings
	logger   *log.Logger
	privKey  protocol.ECDHPrivateKey
	mgr      *teslaauth.TokenManager
	sessions *cache.SessionCache

	// Per-VIN serialization, mirroring the proxy's lockVIN: VCSEC commands fail
	// if they arrive out of order, and the SDK's vehicle.Vehicle is not safe to
	// share across concurrent callers for the same VIN.
	vinMu sync.Map // vin -> *sync.Mutex

	// Stats counters, read by Stats() at scrape time. Wakes are counted
	// separately because they are the expensive billing category.
	statMu sync.Mutex
	stats  metrics.CommandStats
}

func newCommander(node *yaml.Node, logger *log.Logger) (commander.Commander, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s Settings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("tesla-command: decode settings: %w", err)
		}
	}
	s.applyDefaults()
	if s.KeyFile == "" {
		return nil, fmt.Errorf("tesla-command: key_file is required (enroll the public key in each vehicle's keychain first)")
	}
	if s.CredsFile == "" {
		return nil, fmt.Errorf("tesla-command: creds_file is required (the same Fleet OAuth creds file the source reads)")
	}

	// Load the EC private key once. A missing/unreadable key fails fast here
	// rather than on the first command.
	priv, err := protocol.LoadPrivateKey(s.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tesla-command: load private key %q: %w", s.KeyFile, err)
	}

	// Join the central TokenManager for the creds file (shared with the Fleet
	// source). It reads the creds eagerly, so a missing token fails fast here; each
	// command then binds the current (refreshed) access token, whose audience
	// selects the regional API host.
	mgr, err := teslaauth.Shared(s.CredsFile, logger)
	if err != nil {
		return nil, fmt.Errorf("tesla-command: %w", err)
	}

	// Session cache: persisted to CacheFile when set (reduces handshake calls
	// across restarts); in-memory otherwise. A modest entry cap bounds memory.
	sessions := cache.New(64)
	if s.CacheFile != "" {
		if loaded, err := cache.ImportFromFile(s.CacheFile); err == nil {
			sessions = loaded
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.Printf("tesla-command: ignoring unreadable cache_file %q: %v", s.CacheFile, err)
		}
	}

	return &teslaCommander{
		settings: s,
		logger:   logger,
		privKey:  priv,
		mgr:      mgr,
		sessions: sessions,
	}, nil
}

// Name implements commander.Commander.
func (c *teslaCommander) Name() string { return PluginName }

// Stats returns the current command-path counters for /metrics. It is read at
// scrape time by the optional metrics.Stats capability (wired in serve.go).
func (c *teslaCommander) Stats() metrics.CommandStats {
	c.statMu.Lock()
	defer c.statMu.Unlock()
	return c.stats
}

// SendCommand dispatches name (with params) to the vehicle identified by vin via
// the vehicle-command SDK, and records the outcome in the stats counters. The
// flow mirrors tesla-http-proxy's handleVehicleCommand so behavior matches what
// consumers expect from the proxy:
//
//  1. Per-VIN lock (VCSEC ordering + Vehicle object sharing).
//  2. account.New parses the OAuth token's audience to pick the regional host.
//  3. acct.GetVehicle wraps the inet (Fleet API) connector with the private key
//     and session cache.
//  4. Connect + StartSession establish the signed-command session (cached so a
//     repeat command skips the handshake).
//  5. proxy.ExtractCommandAction maps name+params to a vehicle.Vehicle action.
//  6. Run the action; a nominal error (vehicle rejected for a known reason) is
//     returned as Ack{Result:false}, not a Go error.
//
// A nominal error returns Ack{Result: false, Reason: err} with nil error so the
// route answers 200 with the reason (the proxy does the same). An infrastructure
// error returns a non-nil error so the route answers 5xx.
func (c *teslaCommander) SendCommand(ctx context.Context, vin, name string, params map[string]any) (commander.Ack, error) {
	ctx, cancel := context.WithTimeout(ctx, c.settings.Timeout)
	defer cancel()

	c.statMu.Lock()
	c.stats.Sent++
	if name == "wake_up" {
		c.stats.Wakes++
	}
	c.statMu.Unlock()

	ack, err := c.dispatch(ctx, vin, name, params)
	c.statMu.Lock()
	switch {
	case err != nil:
		c.stats.InfraErrors++
	case ack.Result:
		c.stats.Successes++
	default:
		c.stats.NominalFailures++
	}
	c.statMu.Unlock()
	return ack, err
}

// dispatch is the uncounted command submission; SendCommand records the outcome
// so the stats stay in one place and the dispatch path reads cleanly.
func (c *teslaCommander) dispatch(ctx context.Context, vin, name string, params map[string]any) (commander.Ack, error) {
	if vin == "" || name == "" {
		return commander.Ack{Result: false, Reason: "vin and command name are required"}, nil
	}

	unlock := c.lockVIN(vin)
	defer unlock()

	token, err := c.mgr.Token(ctx)
	if err != nil {
		return commander.Ack{}, fmt.Errorf("tesla-command: token: %w", err)
	}
	acct, err := account.New(token, userAgent)
	if err != nil {
		return commander.Ack{}, fmt.Errorf("tesla-command: build account: %w", err)
	}

	car, err := acct.GetVehicle(ctx, vin, c.privKey, c.sessions)
	if err != nil {
		return commander.Ack{}, fmt.Errorf("tesla-command: get vehicle: %w", err)
	}
	if err := car.Connect(ctx); err != nil {
		return commander.Ack{}, fmt.Errorf("tesla-command: connect: %w", err)
	}
	defer car.Disconnect()

	if err := car.StartSession(ctx, nil); err != nil {
		// The vehicle does not support the signed Vehicle Command Protocol and
		// must use the REST API. Surface as a nominal failure (not 5xx) so a
		// consumer sees the real reason the way the proxy exposes it.
		if errors.Is(err, protocol.ErrProtocolNotSupported) {
			return commander.Ack{Result: false, Reason: err.Error()}, nil
		}
		return commander.Ack{}, fmt.Errorf("tesla-command: start session: %w", err)
	}
	defer func() { _ = car.UpdateCachedSessions(c.sessions) }()

	action, err := proxy.ExtractCommandAction(ctx, name, proxy.RequestParameters(params))
	if err != nil {
		// Unknown command name or bad params: a client error, not 5xx.
		return commander.Ack{Result: false, Reason: err.Error()}, nil
	}
	if err := action(car); err != nil {
		if errors.Is(err, proxy.ErrCommandNotImplemented) || errors.Is(err, proxy.ErrCommandUseRESTAPI) {
			return commander.Ack{Result: false, Reason: err.Error()}, nil
		}
		if protocol.IsNominalError(err) {
			return commander.Ack{Result: false, Reason: err.Error()}, nil
		}
		return commander.Ack{}, fmt.Errorf("tesla-command: %s: %w", name, err)
	}

	// Persist the refreshed session cache so a restart skips handshakes. Best
	// effort: a write failure only costs a future handshake, not the command.
	if c.settings.CacheFile != "" {
		if err := c.sessions.ExportToFile(c.settings.CacheFile); err != nil {
			c.logger.Printf("tesla-command: cache persist: %v", err)
		}
	}
	return commander.Ack{Result: true}, nil
}

// lockVIN returns a release function that holds a per-VIN mutex for the duration
// of a command, serializing commands to one VIN (the proxy's lockVIN discipline).
func (c *teslaCommander) lockVIN(vin string) func() {
	v, _ := c.vinMu.LoadOrStore(vin, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// parseParams decodes a JSON request body into the map proxy.ExtractCommandAction
// consumes. An empty body is an empty params map (some commands take none).
func parseParams(body []byte) (map[string]any, error) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p == nil {
		p = map[string]any{}
	}
	return p, nil
}
