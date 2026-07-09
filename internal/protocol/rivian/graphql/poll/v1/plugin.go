package v1

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// PluginName is the registry key for the Rivian GraphQL source plugin.
const PluginName = "rivian-graphql-poll-v1"

// init self-registers the Rivian source plugin so a binary that blank-imports
// this package can select source: rivian-graphql-poll-v1 in config.
func init() {
	source.Register(PluginName, newPlugin)
}

// Settings is the sources.rivian config sub-block. Defaults mirror the previous
// dedicated config so a partial (or absent) block still works.
type Settings struct {
	CredsFile   string                     `yaml:"creds_file"`
	BaseURL     string                     `yaml:"base_url"`
	Timeout     time.Duration              `yaml:"timeout"`
	Model       string                     `yaml:"model"`
	CarType     string                     `yaml:"car_type"`
	DisplayName string                     `yaml:"display_name"`
	Vehicles    map[string]VehicleOverride `yaml:"vehicles"`
	Debug       bool                       `yaml:"debug"`
}

// VehicleOverride carries optional per-VIN identity overrides for a mixed fleet.
type VehicleOverride struct {
	CarType     string `yaml:"car_type"`
	Model       string `yaml:"model"`
	DisplayName string `yaml:"display_name"`
}

func (s *Settings) applyDefaults() {
	if s.CredsFile == "" {
		s.CredsFile = "/data/rivian.json"
	}
	if s.BaseURL == "" {
		s.BaseURL = RivianBasePath
	}
	if s.Model == "" {
		s.Model = "R1T"
	}
	if s.CarType == "" {
		s.CarType = "model3"
	}
}

// Plugin is the Rivian implementation of source.Source. It wraps the existing
// account-level *Client and maps its parsed structs into the canonical model.
type Plugin struct {
	client   *Client
	settings Settings
	logger   *log.Logger
}

// newPlugin is the source.Factory for "rivian-graphql-poll-v1". It decodes the settings node,
// applies defaults, and constructs the underlying client (which reads the creds
// file eagerly so a missing/expired login fails fast).
func newPlugin(node *yaml.Node, logger *log.Logger) (source.Source, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s Settings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("rivian: decode settings: %w", err)
		}
	}
	s.applyDefaults()

	c, err := New(s.CredsFile, s.BaseURL, s.Debug)
	if err != nil {
		return nil, fmt.Errorf("rivian: %w (mint creds with rivian_auth first?)", err)
	}
	return &Plugin{client: c, settings: s, logger: logger}, nil
}

// Name implements source.Source.
func (p *Plugin) Name() string { return PluginName }

// Settings exposes the resolved settings so the wiring layer can read the
// per-vehicle identity overrides (model/car_type/display_name).
func (p *Plugin) Config() Settings { return p.settings }

// Vehicles implements source.Source: the account vehicle list as canonical
// identities, with model/display-name resolved from settings overrides.
func (p *Plugin) Vehicles(ctx context.Context) ([]vehicle.Identity, error) {
	vs := p.client.Vehicles()
	out := make([]vehicle.Identity, 0, len(vs))
	for _, v := range vs {
		out = append(out, vehicle.Identity{
			ID:          v.GUID,
			VIN:         v.VIN,
			DisplayName: p.displayName(v),
			Make:        "Rivian",
			Model:       p.model(v.VIN),
		})
	}
	return out, nil
}

// Poll implements source.Source: fetch the vehicle state and (only when plugged
// in) the live charging session, then map both into the canonical snapshot.
func (p *Plugin) Poll(ctx context.Context, id string) (vehicle.Snapshot, error) {
	vs, err := p.client.VehicleState(ctx, id)
	if err != nil {
		return vehicle.Snapshot{}, err
	}
	var live *LiveSessionData
	if pluggedIn(vs) {
		ls, active, lerr := p.client.LiveSession(ctx, id)
		if lerr != nil {
			if IsRateLimited(lerr) {
				return vehicle.Snapshot{}, lerr
			}
			p.logger.Printf("rivian: live session error (continuing without live data): %v", lerr)
		} else if active {
			live = ls
		}
	}
	snap := toCanonical(vs, live)
	snap.FetchedAt = time.Now()
	return snap, nil
}

// rivianWMIs are Rivian's World Manufacturer Identifiers (VIN chars 1-3): 7FC (R1T
// and the commercial van), 7PD (R1S and R2). The WMI gate keeps a non-Rivian VIN
// from decoding (e.g. a Tesla, whose char 4 'S' would otherwise look like an R1S).
var rivianWMIs = map[string]bool{"7FC": true, "7PD": true}

// rivianLineModel maps the Rivian VIN line digit (char 4) to the model: T=R1T,
// S=R1S, 2=R2 (Rivian NHTSA filings; R2 confirmed from an observed VIN 7PD2...).
var rivianLineModel = map[byte]string{
	'T': "R1T",
	'S': "R1S",
	'2': "R2",
}

// modelFromVIN auto-detects the Rivian model from the VIN, for a Rivian WMI only.
// Returns "" for a non-Rivian VIN or an unmapped line, so a config override or the
// settings default still applies.
func modelFromVIN(vin string) string {
	if len(vin) < 4 || !rivianWMIs[vin[:3]] {
		return ""
	}
	return rivianLineModel[vin[3]]
}

// model resolves a vehicle's model: an explicit per-VIN config override wins, else
// the VIN is auto-detected, else the settings default (applyDefaults -> "R1T").
func (p *Plugin) model(vin string) string {
	if ov, ok := p.settings.Vehicles[vin]; ok && ov.Model != "" {
		return ov.Model
	}
	if m := modelFromVIN(vin); m != "" {
		return m
	}
	return p.settings.Model
}

func (p *Plugin) carType(vin string) string {
	ct := p.settings.CarType
	if ov, ok := p.settings.Vehicles[vin]; ok && ov.CarType != "" {
		ct = ov.CarType
	}
	return ct
}

// CarType resolves the Tesla-side car_type placeholder for a VIN (settings
// default overlaid with any per-VIN override). The sink wiring reads it.
func (p *Plugin) CarType(vin string) string { return p.carType(vin) }

func (p *Plugin) displayName(v Vehicle) string {
	if ov, ok := p.settings.Vehicles[v.VIN]; ok && ov.DisplayName != "" {
		return ov.DisplayName
	}
	if v.Name != "" {
		return v.Name
	}
	if p.settings.DisplayName != "" {
		return p.settings.DisplayName
	}
	return "Rivian"
}

// pluggedIn reports whether the vehicle state suggests a cable is connected, so
// the charging endpoint is worth querying. (Moved from the poller; it is a
// Rivian-wire concern.)
func pluggedIn(vs *VehicleState) bool {
	if vs.ChargerStatus == "chrgr_sts_not_connected" {
		return false
	}
	switch vs.ChargerState {
	case "charging_active", "charging_connecting", "charging_ready":
		return true
	}
	return vs.ChargerStatus != "" && vs.ChargerStatus != "chrgr_sts_not_connected"
}
