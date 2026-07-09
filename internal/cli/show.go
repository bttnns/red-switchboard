package cli

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bttnns/red-switchboard/internal/mock"
	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/poll"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"gopkg.in/yaml.v3"
)

// newShowCmd prints a protocol's API wire shape (the JSON a consumer would
// receive), choosing its data source by precedence: a running server (--server),
// else the real source if its creds are available, else the protocol-agnostic
// mock. The shape is rendered by feeding a sink.Provider through the protocol's
// SINK and sampling its primary vehicle-data endpoint (via sink.Sampler), so no
// per-protocol routes are hardcoded here. A one-line note on stderr reports which
// mode and endpoint produced the output.
func newShowCmd() *cobra.Command {
	var server, creds, vehicleID, scenario string
	cmd := &cobra.Command{
		Use:     "show <protocol>",
		Short:   "render a protocol's API shape (--server, else live creds, else mock)",
		GroupID: groupInspect,
		Long: "Print a protocol's API wire shape. Data source precedence:\n" +
			"  1. --server set: GET the running server's sink endpoint (live).\n" +
			"  2. else creds available: open the real source and render its data.\n" +
			"  3. else: render synthetic mock data.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			protocol := ""
			if len(args) == 1 {
				protocol = args[0]
			}
			if protocol == "" {
				return fmt.Errorf("show: missing protocol argument (registered sinks: %s)", strings.Join(sink.Names(), ", "))
			}
			if !knownSink(protocol) {
				return fmt.Errorf("show: unknown protocol %q (registered sinks: %s)", protocol, strings.Join(sink.Names(), ", "))
			}

			logger := log.New(io.Discard, "", 0)
			if server != "" { // 1. live wire from a running server
				return showFromServer(protocol, server, vehicleID, logger)
			}
			if body, ok := showFromSource(protocol, creds, vehicleID, logger); ok { // 2. real source
				return printIndentedJSON(body)
			}
			return showFromMock(protocol, scenario, vehicleID, logger) // 3. mock
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "address of a running server (host:port); fetch live wire from it")
	cmd.Flags().StringVar(&creds, "creds", "", "creds file for the protocol's source (default: the source's own default path)")
	cmd.Flags().StringVar(&vehicleID, "vehicle", "", "canonical vehicle id to render (default: the first vehicle)")
	cmd.Flags().StringVar(&scenario, "scenario", mock.ScenarioIdle, "mock scenario when rendering mock data (idle|driving|charging)")
	return cmd
}

// knownSink reports whether name is a registered sink plugin.
func knownSink(name string) bool {
	for _, n := range sink.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// showFromServer builds the sink's sample request and sends it to a running
// server, printing the live wire it returns.
func showFromServer(protocol, addr, vehicleID string, logger *log.Logger) error {
	// Build a sink over an empty provider just to construct the sample request;
	// the server (not this provider) supplies the data.
	out, err := sink.Open(protocol, nil, emptyProvider{}, logger)
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	sampler, ok := out.(sink.Sampler)
	if !ok {
		return fmt.Errorf("show: %s sink does not support sampling", protocol)
	}
	// For server mode the canonical id must address a vehicle the SERVER knows,
	// so a blank id is left blank (let the server pick) only when the sampler can
	// produce a request without it. Tesla needs a concrete id, so require one
	// from --vehicle when the local empty provider cannot resolve it.
	req, err := sampler.SampleRequest(vehicleID)
	if err != nil {
		return fmt.Errorf("show: cannot build sample request (try --vehicle <id>): %w", err)
	}

	target := "http://" + addr + req.URL.Path
	req.URL.Scheme = "http"
	req.URL.Host = addr
	req.RequestURI = ""

	fmt.Fprintf(os.Stderr, "show: mode=server endpoint=%s %s\n", req.Method, target)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w (is the server running?)", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}
	return printIndentedJSON(body)
}

// showFromSource opens the real source for protocol, polls one vehicle, and
// renders the snapshot through the protocol's sink in-process. It reports ok=false
// (so the caller falls back to mock) whenever creds are missing or the source
// cannot be opened/listed, rather than erroring out.
func showFromSource(protocol, credsPath, vehicleID string, logger *log.Logger) ([]byte, bool) {
	var settings *yaml.Node
	if credsPath != "" {
		var n yaml.Node
		if err := n.Encode(map[string]string{"creds_file": credsPath}); err != nil {
			return nil, false
		}
		settings = &n
	}

	src, err := source.Open(protocol, settings, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "show: no live creds for %s (%v); using mock\n", protocol, err)
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ids, err := src.Vehicles(ctx)
	if err != nil {
		if source.IsUnauthenticated(err) {
			fmt.Fprintf(os.Stderr, "show: %s creds present but unauthenticated; using mock\n", protocol)
		} else {
			fmt.Fprintf(os.Stderr, "show: %s source unavailable (%v); using mock\n", protocol, err)
		}
		return nil, false
	}
	if len(ids) == 0 {
		fmt.Fprintf(os.Stderr, "show: %s source reported no vehicles; using mock\n", protocol)
		return nil, false
	}

	id := ids[0]
	if vehicleID != "" {
		found := false
		for _, v := range ids {
			if v.ID == vehicleID {
				id, found = v, true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "show: vehicle %q not found in %s account; using first\n", vehicleID, protocol)
		}
	}

	snap, err := src.Poll(ctx, id.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "show: %s poll failed (%v); using mock\n", protocol, err)
		return nil, false
	}

	prov := &singleProvider{id: id, snap: snap}
	body, endpoint, err := renderWire(protocol, prov, id.ID, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "show: rendering %s wire failed (%v); using mock\n", protocol, err)
		return nil, false
	}
	fmt.Fprintf(os.Stderr, "show: mode=live-creds vehicle=%s endpoint=%s\n", id.ID, endpoint)
	return body, true
}

