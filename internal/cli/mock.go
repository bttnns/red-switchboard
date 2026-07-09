package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	rivsrc "github.com/bttnns/red-switchboard/internal/protocol/rivian/graphql/poll/v1"
	teslafi "github.com/bttnns/red-switchboard/internal/protocol/teslafi/csv/v1"
)

// runMock runs the boundary source fake: one synthetic engine produces canonical
// snapshots that the chosen protocol's SINK renders, so the fake speaks that
// protocol's API with no car and no account.
//
// With --since it first GENERATES history: it runs the engine's auto-cycle across
// the window on a fast synthetic clock and exports a TeslaFi CSV (the shape
// TeslaMate's importer ingests), then triggers that import. With --serve (or no
// --since) it then serves live, auto-cycling drive/charge/idle until interrupted,
// so you get backfilled history AND something happening now.
func newMockCmd() *cobra.Command {
	var (
		protocol, addr, scenario, vehicles, credsOut string
		timeScale                                    float64
		backfill                                     time.Duration
		since, importDir, timezone, tmCompose        string
		serve                                        bool
		socMin, socMax, dcKw, acKw                   float64
		cycle                                        bool
	)
	cmd := &cobra.Command{
		Use:     "mock",
		Short:   "run the boundary source fake (no car, no account)",
		GroupID: groupRun,
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			logger := log.New(os.Stderr, "", log.LstdFlags)
			specs, err := parseMockVehicles(vehicles)
			if err != nil {
				return fmt.Errorf("mock: --vehicles: %w", err)
			}
			cfg := mock.DefaultCycle(socMin, socMax, dcKw, acKw)

			if since != "" {
				dur, err := parseSince(since)
				if err != nil {
					return fmt.Errorf("mock: --since: %w", err)
				}
				if err := generateBackfill(specs, dur, cfg, importDir, timezone, tmCompose, logger); err != nil {
					return err
				}
				if !serve {
					return nil
				}
			}

			return serveLive(specs, serveOpts{
				protocol:  protocol,
				addr:      addr,
				scenario:  scenario,
				timeScale: timeScale,
				backfill:  backfill,
				credsOut:  credsOut,
				cycle:     cycle,
			}, cfg, logger)
		},
	}
	f := cmd.Flags()
	f.StringVar(&protocol, "protocol", "rivian-graphql-poll-v1",
		"which protocol to mock (any registered sink: rivian-graphql-poll-v1, tesla-fleet-poll-v1, tesla-owner-poll-v1)")
	f.StringVar(&addr, "addr", ":5050", "listen address")
	f.StringVar(&scenario, "scenario", mock.ScenarioIdle, "initial scenario for all vehicles")
	f.Float64Var(&timeScale, "time-scale", 1, "simulated-clock acceleration (1=real)")
	f.DurationVar(&backfill, "backfill", 0, "start the sim clock this far in the past and run accelerated up to now")
	f.StringVar(&vehicles, "vehicles", "RIV0MOCK00000001/7FCTGAAA0NN000001/Mock Rivian/R1T",
		"comma-separated vehicles as GUID/VIN/Name/Model")
	f.StringVar(&credsOut, "creds-out", "", "if set, write a creds file for the chosen protocol's source (dev stack)")

	// Backfill (TeslaFi CSV) flags.
	f.StringVar(&since, "since", "", "generate this much history then import it (e.g. 3d, 2w, 6mo, 1y); empty = live only")
	f.BoolVar(&serve, "serve", false, "after a --since backfill, keep serving live until interrupted")
	f.StringVar(&importDir, "import-dir", "./import", "directory to write TeslaFi*.csv into (mount into TeslaMate's IMPORT_DIR)")
	f.StringVar(&timezone, "timezone", "UTC", "IANA timezone for the CSV Date column and the TeslaMate import")
	f.StringVar(&tmCompose, "teslamate-compose", "",
		`compose prefix that reaches the stack (e.g. "docker compose -f a -f b"); empty just writes the CSV and prints next steps`)

	// Auto-cycle (drive/charge/idle) knobs, shared by backfill and live serve.
	f.Float64Var(&socMin, "soc-min", 10, "auto-cycle: drive down to this SOC %")
	f.Float64Var(&socMax, "soc-max", 90, "auto-cycle: charge up to this SOC %")
	f.Float64Var(&dcKw, "dc-kw", 200, "auto-cycle: DC fast-charge power (kW)")
	f.Float64Var(&acKw, "ac-kw", 11, "auto-cycle: AC charge power (kW)")
	f.BoolVar(&cycle, "cycle", true, "live serve: auto-cycle drive/charge/idle (false = steer manually via /mock/scenario)")

	// Streaming-upstream doubles (former cmd/fleet-pusher, cmd/owner-stream-mock).
	cmd.AddCommand(newMockFleetPushCmd(), newMockOwnerStreamCmd())
	return cmd
}

