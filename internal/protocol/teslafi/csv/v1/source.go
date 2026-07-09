package v1

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/go-resty/resty/v2"
	"github.com/gocarina/gocsv"
	"gopkg.in/yaml.v3"
)

// SourcePluginName is the registry key for the TeslaFi-CSV input source.
const SourcePluginName = "teslafi-csv-v1"

func init() { source.Register(SourcePluginName, newSource) }

// SourceSettings is the sources.teslafi-csv config sub-block. Exactly one of
// path (a directory of TeslaFi*.csv files) or url (an HTTP endpoint serving the
// CSV, e.g. the teslafi-csv sink's /export.csv) supplies the recording.
// time_scale accelerates replay (1 = lifelike); timezone parses the naive Dates.
type SourceSettings struct {
	Path      string  `yaml:"path"`
	URL       string  `yaml:"url"`
	Timezone  string  `yaml:"timezone"`
	TimeScale float64 `yaml:"time_scale"`
}

// csvSource replays a recorded TeslaFi CSV out as canonical snapshots. The
// CSV's Date column IS the timeline: a per-vehicle cursor advances from the
// earliest Date by (wall-clock since start * time_scale), and each replayed
// Snapshot keeps its ORIGINAL recorded timestamp (no re-anchoring to now).
type csvSource struct {
	loc       *time.Location
	scale     float64
	start     time.Time
	firstDate time.Time

	order  []string                    // canonID (vehicle_id) order
	rows   map[string][]Row            // canonID -> rows sorted by Date
	idents map[string]vehicle.Identity // canonID -> identity
	logger *log.Logger
}

// newSource is the source.Factory for "teslafi-csv-v1".
func newSource(node *yaml.Node, logger *log.Logger) (source.Source, error) {
	if logger == nil {
		logger = log.Default()
	}
	var s SourceSettings
	if node != nil {
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("teslafi-csv source: decode settings: %w", err)
		}
	}
	if (s.Path == "") == (s.URL == "") {
		return nil, fmt.Errorf("teslafi-csv source: set exactly one of path or url")
	}
	loc, err := loadLocation(s.Timezone)
	if err != nil {
		return nil, fmt.Errorf("teslafi-csv source: %w", err)
	}
	scale := s.TimeScale
	if scale <= 0 {
		scale = 1
	}

	rows, err := loadRows(s, loc)
	if err != nil {
		return nil, err
	}
	src := &csvSource{
		loc:    loc,
		scale:  scale,
		start:  time.Now(),
		rows:   map[string][]Row{},
		idents: map[string]vehicle.Identity{},
		logger: logger,
	}
	src.index(rows)
	return src, nil
}

// loadRows reads the recording from a local dir or, via resty, an HTTP URL.
func loadRows(s SourceSettings, loc *time.Location) ([]Row, error) {
	if s.Path != "" {
		return readDir(s.Path, loc)
	}
	resp, err := resty.New().R().Get(s.URL)
	if err != nil {
		return nil, fmt.Errorf("teslafi-csv source: fetch %s: %w", s.URL, err)
	}
	if resp.IsError() {
		return nil, fmt.Errorf("teslafi-csv source: fetch %s: status %d", s.URL, resp.StatusCode())
	}
	var rows []Row
	if err := gocsv.UnmarshalBytes(resp.Body(), &rows); err != nil {
		return nil, fmt.Errorf("teslafi-csv source: parse %s: %w", s.URL, err)
	}
	return rows, nil
}

// index groups rows by vehicle, sorts each by Date, and records the earliest
// Date (the replay origin) and per-vehicle identities.
func (s *csvSource) index(rows []Row) {
	for _, r := range rows {
		id := strconv.Itoa(r.VehicleID)
		s.rows[id] = append(s.rows[id], r)
		if _, ok := s.idents[id]; !ok {
			s.order = append(s.order, id)
			name := r.DisplayName
			if name == "" {
				name = r.VehicleName
			}
			s.idents[id] = vehicle.Identity{ID: id, VIN: r.VIN, DisplayName: name, Make: "TeslaFi"}
		}
	}
	sort.Strings(s.order)
	for id := range s.rows {
		sort.SliceStable(s.rows[id], func(i, j int) bool {
			ti, _ := s.rows[id][i].parsedDate(s.loc)
			tj, _ := s.rows[id][j].parsedDate(s.loc)
			return ti.Before(tj)
		})
	}
	for _, rs := range s.rows {
		if len(rs) == 0 {
			continue
		}
		if t, err := rs[0].parsedDate(s.loc); err == nil {
			if s.firstDate.IsZero() || t.Before(s.firstDate) {
				s.firstDate = t
			}
		}
	}
}

// Name implements source.Source.
func (s *csvSource) Name() string { return SourcePluginName }

// Vehicles implements source.Source.
func (s *csvSource) Vehicles(_ context.Context) ([]vehicle.Identity, error) {
	out := make([]vehicle.Identity, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.idents[id])
	}
	return out, nil
}

// Poll implements source.Source: return the recorded snapshot at the current
// replay cursor for one vehicle. The cursor = firstDate + elapsed*scale; the
// returned snapshot keeps its original recorded timestamp.
func (s *csvSource) Poll(_ context.Context, id string) (vehicle.Snapshot, error) {
	rows := s.rows[id]
	if len(rows) == 0 {
		return vehicle.Snapshot{}, fmt.Errorf("teslafi-csv source: unknown vehicle %q", id)
	}
	cursor := s.firstDate.Add(time.Duration(float64(time.Since(s.start)) * s.scale))
	idx := 0
	for i, r := range rows {
		t, err := r.parsedDate(s.loc)
		if err != nil {
			continue
		}
		if t.After(cursor) {
			break
		}
		idx = i
	}
	return rows[idx].toSnapshot(s.loc), nil
}
