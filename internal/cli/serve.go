package cli

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bttnns/red-switchboard/internal/cache"
	"github.com/bttnns/red-switchboard/internal/config"
	"github.com/bttnns/red-switchboard/internal/metrics"
	"github.com/bttnns/red-switchboard/internal/plugin/commander"
	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/plugin/streamsink"
	"github.com/bttnns/red-switchboard/internal/plugin/streamsource"
	"github.com/bttnns/red-switchboard/internal/poll"
	rivsrc "github.com/bttnns/red-switchboard/internal/protocol/rivian/graphql/poll/v1"
	cmdv1 "github.com/bttnns/red-switchboard/internal/protocol/tesla/command/v1"
	teslasink "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/transport/restclient"
	"github.com/bttnns/red-switchboard/internal/transport/wssutil"
	"github.com/bttnns/red-switchboard/internal/vehicle"
)

// cacheProvider implements sink.Provider over the poll Manager + the canonical
// identity list (the Manager only knows ids; the Provider adds identities).
type cacheProvider struct {
	mgr        *poll.Manager
	identities []vehicle.Identity
}

func (c *cacheProvider) Vehicles() []vehicle.Identity      { return c.identities }
func (c *cacheProvider) Latest(id string) vehicle.Snapshot { return c.mgr.Latest(id) }
func (c *cacheProvider) Stats(id string) poll.Stats        { return c.mgr.Stats(id) }

// newServeCmd runs the real pipeline: open the source, start a per-vehicle poll
// loop caching canonical snapshots, then open the sink over that cache and serve.
func newServeCmd() *cobra.Command {
	var configPath, addr, srcName, sinkName string
	var debug bool
	cmd := &cobra.Command{
		Use:     "serve",
		Short:   "run the translator (source -> sink) on an HTTP listener",
		GroupID: groupRun,
		Args:    cobra.NoArgs,
		RunE:    func(_ *cobra.Command, _ []string) error { return runServe(configPath, addr, srcName, sinkName, debug) },
	}
	cmd.Flags().StringVar(&configPath, "config", "config/redswitchboard.yaml", "path to config YAML")
	cmd.Flags().StringVar(&addr, "addr", "", "listen address override (default from config, :4000)")
	cmd.Flags().StringVar(&srcName, "source", "", "input source plugin override (default from config)")
	cmd.Flags().StringVar(&sinkName, "sink", "", "output sink plugin override (default from config)")
	cmd.Flags().BoolVar(&debug, "debug", false, "enable source client debug logging")
	return cmd
}