// generateBackfill produces a TeslaFi CSV per vehicle and imports them into
// TeslaMate. TeslaMate's importer takes exactly one car per run and only enters
// import mode on (re)start when files are present, so cars are imported
// SEQUENTIALLY: write one car's CSV, restart, run, wait; repeat. One shared
// engine (varied per-car start state) is filtered per car so each car's history
// is generated from the same simulation.
func generateBackfill(specs []mock.VehicleSpec, dur time.Duration, cfg mock.CycleConfig,
	importDir, tz, compose string, logger *log.Logger) error {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("mock: --timezone %q: %w", tz, err)
	}
	if compose == "" && len(specs) > 1 {
		return fmt.Errorf("mock: multi-car backfill needs --teslamate-compose to sequence per-car imports; pass one --vehicles entry to write a CSV manually")
	}
	engine := mock.NewEngine(specs, 1, 0, logger)

	for _, spec := range specs {
		if err := clearCSVs(importDir); err != nil {
			return err
		}
		exporter, ok := teslafi.NewExportSink(engine, loc, importDir, logger).(sink.Exporter)
		if !ok {
			return fmt.Errorf("mock: teslafi-csv sink does not implement Exporter")
		}
		logger.Printf("mock: generating %s of history for %s -> %s", dur, spec.Name, importDir)
		if err := exporter.ExportHistory(onlyCar(engine.GenerateHistory(dur, cfg), spec.GUID)); err != nil {
			return fmt.Errorf("mock: generate %s: %w", spec.Name, err)
		}
		if compose == "" {
			logger.Printf("mock: CSV written to %s. Import it with:\n"+
				"  <compose> restart teslamate && <compose> exec -T teslamate bin/teslamate rpc %q",
				importDir, fmt.Sprintf("TeslaMate.Import.run(%q)", tz))
			return nil
		}
		if err := importOne(compose, tz, spec.Name, logger); err != nil {
			return err
		}
	}

	// Leave TeslaMate serving normally: empty the import dir and restart so it
	// exits import mode.
	if err := clearCSVs(importDir); err != nil {
		return err
	}
	logger.Printf("mock: all imports done; restarting teslamate to resume normal operation")
	return restartTeslamate(compose, logger)
}

// onlyCar filters a sample stream to one vehicle's snapshots.
func onlyCar(samples iter.Seq[sink.HistorySample], guid string) iter.Seq[sink.HistorySample] {
	return func(yield func(sink.HistorySample) bool) {
		for s := range samples {
			if s.CanonID == guid && !yield(s) {
				return
			}
		}
	}
}

// importOne restarts TeslaMate (so it discovers the CSV), triggers the import,
// and waits for it to finish.
func importOne(compose, tz, name string, logger *log.Logger) error {
	logger.Printf("mock: importing %s ...", name)
	if err := restartTeslamate(compose, logger); err != nil {
		return err
	}
	if err := waitImportReady(compose); err != nil {
		return err
	}
	if out, err := rpc(compose, fmt.Sprintf("TeslaMate.Import.run(%q)", tz)); err != nil {
		return fmt.Errorf("mock: import run failed: %w\n%s", err, out)
	}
	return pollImport(compose, name, logger)
}

// restartTeslamate restarts the teslamate service and waits for its node to
// answer rpc again.
func restartTeslamate(compose string, logger *log.Logger) error {
	if out, err := run(compose + " restart teslamate"); err != nil {
		return fmt.Errorf("mock: restart teslamate: %w\n%s", err, out)
	}
	return waitNode(compose)
}

// waitNode blocks until the teslamate node answers a trivial rpc.
func waitNode(compose string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := rpc(compose, ":ok"); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("mock: teslamate did not come back within 2m")
}