// showFromMock builds a single-vehicle mock engine and renders it through the
// protocol's sink in-process.
func showFromMock(protocol, scenario, vehicleID string, logger *log.Logger) error {
	specs, err := parseMockVehicles("RIV0MOCK00000001/7FCTGAAA0NN000001/Mock Vehicle/R1T")
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	engine := mock.NewEngine(specs, 1, 0, logger)
	if scenario != mock.ScenarioIdle {
		if !engine.SetScenario(scenario, "") {
			return fmt.Errorf("show: unknown scenario %q", scenario)
		}
	}
	// The engine builds a snapshot lazily on Latest from its current scenario
	// state, so no clock advance is needed to render a representative payload.
	canon := vehicleID
	if canon == "" {
		if vs := engine.Vehicles(); len(vs) > 0 {
			canon = vs[0].ID
		}
	}

	body, endpoint, err := renderWire(protocol, engine, canon, logger)
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	fmt.Fprintf(os.Stderr, "show: mode=mock scenario=%s endpoint=%s\n", scenario, endpoint)
	return printIndentedJSON(body)
}

// renderWire opens the protocol's sink over prov, builds its sample request via
// sink.Sampler, serves it in-process, and returns the response body plus the
// sampled endpoint (method + path) for the stderr note.
func renderWire(protocol string, prov sink.Provider, canonID string, logger *log.Logger) (body []byte, endpoint string, err error) {
	out, err := sink.Open(protocol, nil, prov, logger)
	if err != nil {
		return nil, "", err
	}
	sampler, ok := out.(sink.Sampler)
	if !ok {
		return nil, "", fmt.Errorf("%s sink does not support sampling", protocol)
	}
	handler, err := out.Handler()
	if err != nil {
		return nil, "", err
	}
	req, err := sampler.SampleRequest(canonID)
	if err != nil {
		return nil, "", err
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, "", fmt.Errorf("sink returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes(), fmt.Sprintf("%s %s", req.Method, req.URL.Path), nil
}

// singleProvider is a sink.Provider over a single polled snapshot, used by the
// live-creds path (no poll cache is running).
type singleProvider struct {
	id   vehicle.Identity
	snap vehicle.Snapshot
}

func (p *singleProvider) Vehicles() []vehicle.Identity { return []vehicle.Identity{p.id} }
func (p *singleProvider) Latest(id string) vehicle.Snapshot {
	if id == p.id.ID {
		return p.snap
	}
	return vehicle.Snapshot{}
}
func (p *singleProvider) Stats(id string) poll.Stats { return poll.Stats{} }

// emptyProvider is a sink.Provider with no vehicles, used in --server mode where
// the running server (not this provider) holds the data; the local sink exists
// only to build the sample request.
type emptyProvider struct{}

func (emptyProvider) Vehicles() []vehicle.Identity   { return nil }
func (emptyProvider) Latest(string) vehicle.Snapshot { return vehicle.Snapshot{} }
func (emptyProvider) Stats(string) poll.Stats        { return poll.Stats{} }