func runServe(configPath, addr, srcName, sinkName string, debug bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if addr != "" {
		cfg.ListenAddr = addr
	}
	if srcName != "" {
		cfg.Source = srcName
	}
	if sinkName != "" {
		cfg.Sink = sinkName
	}
	_ = debug // debug is read from the source sub-block; the flag is advisory.

	logger := log.Default()
	// Structured decision trail (P15): the poll loop, cache, and stream sources emit
	// slog lines at their key decision seams. Install a text handler once so those
	// lines share stderr with the existing log output and read cleanly for an operator.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Push the outbound HTTP client tunables into the shared restclient package
	// BEFORE opening the source, so every source's resty client picks them up.
	// This mirrors how teslasink.SetStaleAfter pushes a cross-cutting setting into
	// a plugin package. A per-source `timeout` still overrides HTTP.Timeout.
	restclient.SetDefaults(restclient.Options{
		Timeout:      cfg.HTTP.Timeout,
		Retries:      cfg.HTTP.Retries,
		RetryWait:    cfg.HTTP.RetryWait,
		RetryMaxWait: cfg.HTTP.RetryMaxWait,
	})
	restclient.SetUserAgent(version)

	// Open the input source plugin.
	src, err := source.Open(cfg.Source, cfg.SourceSettings(), logger)
	if err != nil {
		return err
	}

	identities, err := src.Vehicles(ctx)
	if err != nil {
		return fmt.Errorf("listing vehicles: %w", err)
	}
	ids := make([]string, 0, len(identities))
	for _, v := range identities {
		ids = append(ids, v.ID)
	}

	// Start the per-vehicle poll cache, using the active source's cadence (the
	// global poll block plus any poll_overrides for this source).
	pollCfg := cfg.PollFor(cfg.Source)
	mgr := poll.NewManager(src, ids, poll.Intervals{
		Online:            pollCfg.Online,
		Driving:           pollCfg.Driving,
		DrivingStreaming:  pollCfg.DrivingStreaming,
		StreamFreshWithin: pollCfg.StreamFreshWithin,
		Charging:          pollCfg.Charging,
		ChargingDC:        pollCfg.ChargingDC,
		DCThresholdKw:     pollCfg.DCThresholdKw,
		Asleep:            pollCfg.Asleep,
		Default:           pollCfg.Default,
		MinInterval:       pollCfg.MinInterval,
		MaxBackoff:        pollCfg.MaxBackoff,
		JitterPct:         pollCfg.JitterPct,
		AwakeBackoffCap:   pollCfg.AwakeBackoffCap,
		QuotaBlockFloor:   pollCfg.QuotaBlockFloor,
	}, logger)

	var prov sink.Provider = &cacheProvider{mgr: mgr, identities: identities}

	// Streaming is purely additive: when neither a stream source nor a stream
	// sink is configured, none of this runs and the serve path is today's (the
	// poll Manager owns the cache, the REST sink reads it). When streaming is on,
	// the per-vehicle Merger set becomes the SERVED cache: the poll loop writes
	// through it, every sink reads from it.
	streamingOn := cfg.Stream.Source != "" || cfg.Stream.Sink != ""
	var streamSvc *cache.Service
	if streamingOn {
		streamSvc, err = cache.NewService(identities, cfg.Poll.StaleAfter, cfg.Stream.AsOfFile, cfg.Stream.ReplayFile, cfg.Stream.ReplayDepth, logger)
		if err != nil {
			return err
		}
		streamSvc.SetManager(mgr)
		streamSvc.SetPollTrigger(mgr.PollNow)                     // stream session boundary -> immediate poll
		streamSvc.SetStreamFreshWithin(pollCfg.StreamFreshWithin) // suppress recycle disconnect-polls while the stream is this fresh
		mgr.SetCache(streamSvc)
		prov = streamSvc.Provider(mgr) // Latest from the Merger; Stats from the poll Manager
	}

	// Start the poll loops only AFTER the cache wiring above: SetCache writes each
	// Poller's cache field, which the poll goroutines read on their immediate first
	// poll, so starting Run earlier races that write (and the first poll would bypass
	// the Merger).
	go mgr.Run(ctx)

	// Hand the sink the global stale-after and the source's per-VIN car_type
	// placeholder (Tesla's vehicle_config needs a car_type). These are
	// cross-cutting inputs the generic sink.Factory signature does not carry.
	teslasink.SetStaleAfter(cfg.Poll.StaleAfter)
	if rs, ok := src.(*rivsrc.Plugin); ok {
		teslasink.SetCarTypeResolver(rs.CarType)
	}

	out, err := sink.Open(cfg.Sink, cfg.SinkSettings(), prov, logger)
	if err != nil {
		return err
	}
	handler, err := out.Handler()
	if err != nil {
		return err
	}

	// Add a protocol-agnostic /metrics surface at the serve level (NOT inside any
	// sink), mirroring how mock.Control front-runs the sink handler with a small
	// ServeMux. The sink handler is wrapped so every served request is recorded;
	// the source-side gauges/counters are read from the live poll stats at scrape
	// time (no duplicate counting). The catch-all "/" preserves the sink's own
	// endpoints (e.g. Tesla's /stats and /status).
	reg := metrics.New()
	if streamSvc != nil {
		reg.SetStreamIntegrity(streamSvc.IntegrityStats) // merge-side stream-integrity rejections
		reg.SetSession(streamSvc.SessionStats)           // drive/charge opened-vs-closed counters
	}
	if u, ok := out.(interface{ UnchangedReads() int64 }); ok {
		reg.SetSourceUnchanged(cfg.Source, u.UnchangedReads) // P7 spinning-consumer signal
	}
	sourceSnap := func() []metrics.VehicleMetric {
		out := make([]metrics.VehicleMetric, 0, len(identities))
		for _, v := range identities {
			s := prov.Stats(v.ID)
			vin := v.VIN
			if vin == "" {
				vin = v.ID
			}
			out = append(out, metrics.VehicleMetric{
				VIN:                  vin,
				SuccessCount:         s.SuccessCount,
				ErrorCount:           s.ErrorCount,
				RateLimitedCount:     s.RateLimitedCount,
				ChangedCount:         s.ChangedCount,
				Backoff:              s.Backoff,
				ConsecutiveFailures:  s.ConsecutiveFailures,
				NeedsReauth:          s.NeedsReauth,
				QuotaBlockedCount:    s.QuotaBlockedCount,
				QuotaBlockedUntil:    s.QuotaBlockedUntil,
				TelemetryConfigWiped: s.TelemetryConfigWiped,
				VehicleDataFetches:   s.VehicleDataFetches,
				PollsByState:         s.PollsByState,
				ScheduledInterval:    s.ScheduledInterval,
				StreamBackoffActive:  s.StreamBackoffActive,
				DerivedState:         s.DerivedState,
				LastPollAt:           s.LastSuccessAt,
			})
		}
		return out
	}
	reg.SetSource(cfg.Source, sourceSnap)
	reg.SetSourceCost(cfg.Source, cfg.Metrics.VehicleDataPriceUSD, sourceSnap)

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler(len(identities)))

	// The command path is GATED OFF by default. When commands.enabled is true,
	// open the commander plugin and wrap the sink handler with the command POST
	// routes (/api/1/vehicles/{vin}/command/{cmd}). When false, the sink handler
	// is used verbatim and no write path exists (the structural read-only
	// guarantee). The command routes delegate every non-command request to the
	// sink unchanged, so enabling commands does not alter the read surface.
	sinkHandler := handler
	if cfg.Commands.Enabled {
		pluginName := cfg.Commands.Plugin
		if pluginName == "" {
			pluginName = config.DefaultCommandPlugin
			logger.Printf("commands enabled with no plugin; defaulting to %s", pluginName)
		}
		cmdr, err := commander.Open(pluginName, cfg.CommandSettings(), logger)
		if err != nil {
			return err
		}
		// Wire the command counters if the commander exposes them.
		if sp, ok := cmdr.(interface{ Stats() metrics.CommandStats }); ok {
			name := cmdr.Name()
			reg.SetCommand(name, func() metrics.CommandStats { return sp.Stats() })
		}
		sinkHandler = cmdv1.Mount(cmdr, logger, handler)
		logger.Printf("commands enabled (commander %s); POST /api/1/vehicles/{vin}/command/{cmd}", cmdr.Name())
	}

	mux.Handle("/", reg.Middleware(sinkHandler))

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
	}

	// Start the streaming SOURCE if configured (nil-safe). It writes pushed
	// records into the same cache the poll loop fills.
	if cfg.Stream.Source != "" {
		ssrc, err := streamsource.Open(cfg.Stream.Source, cfg.StreamSourceSettings(), logger)
		if err != nil {
			return err
		}
		// Wire the source-side gauges if the source exposes them (optional
		// capability; both streaming sources do via wssutil.SourceCounters).
		if sp, ok := ssrc.(wssutil.SourceStatser); ok {
			reg.SetStreamSource(func() metrics.StreamSourceStats {
				s := sp.Stats()
				return metrics.StreamSourceStats{
					Connected:    s.Connected,
					Connects:     s.Connects,
					Frames:       s.Frames,
					LastFrameAge: s.LastFrameAge,
					GapBuckets:   s.GapBuckets,
					GapSum:       s.GapSum,
					GapCount:     s.GapCount,
					FieldFrames:  s.FieldFrames,
					Rejects:      s.Rejects,
					Disconnects:  s.Disconnects,
					IdleTimeouts: s.IdleTimeouts,
				}
			})
		}
		// streamSvc implements streamsource.CacheWatcher; an outbound dialer reads
		// it to follow vehicle state, an inbound listener ignores it.
		go func() {
			if err := ssrc.Run(ctx, streamSvc, streamSvc); err != nil {
				logger.Printf("stream source: %v", err)
			}
		}()
	}

	// Start the streaming SINK if configured. It owns its own listener (its own
	// port + optional TLS) and shuts it down on ctx.Done, independent of the REST
	// server. TeslaMate dials wss:// here.
	if cfg.Stream.Sink != "" {
		ssink, err := streamsink.Open(cfg.Stream.Sink, cfg.StreamSinkSettings(), streamSvc, logger)
		if err != nil {
			return err
		}
		// Wire the sink-side gauges if the sink exposes them (optional capability;
		// the data:update sink does).
		if sp, ok := ssink.(wssutil.SinkStatser); ok {
			reg.SetStreamSink(func() metrics.StreamSinkStats {
				c, p, d := sp.Stats()
				return metrics.StreamSinkStats{Consumers: c, FramesPushed: p, FramesDropped: d}
			})
		}
		go func() {
			if err := ssink.Run(ctx); err != nil {
				logger.Printf("stream sink: %v", err)
			}
		}()
	}

	// Graceful shutdown on SIGINT/SIGTERM. When streaming is on, the stream
	// Service's coordinator runs the REST server's Shutdown after ctx.Done (an
	// ordered drain); otherwise the REST server shuts down directly. Streaming
	// components close their own listeners on ctx.Done either way.
	restShutdown := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}
	if streamingOn {
		go streamSvc.ShutdownOn(ctx, func() {
			streamSvc.FlushAsOf()   // write the latest AsOf marks before exit (throttle defers most)
			streamSvc.FlushReplay() // write the latest replay ring before exit (throttle defers most)
			restShutdown()
		})
	} else {
		go func() {
			<-ctx.Done()
			restShutdown()
		}()
	}

	logger.Printf("redswitchboard listening on %s (source %s -> sink %s, %d vehicle(s))",
		cfg.ListenAddr, cfg.Source, cfg.Sink, len(identities))
	logger.Printf("metrics exposed at %s/metrics", cfg.ListenAddr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}
