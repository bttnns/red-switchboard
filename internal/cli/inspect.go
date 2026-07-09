package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// inspectOpts holds the flags shared by status/stats/cache.
type inspectOpts struct {
	addr     string
	format   string
	id       int64
	watch    bool
	interval time.Duration
}

// addInspectFlags wires the common inspect flags (id only where meaningful).
func addInspectFlags(cmd *cobra.Command, o *inspectOpts, withID bool) {
	cmd.Flags().StringVar(&o.addr, "addr", "localhost:4000", "address of the running server")
	cmd.Flags().StringVar(&o.format, "format", "table", "output format: table|json")
	if withID {
		cmd.Flags().Int64Var(&o.id, "id", 0, "vehicle id (defaults to the first vehicle)")
	}
	cmd.Flags().BoolVar(&o.watch, "watch", false, "poll repeatedly until interrupted")
	cmd.Flags().DurationVar(&o.interval, "interval", 2*time.Second, "poll interval for --watch")
}

// newMetricsCmd fetches a running server's Prometheus /metrics endpoint and
// prints the raw exposition verbatim (plain text, not JSON).
func newMetricsCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:     "metrics",
		Short:   "fetch a running server's Prometheus /metrics exposition",
		GroupID: groupInspect,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			body, err := httpGet(addr, "/metrics")
			if err != nil {
				return err
			}
			fmt.Print(string(body))
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "localhost:4000", "address of the running server")
	return cmd
}

// newStatusCmd queries a running server's /status (per-vehicle freshness/health).
func newStatusCmd() *cobra.Command {
	var o inspectOpts
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "query a running server for per-vehicle freshness/health",
		GroupID: groupInspect,
		Args:    cobra.NoArgs,
		RunE:    func(*cobra.Command, []string) error { return inspect("status", o) },
	}
	addInspectFlags(cmd, &o, false)
	return cmd
}

// newStatsCmd queries a running server's /stats (rate-decoupling metrics).
func newStatsCmd() *cobra.Command {
	var o inspectOpts
	cmd := &cobra.Command{
		Use:     "stats",
		Short:   "query a running server for rate-decoupling stats",
		GroupID: groupInspect,
		Args:    cobra.NoArgs,
		RunE:    func(*cobra.Command, []string) error { return inspect("stats", o) },
	}
	addInspectFlags(cmd, &o, false)
	return cmd
}

// newCacheCmd views a running server's cache: `cache show` = the served snapshot
// (vehicle_data), `cache raw` = the source-native extras dump.
func newCacheCmd() *cobra.Command {
	var o inspectOpts
	cmd := &cobra.Command{
		Use:     "cache [show|raw]",
		Short:   "view a running server's cache: cache [show|raw] [--id]",
		GroupID: groupInspect,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			mode := "show"
			if len(args) == 1 {
				if args[0] != "show" && args[0] != "raw" {
					return fmt.Errorf("cache: want 'show' or 'raw', got %q", args[0])
				}
				mode = args[0]
			}
			return inspect("cache:"+mode, o)
		},
	}
	addInspectFlags(cmd, &o, true)
	return cmd
}

// inspect runs one inspect verb once, or repeatedly under --watch.
func inspect(verb string, o inspectOpts) error {
	run := func() error { return inspectOnce(verb, o.addr, o.format, o.id) }
	if !o.watch {
		return run()
	}
	for {
		fmt.Print("\033[H\033[2J") // clear screen
		if err := run(); err != nil {
			fmt.Printf("error: %v\n", err)
		}
		time.Sleep(o.interval)
	}
}

func inspectOnce(verb, addr, format string, id int64) error {
	var path string
	jsonOnly := false
	switch verb {
	case "status":
		path = "/status"
	case "stats":
		path = "/stats"
	case "cache:show":
		if id == 0 {
			var err error
			if id, err = firstVehicleID(addr); err != nil {
				return err
			}
		}
		path = fmt.Sprintf("/api/1/vehicles/%d/vehicle_data", id)
		jsonOnly = true
	case "cache:raw":
		if id == 0 {
			var err error
			if id, err = firstVehicleID(addr); err != nil {
				return err
			}
		}
		path = fmt.Sprintf("/api/1/vehicles/%d/source_extras", id)
		jsonOnly = true
	default:
		return fmt.Errorf("unknown inspect verb %q", verb)
	}

	body, err := httpGet(addr, path)
	if err != nil {
		return err
	}
	if jsonOnly || format == "json" {
		return printIndentedJSON(body)
	}
	switch verb {
	case "status":
		return printStatusTable(body)
	case "stats":
		return printStatsTable(body)
	}
	return printIndentedJSON(body)
}

