package v1

import (
	"fmt"
	"iter"
	"log"
	"net/http"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gocarina/gocsv"
	"gopkg.in/yaml.v3"
)

// SinkPluginName is the registry key for the TeslaFi-CSV output sink.
const SinkPluginName = "teslafi-csv-v1"

func init() { sink.Register(SinkPluginName, newSink) }

// SinkSettings is the sinks.teslafi-csv config sub-block. export_dir is where
// ExportHistory writes the TeslaFi*.csv files (this must be the host path that is
// mounted into the TeslaMate container's IMPORT_DIR). timezone names the IANA
// zone the naive `Date` cells are written in (and that the TeslaMate import must
// be told); empty means the host local zone.
type SinkSettings struct {
	ExportDir string `yaml:"export_dir"`
	Timezone  string `yaml:"timezone"`
}

// csvSink renders canonical snapshots to TeslaFi CSV. ExportHistory writes the
// historical batch to export_dir (for TeslaMate's importer); the chi Handler
// serves current state as CSV over HTTP.
type csvSink struct {
	prov      sink.Provider
	loc       *time.Location
	exportDir string
	logger    *log.Logger
}

// newSink is the sink.Factory for "teslafi-csv-v1".
func newSink(node *yaml.Node, prov sink.Provider, logger *log.Logger) (sink.Sink, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SinkSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("teslafi-csv sink: decode settings: %w", err)
		}
	}
	loc, err := loadLocation(s.Timezone)
	if err != nil {
		return nil, fmt.Errorf("teslafi-csv sink: %w", err)
	}
	return NewExportSink(prov, loc, s.ExportDir, logger), nil
}

// NewExportSink builds the sink directly (used by the `mock --since` generator,
// which supplies the timezone and export dir as args rather than via YAML).
func NewExportSink(prov sink.Provider, loc *time.Location, exportDir string, logger *log.Logger) sink.Sink {
	if loc == nil {
		loc = time.Local
	}
	if logger == nil {
		logger = log.Default()
	}
	return &csvSink{prov: prov, loc: loc, exportDir: exportDir, logger: logger}
}

func loadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", tz, err)
	}
	return loc, nil
}

// Name implements sink.Sink.
func (s *csvSink) Name() string { return SinkPluginName }

// Handler implements sink.Sink: a chi router that serves the CURRENT canonical
// state of every vehicle as a TeslaFi CSV body (one row per car) at GET
// /export.csv. (Historical batches go to files via ExportHistory; this live
// surface lets the teslafi-csv SOURCE fetch a recording over HTTP via resty.)
func (s *csvSink) Handler() (http.Handler, error) {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/export.csv", s.handleExport)
	return r, nil
}

func (s *csvSink) handleExport(w http.ResponseWriter, _ *http.Request) {
	rows := s.currentRows()
	body, err := gocsv.MarshalString(&rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	_, _ = w.Write([]byte(body))
}

// currentRows renders one Row per vehicle from the Provider's latest snapshot.
func (s *csvSink) currentRows() []Row {
	var rows []Row
	for i, v := range s.prov.Vehicles() {
		snap := s.prov.Latest(v.ID)
		if snap.State == nil {
			continue
		}
		rows = append(rows, toRow(snap, i+1, vehicleIdent{name: v.DisplayName, vin: v.VIN}, s.loc))
	}
	return rows
}

// ExportHistory implements sink.Exporter: write the canonical snapshot stream to
// monthly TeslaFi CSV files in the configured export_dir. Each vehicle gets a
// stable vehicle_id (its 1-based position in the Provider's vehicle list) so
// multi-car imports split.
func (s *csvSink) ExportHistory(samples iter.Seq[sink.HistorySample]) error {
	if s.exportDir == "" {
		return fmt.Errorf("teslafi-csv: export_dir is not configured")
	}
	type carInfo struct {
		id    int
		ident vehicleIdent
	}
	cars := map[string]carInfo{}
	for i, v := range s.prov.Vehicles() {
		cars[v.ID] = carInfo{id: i + 1, ident: vehicleIdent{name: v.DisplayName, vin: v.VIN}}
	}

	var rows []Row
	count := 0
	for hs := range samples {
		if hs.Snap.State == nil {
			continue
		}
		c, ok := cars[hs.CanonID]
		if !ok {
			c = carInfo{id: len(cars) + 1, ident: vehicleIdent{name: hs.CanonID}}
			cars[hs.CanonID] = c
		}
		rows = append(rows, toRow(hs.Snap, c.id, c.ident, s.loc))
		count++
	}
	if err := writeMonthly(s.exportDir, rows, s.loc); err != nil {
		return err
	}
	s.logger.Printf("teslafi-csv: wrote %d rows for %d vehicle(s) to %s", count, len(cars), s.exportDir)
	return nil
}

// vehicleIdent is the identity fields the CSV carries per car.
type vehicleIdent struct {
	name string
	vin  string
}