// waitImportReady blocks until the importer process has started (it only does so
// at boot when CSV files are present).
func waitImportReady(compose string) error {
	deadline := time.Now().Add(1 * time.Minute)
	for time.Now().Before(deadline) {
		if out, err := rpc(compose, "IO.inspect(TeslaMate.Import.enabled?())"); err == nil && strings.Contains(out, "true") {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("mock: TeslaMate did not enter import mode (no CSV found?)")
}

// pollImport waits for the importer's state machine to reach :complete.
func pollImport(compose, name string, logger *log.Logger) error {
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := rpc(compose, "IO.inspect(TeslaMate.Import.get_status().state)")
		if err != nil {
			return fmt.Errorf("mock: import status: %w\n%s", err, out)
		}
		switch {
		case strings.Contains(out, ":error"):
			return fmt.Errorf("mock: import of %s reported error:\n%s", name, out)
		case strings.Contains(out, ":complete"):
			logger.Printf("mock: imported %s", name)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("mock: import of %s did not finish within 15m", name)
}

// rpc runs `<compose> exec -T teslamate bin/teslamate rpc '<expr>'`.
func rpc(compose, expr string) (string, error) {
	return run(fmt.Sprintf("%s exec -T teslamate bin/teslamate rpc %s", compose, shellSingleQuote(expr)))
}

// run executes a shell command and returns its combined output.
func run(cmd string) (string, error) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	return string(out), err
}

// clearCSVs removes any TeslaFi*.csv already in dir (so each car imports alone).
func clearCSVs(dir string) error {
	matches, _ := filepath.Glob(filepath.Join(dir, "TeslaFi*.csv"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			return fmt.Errorf("mock: clear %s: %w", m, err)
		}
	}
	return nil
}

// shellSingleQuote single-quotes s for /bin/sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseSince parses a backfill window, extending Go durations with calendar
// suffixes (d, w, mo, y). Plain h/m/s fall through to time.ParseDuration ("m" is
// minutes there; months are "mo").
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	for _, u := range []struct {
		suffix string
		span   time.Duration
	}{
		{"mo", 30 * 24 * time.Hour}, // before "m"/"o"
		{"y", 365 * 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
	} {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil {
				return 0, fmt.Errorf("invalid %s value %q", u.suffix, s)
			}
			return time.Duration(n * float64(u.span)), nil
		}
	}
	return time.ParseDuration(s)
}

// serveOpts groups the live-server settings.
type serveOpts struct {
	protocol, addr, scenario, credsOut string
	timeScale                          float64
	backfill                           time.Duration
	cycle                              bool
}

// serveLive runs the mock HTTP server, optionally auto-cycling vehicles.
func serveLive(specs []mock.VehicleSpec, o serveOpts, cfg mock.CycleConfig, logger *log.Logger) error {
	engine := mock.NewEngine(specs, o.timeScale, o.backfill, logger)
	if o.scenario != mock.ScenarioIdle {
		if !engine.SetScenario(o.scenario, "") {
			return fmt.Errorf("mock: unknown initial scenario %q", o.scenario)
		}
	}
	if o.credsOut != "" {
		if err := writeMockCreds(o.protocol, o.credsOut, specs); err != nil {
			return fmt.Errorf("mock: writing creds: %w", err)
		}
		logger.Printf("mock: wrote dev creds for %d vehicle(s) to %s", len(specs), o.credsOut)
	}

	out, err := sink.Open(o.protocol, nil, engine, logger)
	if err != nil {
		return fmt.Errorf("mock: %w", err)
	}
	handler, err := out.Handler()
	if err != nil {
		return fmt.Errorf("mock: building %s handler: %w", o.protocol, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx.Done())
	if o.cycle && o.scenario == mock.ScenarioIdle {
		go runAutopilot(ctx, engine, cfg)
	}

	srv := &http.Server{
		Addr:              o.addr,
		Handler:           mock.Control(engine, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Printf("mock: serving %s on %s (%dx, %d vehicle(s), scenario %q, cycle=%v)",
		o.protocol, o.addr, int(o.timeScale), len(specs), o.scenario, o.cycle && o.scenario == mock.ScenarioIdle)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("mock: server error: %w", err)
	}
	return nil
}

// runAutopilot ticks the auto-cycle on the engine's clock until ctx is done.
func runAutopilot(ctx context.Context, engine *mock.Engine, cfg mock.CycleConfig) {
	auto := mock.NewAutopilot(engine, cfg)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			auto.Tick(engine.Now())
		}
	}
}

// parseMockVehicles parses the --vehicles flag (comma-separated GUID/VIN/Name/Model).
func parseMockVehicles(s string) ([]mock.VehicleSpec, error) {
	var out []mock.VehicleSpec
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, "/")
		if len(parts) < 2 {
			return nil, fmt.Errorf("entry %q: want GUID/VIN[/Name[/Model]]", entry)
		}
		spec := mock.VehicleSpec{GUID: parts[0], VIN: parts[1], Model: "R1T", Name: "Mock Rivian"}
		if len(parts) >= 3 && parts[2] != "" {
			spec.Name = parts[2]
		}
		if len(parts) >= 4 && parts[3] != "" {
			spec.Model = parts[3]
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no vehicles specified")
	}
	return out, nil
}

// writeMockCreds writes a creds file the chosen protocol's source can read. The
// fake does not validate it; it just needs to exist and parse.
func writeMockCreds(protocol, path string, specs []mock.VehicleSpec) error {
	switch {
	case strings.HasPrefix(protocol, "rivian"):
		vehicles := make([]rivsrc.Vehicle, 0, len(specs))
		for _, s := range specs {
			vehicles = append(vehicles, rivsrc.Vehicle{GUID: s.GUID, VIN: s.VIN, Name: s.Name})
		}
		first := vehicles[0]
		return rivsrc.SaveCreds(path, &rivsrc.AuthData{
			Token:            "mock-access",
			RefreshToken:     "mock-refresh",
			UserSessionToken: "mock-user-session",
			CSRFToken:        "mock-csrf-token",
			AppSessionToken:  "mock-app-session",
			VehicleID:        first.GUID,
			VIN:              first.VIN,
			VehicleName:      first.Name,
			Vehicles:         vehicles,
			Username:         "mock@example.com",
		})
	case strings.HasPrefix(protocol, "tesla"):
		// Tesla sources read a plain-JSON token file.
		b, _ := json.MarshalIndent(map[string]any{
			"access_token":  "mock-access",
			"refresh_token": "mock-refresh",
		}, "", "  ")
		return os.WriteFile(path, b, 0o600)
	default:
		return fmt.Errorf("no creds writer for protocol %q", protocol)
	}
}