func firstVehicleID(addr string) (int64, error) {
	body, err := httpGet(addr, "/status")
	if err != nil {
		return 0, err
	}
	var s struct {
		Vehicles []struct {
			ID int64 `json:"id"`
		} `json:"vehicles"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return 0, err
	}
	if len(s.Vehicles) == 0 {
		return 0, fmt.Errorf("no vehicles known to the server yet")
	}
	return s.Vehicles[0].ID, nil
}

func httpGet(addr, path string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return nil, fmt.Errorf("contacting %s: %w (is the server running?)", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func printIndentedJSON(body []byte) error {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Println(string(body))
		return nil
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
	return nil
}

func printStatusTable(body []byte) error {
	var s struct {
		Vehicles []struct {
			ID          int64   `json:"id"`
			VIN         string  `json:"vin"`
			DisplayName string  `json:"display_name"`
			State       string  `json:"state"`
			AgeSeconds  float64 `json:"age_seconds"`
			Stale       bool    `json:"stale"`
			LastError   string  `json:"last_error"`
			NeedsReauth bool    `json:"needs_reauth"`
			PollSuccess int64   `json:"poll_success"`
			PollErrors  int64   `json:"poll_errors"`
		} `json:"vehicles"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return err
	}
	fmt.Printf("%-12s %-18s %-8s %8s %-6s %6s %6s  %s\n",
		"NAME", "VIN", "STATE", "AGE(s)", "STALE", "OK", "ERR", "LAST_ERROR")
	for _, v := range s.Vehicles {
		fmt.Printf("%-12s %-18s %-8s %8.0f %-6t %6d %6d  %s\n",
			truncate(v.DisplayName, 12), truncate(v.VIN, 18), v.State, v.AgeSeconds,
			v.Stale, v.PollSuccess, v.PollErrors, v.LastError)
	}
	return nil
}

func printStatsTable(body []byte) error {
	var s struct {
		UptimeSeconds  float64          `json:"uptime_seconds"`
		VehiclesKnown  int              `json:"vehicles_known"`
		Requests       map[string]int64 `json:"requests"`
		RivianPolls    int64            `json:"rivian_polls"`
		RivianChanges  int64            `json:"rivian_changes"`
		RivianErrors   int64            `json:"rivian_errors"`
		RateLimited    int64            `json:"rate_limited"`
		Reads          int64            `json:"reads"`
		ReadsPerPoll   float64          `json:"reads_per_poll"`
		ReadsPerChange float64          `json:"reads_per_change"`
		ChangeRatio    float64          `json:"change_ratio"`
		RatesPerMin    struct {
			ConsumerReads float64 `json:"consumer_reads"`
			RivianPolls   float64 `json:"rivian_polls"`
			DataChanges   float64 `json:"data_changes"`
		} `json:"rates_per_min"`
		Resources struct {
			Goroutines int    `json:"goroutines"`
			HeapAlloc  uint64 `json:"heap_alloc"`
			Sys        uint64 `json:"sys"`
			NumGC      uint32 `json:"num_gc"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return err
	}
	fmt.Printf("uptime          %s\n", time.Duration(s.UptimeSeconds*float64(time.Second)).Round(time.Second))
	fmt.Printf("vehicles known  %d\n", s.VehiclesKnown)
	fmt.Printf("source polls    %d (changed %d, errors %d, rate-limited %d)\n", s.RivianPolls, s.RivianChanges, s.RivianErrors, s.RateLimited)
	fmt.Printf("consumer reads  %d\n", s.Reads)
	fmt.Printf("decoupling      %.1f reads/poll, %.1f reads/change, %.0f%% of polls saw new data\n",
		s.ReadsPerPoll, s.ReadsPerChange, s.ChangeRatio*100)
	fmt.Printf("rates/min       reads %.1f, polls %.1f, data-changes %.1f\n",
		s.RatesPerMin.ConsumerReads, s.RatesPerMin.RivianPolls, s.RatesPerMin.DataChanges)
	fmt.Printf("goroutines      %d\n", s.Resources.Goroutines)
	fmt.Printf("heap alloc      %.1f MiB (sys %.1f MiB, gc %d)\n",
		float64(s.Resources.HeapAlloc)/1024/1024, float64(s.Resources.Sys)/1024/1024, s.Resources.NumGC)
	fmt.Printf("requests:\n")
	for route, n := range s.Requests {
		fmt.Printf("  %-44s %d\n", route, n)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
