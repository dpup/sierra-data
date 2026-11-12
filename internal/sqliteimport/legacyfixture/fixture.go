// Package legacyfixture builds realistic legacy (schema v14) SQLite Grid
// stores for the importer's tests and timing runs, without the legacy Go
// package: the schema is the legacy ladder's SQL, verbatim from the
// legacy-efs-final tag (ladder/*.sql), applied through the SQLite driver the
// way the legacy store's migrate did, and the rows follow the legacy store's
// write rules (one revision snapshot per content change or transition,
// events holding the latest blob plus in-place refreshes, the lite copy, the
// event->place join, the R*Tree bbox, mesh measurements with a compaction
// watermark).
//
// Test support only; nothing in a server build imports it.
package legacyfixture

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	placesdata "github.com/sierragridteam/sierra-data/data/places"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"github.com/sierragridteam/sierra-data/internal/store"

	_ "modernc.org/sqlite"
)

//go:embed ladder/*.sql
var ladderFS embed.FS

// Spec sizes a fixture. Every count is approximate except where noted.
type Spec struct {
	Events            int // events (exact)
	MaxRevisions      int // revisions per event are 1..MaxRevisions, skewed low; mesh and burn events go higher
	PerimeterVertices int // upper bound for a wildfire/power polygon ring
	MeshNodes         int
	MeshObservations  int // rows (exact), ids with gaps
	MeshTelemetry     int // rows (approximately)
	LinkRollups       int
	RoadSamples       int // rows (approximately)
	Seed              uint64
}

// Small is the fixture the tests use: every shape at least once, built in
// well under a second.
func Small() Spec {
	return Spec{Events: 40, MaxRevisions: 5, PerimeterVertices: 60, MeshNodes: 6,
		MeshObservations: 200, MeshTelemetry: 30, LinkRollups: 20, RoadSamples: 60, Seed: 1}
}

// ProductionScale approximates the production grid.db (about 150 MiB,
// dominated by event_revisions and mesh_observations).
func ProductionScale() Spec {
	return Spec{Events: 6000, MaxRevisions: 8, PerimeterVertices: 700, MeshNodes: 450,
		MeshObservations: 200000, MeshTelemetry: 40000, LinkRollups: 30000, RoadSamples: 70000, Seed: 7}
}

// Summary describes what Build wrote, for assertions.
type Summary struct {
	Rows             map[string]int64
	GeoEventID       string // an event with geometry (bbox columns derive from it)
	NoGeoEventID     string // an event without geometry
	ResolvedEventID  string // an event whose last revision is a transition
	ActiveEventID    string // an ACTIVE road incident from source "chp"
	MaxObservationID int64
	Watermark        int64
	SourceIDs        []string
}

// CreateSchema applies the legacy ladder v1..v14 to db exactly as the legacy
// store's migrate did: each version in its own transaction, recorded in
// schema_migrations (version, applied_at).
func CreateSchema(ctx context.Context, db *sql.DB, appliedAt time.Time) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	files, err := fs.Glob(ladderFS, "ladder/v*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for n, name := range files {
		ddl, err := ladderFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(ddl)); err != nil {
			tx.Rollback()
			return fmt.Errorf("legacyfixture: applying %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			n+1, appliedAt.Add(time.Duration(n)*24*time.Hour).Unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Build writes a new v14 SQLite store at path.
func Build(ctx context.Context, path string, spec Spec) (*Summary, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("legacyfixture: %s exists", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(TRUNCATE)&_pragma=synchronous(OFF)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	b := &builder{spec: spec, rng: rand.New(rand.NewPCG(spec.Seed, spec.Seed^0x5eed)),
		base: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), sum: &Summary{Rows: map[string]int64{}}}
	if err := CreateSchema(ctx, db, b.base.Add(-60*24*time.Hour)); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	b.tx = tx
	steps := []func() error{b.sources, b.places, b.events, b.mesh, b.burn, b.roads}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return b.sum, nil
}

type builder struct {
	spec     Spec
	rng      *rand.Rand
	base     time.Time
	tx       *sql.Tx
	sum      *Summary
	counties []county
	geoRowid int64
	meshKeys []string
}

type county struct {
	id   string
	geom *geojson.Geom
}

func (b *builder) exec(table, q string, args ...any) error {
	if _, err := b.tx.Exec(q, args...); err != nil {
		return fmt.Errorf("legacyfixture: insert %s: %w", table, err)
	}
	b.sum.Rows[table]++
	return nil
}

type sourceRow struct {
	id, name, disappearance string
	kind                    int
	poll                    int64
	expire                  any
}

var sources = []sourceRow{
	{"calfire", "CAL FIRE", "resolve", 1, 300, nil},
	{"firis", "FIRIS", "expire", 1, 600, int64(172800)},
	{"caloes", "Cal OES", "resolve", 1, 300, nil},
	{"nws", "National Weather Service", "expire", 1, 300, int64(86400)},
	{"usgs", "USGS", "expire", 1, 300, int64(604800)},
	{"chp", "CHP", "resolve", 1, 120, nil},
	{"caltrans", "Caltrans", "resolve", 1, 900, nil},
	{"pge", "PG&E", "resolve", 1, 300, nil},
	{"psps", "PG&E PSPS", "resolve", 1, 900, nil},
	{"meshcore", "MeshCore", "expire", 1, 60, int64(7200)},
	{"calfire-burn", "CAL FIRE burn permits", "resolve", 1, 43200, nil},
	{"burnline", "County burn line", "resolve", 1, 3600, nil},
	{"alanpi", "alanpi", "resolve", 2, 900, nil},
	{"burn-line", "burn-line", "resolve", 2, 3600, nil},
}

func (b *builder) sources() error {
	for n, s := range sources {
		var lastErr string
		var stale any
		status := 1
		if n%5 == 4 {
			lastErr, status = "fetch: context deadline exceeded", 2
			stale = int64(s.poll * 4)
		}
		at := b.base.Add(90 * 24 * time.Hour).Unix()
		if err := b.exec("sources", `INSERT INTO sources (id, name, attribution, homepage_url, kind,
			poll_interval_seconds, stale_after_seconds, expire_after_seconds, last_success_at,
			last_attempt_at, last_error, status, disappearance) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.id, s.name, "Data: "+s.name, "https://example.org/"+s.id, s.kind, s.poll, stale, s.expire,
			at-30, at, lastErr, status, s.disappearance); err != nil {
			return err
		}
		b.sum.SourceIDs = append(b.sum.SourceIDs, s.id)
	}
	return nil
}

func geometry(raw []byte) (*gridv1.Geometry, *geojson.Geom, error) {
	g, err := geojson.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	minLat, minLng, maxLat, maxLng := g.Bbox()
	cLat, cLng := g.Centroid()
	return &gridv1.Geometry{Geojson: raw,
		Bbox:     &gridv1.BoundingBox{MinLat: minLat, MinLng: minLng, MaxLat: maxLat, MaxLng: maxLng},
		Centroid: &gridv1.LatLng{Lat: cLat, Lng: cLng}}, g, nil
}

func (b *builder) insertPlace(p *gridv1.Place) error {
	blob, err := proto.Marshal(p)
	if err != nil {
		return err
	}
	var parent any
	if p.GetParentId() != "" {
		parent = p.GetParentId()
	}
	return b.exec("places", `INSERT INTO places (id, kind, name, slug, parent_id, proto) VALUES (?,?,?,?,?,?)`,
		p.GetId(), int32(p.GetKind()), p.GetName(), p.GetSlug(), parent, blob)
}

// places writes the real county polygons and corridors (data/places) plus
// towns as points, parented to their county.
func (b *builder) places() error {
	var fc struct {
		Features []struct {
			Properties struct {
				Name string `json:"NAME"`
				ID   string `json:"id"`
				N    string `json:"name"`
			} `json:"properties"`
			Geometry jsonRaw `json:"geometry"`
		} `json:"features"`
	}
	if err := unmarshalJSON(placesdata.CountiesGeoJSON, &fc); err != nil {
		return err
	}
	for _, f := range fc.Features {
		geom, g, err := geometry(f.Geometry)
		if err != nil {
			return err
		}
		slug := strings.ReplaceAll(strings.ToLower(f.Properties.Name), " ", "-")
		id := "county:" + slug
		b.counties = append(b.counties, county{id: id, geom: g})
		if err := b.insertPlace(&gridv1.Place{Id: id, Kind: gridv1.PlaceKind_COUNTY,
			Name: f.Properties.Name, Slug: slug, Geometry: geom}); err != nil {
			return err
		}
	}
	fc.Features = nil
	if err := unmarshalJSON(placesdata.CorridorsGeoJSON, &fc); err != nil {
		return err
	}
	for _, f := range fc.Features {
		geom, _, err := geometry(f.Geometry)
		if err != nil {
			return err
		}
		if err := b.insertPlace(&gridv1.Place{Id: "corridor:" + f.Properties.ID, Kind: gridv1.PlaceKind_CORRIDOR,
			Name: f.Properties.N, Slug: f.Properties.ID, Geometry: geom}); err != nil {
			return err
		}
	}
	towns := []struct {
		name     string
		lat, lng float64
	}{{"Arnold", 38.2555, -120.3513}, {"Murphys", 38.1377, -120.4605}, {"Sonora", 37.9841, -120.3822},
		{"Angels Camp", 38.0683, -120.5397}, {"Columbia", 38.0363, -120.4013}}
	for _, t := range towns {
		raw := []byte(fmt.Sprintf(`{"type":"Point","coordinates":[%.4f,%.4f]}`, t.lng, t.lat))
		geom, _, err := geometry(raw)
		if err != nil {
			return err
		}
		slug := strings.ReplaceAll(strings.ToLower(t.name), " ", "-")
		if err := b.insertPlace(&gridv1.Place{Id: "town:" + slug, Kind: gridv1.PlaceKind_TOWN, Name: t.name,
			Slug: slug, ParentId: b.countyAt(t.lat, t.lng), Geometry: geom}); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) countyAt(lat, lng float64) string {
	for _, c := range b.counties {
		if geojson.PointInGeometry(lat, lng, c.geom) {
			return c.id
		}
	}
	return ""
}

// point is a random location in the foothills (Calaveras/Tuolumne).
func (b *builder) point() (lat, lng float64) {
	return 37.85 + b.rng.Float64()*0.6, -120.95 + b.rng.Float64()*0.9
}

// polygon is a closed ring of up to max vertices around a point.
func (b *builder) polygon(lat, lng, radius float64, maxVertices int) []byte {
	n := 8 + b.rng.IntN(max(maxVertices-8, 1))
	var sb strings.Builder
	sb.WriteString(`{"type":"Polygon","coordinates":[[`)
	for k := 0; k <= n; k++ {
		a := 2 * math.Pi * float64(k%n) / float64(n)
		r := radius * (0.7 + 0.3*math.Sin(float64(k)*1.7))
		if k > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "[%.5f,%.5f]", lng+r*math.Cos(a), lat+r*math.Sin(a))
	}
	sb.WriteString(`]]}`)
	return []byte(sb.String())
}

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func (b *builder) hex(n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	for k := range out {
		out[k] = digits[b.rng.IntN(16)]
	}
	return string(out)
}

func (b *builder) prose(words int) string {
	vocab := strings.Fields(`the a crews fire road lane closed traffic vehicle report units responding
		highway near north south east west of at mile marker containment acres evacuation warning
		order winds gusts humidity critical conditions expected until advisory caltrans chp`)
	var sb strings.Builder
	for k := 0; k < words; k++ {
		if k > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(vocab[b.rng.IntN(len(vocab))])
	}
	return sb.String()
}

// newEvent makes revision 1 of event n: a layer, its detail, geometry where
// the layer has one, as the normalizers produce them.
func (b *builder) newEvent(n int, first time.Time) *gridv1.Event {
	lat, lng := b.point()
	layers := []gridv1.Layer{gridv1.Layer_ROAD_INCIDENT, gridv1.Layer_ROAD_INCIDENT, gridv1.Layer_WEATHER_ALERT,
		gridv1.Layer_EARTHQUAKE, gridv1.Layer_WILDFIRE, gridv1.Layer_POWER, gridv1.Layer_MESH,
		gridv1.Layer_EVACUATION, gridv1.Layer_BURN_STATUS}
	ev := &gridv1.Event{Layer: layers[n%len(layers)], Severity: gridv1.Severity(b.rng.IntN(5)),
		Status: gridv1.EventStatus_ACTIVE, ObservedAt: ts(first), Effective: ts(first.Add(-5 * time.Minute))}
	prov := func(src, name string) {
		ev.Provenance = &gridv1.Provenance{SourceId: src, SourceName: name, Attribution: "Data: " + name,
			SourceUrl: "https://example.org/" + src, FetchedAt: ts(first)}
	}
	pointGeom := func() {
		ev.Geometry, _, _ = geometry([]byte(fmt.Sprintf(`{"type":"Point","coordinates":[%.5f,%.5f]}`, lng, lat)))
	}
	switch ev.Layer {
	case gridv1.Layer_ROAD_INCIDENT:
		ev.Id = fmt.Sprintf("chp:2611%02dST%04d", n%28+1, n)
		ev.Category = "incident"
		ev.Headline = "Traffic hazard " + b.prose(4)
		ev.Description = b.prose(40 + b.rng.IntN(80))
		ev.AreaLabel = "SR 4 at Country Club Drive"
		prov("chp", "CHP")
		pointGeom()
		var log []*gridv1.IncidentLogEntry
		for k := 0; k < 1+b.rng.IntN(8); k++ {
			log = append(log, &gridv1.IncidentLogEntry{Number: int32(k + 1), At: ts(first.Add(time.Duration(k) * time.Minute)), Text: b.prose(12)})
		}
		ev.Detail = &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{LogNumber: fmt.Sprint(n),
			Impact: "moderate", Duration: "< 1 hour", Metadata: map[string]string{"lanes": "1"}, Log: log}}
		if n%3 == 0 {
			ev.Summary = b.prose(25)
			ev.Enhancement = &gridv1.Enhancement{Model: "gpt-4o-mini", EnhancedAt: ts(first),
				Fields: []string{"summary", "severity"}, Request: b.prose(60), Response: `{"summary":"` + b.prose(20) + `"}`}
		}
	case gridv1.Layer_WEATHER_ALERT:
		ev.Id = fmt.Sprintf("wx:urn:oid:2.49.0.1.840.0.%s.%03d.1", b.hex(12), n)
		ev.Category = "Red Flag Warning"
		ev.Headline = "Red Flag Warning issued " + b.prose(5)
		ev.Description = b.prose(150 + b.rng.IntN(200))
		ev.Expires = ts(first.Add(36 * time.Hour))
		ev.PlaceIds = []string{"county:calaveras-county", "county:tuolumne-county"} // preset zone -> area mapping
		prov("nws", "National Weather Service")
		ev.Detail = &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{NwsSeverity: "Severe",
			Certainty: "Likely", Urgency: "Expected", Instruction: b.prose(40), AreaDesc: "West Slope Central Sierra",
			Zones: []string{"CAZ137", "CAZ138"}}}
	case gridv1.Layer_EARTHQUAKE:
		ev.Id = fmt.Sprintf("usgs:nc75%06d", n)
		ev.Category = "earthquake"
		ev.Headline = fmt.Sprintf("M %.1f - %s", 1+b.rng.Float64()*3, b.prose(3))
		ev.CanonicalUrl = "https://earthquake.usgs.gov/earthquakes/eventpage/nc75" + fmt.Sprint(n)
		prov("usgs", "USGS")
		pointGeom()
		ev.Detail = &gridv1.Event_Earthquake{Earthquake: &gridv1.EarthquakeDetail{Magnitude: 1 + b.rng.Float64()*3, DepthKm: b.rng.Float64() * 20}}
	case gridv1.Layer_WILDFIRE:
		ev.Id = fmt.Sprintf("calfire:%s-%s-%04d", b.hex(8), b.hex(4), n)
		ev.Category = "wildfire"
		ev.Headline = "Fire " + b.prose(2)
		ev.Description = b.prose(30)
		prov("calfire", "CAL FIRE")
		ev.Geometry, _, _ = geometry(b.polygon(lat, lng, 0.01+b.rng.Float64()*0.05, b.spec.PerimeterVertices))
		ev.Detail = &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{Acres: 10 + b.rng.Float64()*2000,
			Containment: int32(b.rng.IntN(100)), County: "Calaveras", HasPerimeter: true}}
	case gridv1.Layer_POWER:
		ev.Id = fmt.Sprintf("pge:%d", 40000000+n)
		ev.Category = "unplanned"
		ev.Headline = fmt.Sprintf("Power outage affecting %d customers", 1+b.rng.IntN(900))
		prov("pge", "PG&E")
		ev.Geometry, _, _ = geometry(b.polygon(lat, lng, 0.01, b.spec.PerimeterVertices/4+8))
		ev.Detail = &gridv1.Event_Power{Power: &gridv1.PowerDetail{OutageId: fmt.Sprint(n), Cause: "Equipment",
			CustomersAffected: int32(1 + b.rng.IntN(900)), CrewStatus: "Crew assigned", EstimatedRestoration: ts(first.Add(4 * time.Hour))}}
	case gridv1.Layer_MESH:
		key := b.hex(64)
		b.meshKeys = append(b.meshKeys, key)
		ev.Id = "meshcore:" + key
		ev.Category = "repeater"
		ev.Severity = gridv1.Severity_INFO
		ev.Headline = "Repeater " + b.prose(1)
		prov("meshcore", "MeshCore")
		pointGeom()
		ev.Detail = &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: key, NodeType: "repeater",
			Name: "SIERRA-" + b.hex(4), Reachability: gridv1.MeshReachability_REACHABLE, Telemetry: b.meshTelemetry(first)}}
	case gridv1.Layer_EVACUATION:
		ev.Id = fmt.Sprintf("evac:CAL-E%03d", n)
		ev.Category = "warning"
		ev.Headline = "Evacuation Warning " + b.prose(2)
		ev.Description = b.prose(60)
		prov("caloes", "Cal OES")
		ev.Geometry, _, _ = geometry(b.polygon(lat, lng, 0.02, b.spec.PerimeterVertices/2+8))
		ev.Detail = &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{ZoneId: fmt.Sprintf("CAL-E%03d", n),
			Level: "warning", EventType: "Fire", County: "Calaveras"}}
	case gridv1.Layer_BURN_STATUS: // no geometry: a fact about a county
		c := b.counties[n%len(b.counties)].id
		ev.Id = "burn:" + strings.TrimPrefix(c, "county:") + fmt.Sprintf("-%d", n)
		ev.Category = "burn_status"
		ev.Severity = gridv1.Severity_INFO
		ev.Headline = "Burn day status"
		ev.PlaceIds = []string{c}
		prov("burnline", "County burn line")
		ev.Detail = &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{}}
	}
	return ev
}

func (b *builder) meshTelemetry(at time.Time) *gridv1.MeshTelemetry {
	return &gridv1.MeshTelemetry{Snr: wrapperspb.Double(float64(b.rng.IntN(40)) / 4), Rssi: wrapperspb.Int32(int32(-120 + b.rng.IntN(60))),
		HopCount: wrapperspb.UInt32(uint32(b.rng.IntN(4))), Gateways: []string{"gw-" + b.hex(4)}, LastAdvertAt: ts(at)}
}

// placeIDs is what matchPlaces would roughly attach: the containing county
// for a point, the county under the centroid for a shape, unioned with
// presets.
func (b *builder) placeIDs(ev *gridv1.Event) []string {
	set := map[string]bool{}
	for _, p := range ev.GetPlaceIds() {
		set[p] = true
	}
	if c := ev.GetGeometry().GetCentroid(); c != nil {
		if id := b.countyAt(c.GetLat(), c.GetLng()); id != "" {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// mutate changes hashed content, as an upstream update does.
func (b *builder) mutate(ev *gridv1.Event) {
	switch d := ev.Detail.(type) {
	case *gridv1.Event_Wildfire:
		d.Wildfire.Acres *= 1.3
		d.Wildfire.Containment = min(d.Wildfire.Containment+10, 100)
		lat, lng := ev.GetGeometry().GetCentroid().GetLat(), ev.GetGeometry().GetCentroid().GetLng()
		ev.Geometry, _, _ = geometry(b.polygon(lat, lng, 0.02+b.rng.Float64()*0.05, b.spec.PerimeterVertices))
	case *gridv1.Event_RoadIncident:
		d.RoadIncident.Log = append(d.RoadIncident.Log, &gridv1.IncidentLogEntry{Number: int32(len(d.RoadIncident.Log) + 1), Text: b.prose(12)})
	case *gridv1.Event_Mesh:
		d.Mesh.Name = "SIERRA-" + b.hex(4)
	default:
		ev.Description = b.prose(30 + b.rng.IntN(60))
	}
	ev.Severity = gridv1.Severity(b.rng.IntN(5))
}

func liteBlob(ev *gridv1.Event) ([]byte, error) {
	c := proto.Clone(ev).(*gridv1.Event)
	if g := c.GetGeometry(); g != nil {
		g.Geojson, g.Object = nil, nil
	}
	return proto.Marshal(c)
}

func unixOrNil(t *timestamppb.Timestamp) any {
	if t == nil {
		return nil
	}
	return t.AsTime().Unix()
}

func (b *builder) events() error {
	span := 120 * 24 * time.Hour
	for n := 0; n < b.spec.Events; n++ {
		first := b.base.Add(time.Duration(b.rng.Int64N(int64(span)))).Truncate(time.Second).Add(time.Duration(b.rng.IntN(1e9)))
		ev := b.newEvent(n, first)
		revs := 1 + int(math.Floor(math.Pow(b.rng.Float64(), 2.5)*float64(b.spec.MaxRevisions)))
		if ev.Layer == gridv1.Layer_MESH || ev.Layer == gridv1.Layer_BURN_STATUS {
			revs = 1 + b.rng.IntN(b.spec.MaxRevisions*3)
		}
		// The last revision is a lifecycle transition for most past events.
		transition := n%10 < 6
		at := first
		var cur *gridv1.Event
		for rev := 1; rev <= revs; rev++ {
			c := proto.Clone(ev).(*gridv1.Event)
			if rev > 1 {
				at = at.Add(time.Duration(5+b.rng.IntN(600)) * time.Minute)
				if rev == revs && transition {
					c.Status = gridv1.EventStatus_RESOLVED
					if n%20 == 1 {
						c.Status = gridv1.EventStatus_EXPIRED
					}
				} else {
					b.mutate(ev)
					c = proto.Clone(ev).(*gridv1.Event)
					c.Provenance.FetchedAt = ts(at)
				}
				c.ObservedAt = ts(at)
			}
			ingested := at.Add(time.Duration(1+b.rng.IntN(90)) * time.Second)
			c.Revision = uint32(rev)
			c.IngestedAt = ts(ingested)
			c.PlaceIds = b.placeIDs(c)
			blob, err := proto.Marshal(c)
			if err != nil {
				return err
			}
			if err := b.exec("event_revisions", `INSERT INTO event_revisions (event_id, revision, observed_at, ingested_at, proto)
				VALUES (?,?,?,?,?)`, c.Id, rev, c.ObservedAt.AsTime().Unix(), ingested.Unix(), blob); err != nil {
				return err
			}
			cur = c
		}
		// In-place refreshes (the hash-equal path): fresher mesh telemetry,
		// a new enhancement; the blob moves, the revision does not.
		refreshes := 0
		if m := cur.GetMesh(); m != nil {
			m.Telemetry = b.meshTelemetry(at.Add(time.Hour))
			refreshes = 1 + b.rng.IntN(40)
		}
		blob, err := proto.Marshal(cur)
		if err != nil {
			return err
		}
		var lastSeen int64 // 0 = predates the v2 migration
		if n%7 != 0 {
			lastSeen = at.Add(time.Duration(b.rng.IntN(48)) * time.Hour).Unix()
		}
		if err := b.exec("events", `INSERT INTO events (id, layer, severity, status, source_id, effective, expires,
			observed_at, ingested_at, revision, content_hash, proto, last_seen_at, refreshes)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			cur.Id, int32(cur.Layer), int32(cur.Severity), int32(cur.Status), cur.Provenance.SourceId,
			unixOrNil(cur.Effective), unixOrNil(cur.Expires), cur.ObservedAt.AsTime().Unix(),
			cur.IngestedAt.AsTime().Unix(), cur.Revision, store.ContentHash(cur), blob, lastSeen, refreshes); err != nil {
			return err
		}
		if n%19 != 3 { // a few events predate event_lite and were never backfilled
			lite, err := liteBlob(cur)
			if err != nil {
				return err
			}
			if err := b.exec("event_lite", `INSERT INTO event_lite (event_id, proto) VALUES (?,?)`, cur.Id, lite); err != nil {
				return err
			}
		}
		for _, p := range cur.PlaceIds {
			if err := b.exec("event_places", `INSERT INTO event_places (event_id, place_id) VALUES (?,?)`, cur.Id, p); err != nil {
				return err
			}
		}
		if bb := cur.GetGeometry().GetBbox(); bb != nil {
			b.geoRowid++
			if err := b.exec("event_geo_map", `INSERT INTO event_geo_map (rowid, event_id) VALUES (?,?)`, b.geoRowid, cur.Id); err != nil {
				return err
			}
			if err := b.exec("event_geo", `INSERT INTO event_geo (rowid, min_lat, max_lat, min_lng, max_lng) VALUES (?,?,?,?,?)`,
				b.geoRowid, bb.MinLat, bb.MaxLat, bb.MinLng, bb.MaxLng); err != nil {
				return err
			}
			if b.sum.GeoEventID == "" {
				b.sum.GeoEventID = cur.Id
			}
		} else if b.sum.NoGeoEventID == "" {
			b.sum.NoGeoEventID = cur.Id
		}
		if transition && revs > 1 && b.sum.ResolvedEventID == "" {
			b.sum.ResolvedEventID = cur.Id
		}
		if cur.Status == gridv1.EventStatus_ACTIVE && cur.Layer == gridv1.Layer_ROAD_INCIDENT && b.sum.ActiveEventID == "" {
			b.sum.ActiveEventID = cur.Id
		}
	}
	return nil
}

// mesh writes observations (ids with gaps, the compacted prefix pruned, as
// the legacy prune leaves them), the watermark, rollups, activity and the
// monitor telemetry archive.
func (b *builder) mesh() error {
	keys := b.meshKeys
	for len(keys) < b.spec.MeshNodes {
		keys = append(keys, b.hex(64))
	}
	b.meshKeys = keys
	id := int64(1000) // the oldest rows were pruned
	heard := b.base.Add(118 * 24 * time.Hour)
	stmt, err := b.tx.Prepare(`INSERT INTO mesh_observations (id, pubkey, heard_at, broker, gateway, snr, rssi,
		hop_count, path, path_nodes) VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for n := 0; n < b.spec.MeshObservations; n++ {
		id += 1 + int64(b.rng.IntN(3)) // gaps: pruned or rolled-back ids
		heard = heard.Add(time.Duration(b.rng.IntN(800)) * time.Millisecond)
		hops := b.rng.IntN(4)
		var path, nodes []string
		for h := 0; h < hops; h++ {
			k := keys[b.rng.IntN(len(keys))]
			path = append(path, k[:2])
			if b.rng.IntN(3) > 0 {
				nodes = append(nodes, k)
			} else {
				nodes = append(nodes, "")
			}
		}
		var snr, rssi any
		if n%11 != 0 {
			snr, rssi = float64(b.rng.IntN(60))/4-5, -120+b.rng.IntN(70)
		}
		if _, err := stmt.Exec(id, keys[b.rng.IntN(len(keys))], heard.Unix(), "wss://mqtt.example.org:443/mqtt",
			"gw-"+b.hex(4), snr, rssi, hops, strings.Join(path, ","), strings.Join(nodes, ",")); err != nil {
			return fmt.Errorf("legacyfixture: insert mesh_observations: %w", err)
		}
		b.sum.Rows["mesh_observations"]++
	}
	b.sum.MaxObservationID = id
	// The watermark sits past the remaining rows' MAX(id) when the newest
	// compacted rows were pruned; here it covers most of them.
	b.sum.Watermark = id + 50
	if err := b.exec("mesh_meta", `INSERT INTO mesh_meta (key, value) VALUES ('compaction_watermark', ?)`, b.sum.Watermark); err != nil {
		return err
	}
	seen := map[[3]string]bool{}
	for n := 0; n < b.spec.LinkRollups; n++ {
		a, c := keys[b.rng.IntN(len(keys))], keys[b.rng.IntN(len(keys))]
		if a == c {
			continue
		}
		if a > c {
			a, c = c, a
		}
		day := b.base.Add(time.Duration(b.rng.IntN(120)) * 24 * time.Hour).Truncate(24 * time.Hour)
		k := [3]string{a, c, fmt.Sprint(day.Unix())}
		if seen[k] {
			continue
		}
		seen[k] = true
		var best any
		if n%9 != 0 {
			best = float64(b.rng.IntN(60))/4 - 5
		}
		if err := b.exec("mesh_link_rollup", `INSERT INTO mesh_link_rollup (a_pubkey, b_pubkey, bucket, observations,
			best_snr, first_seen, last_seen) VALUES (?,?,?,?,?,?,?)`, a, c, day.Unix(), 1+b.rng.IntN(500), best,
			day.Unix()+60, day.Unix()+80000); err != nil {
			return err
		}
	}
	for n, k := range keys {
		var relayed int64
		if n%3 != 0 {
			relayed = heard.Unix() - int64(b.rng.IntN(3600))
		}
		if err := b.exec("mesh_activity", `INSERT INTO mesh_activity (pubkey, last_heard_at, last_relayed_at) VALUES (?,?,?)`,
			k, heard.Unix()-int64(b.rng.IntN(7200)), relayed); err != nil {
			return err
		}
	}
	perNode := max(b.spec.MeshTelemetry/9, 1)
	for node := 0; node < 9 && node < len(keys); node++ {
		at := b.base
		for n := 0; n < perNode; n++ {
			at = at.Add(15 * time.Minute)
			var volts, pct, temp any
			if n%13 != 0 {
				volts, pct = 3.5+b.rng.Float64()*0.7, float64(b.rng.IntN(100))
			}
			if node%2 == 0 {
				temp = 5 + b.rng.Float64()*25
			}
			if err := b.exec("mesh_telemetry", `INSERT INTO mesh_telemetry (pubkey, reported_at, received_at, reporter,
				last_success_at, last_attempt_at, battery_volts, battery_pct, battery_pct_source, temperature_c,
				noise_floor_dbm, last_snr_db, last_rssi_dbm, tx_queue_len, uptime_s, airtime_ms, packets_sent,
				packets_recv, recv_errors) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				keys[node], at.Unix(), at.Unix()+3, "alanpi", at.Unix()-5, at.Unix()-5, volts, pct, "estimated", temp,
				-110+b.rng.IntN(10), float64(b.rng.IntN(40))/4, -100+b.rng.IntN(40), b.rng.IntN(3),
				n*900, n*120, n*40, n*300, b.rng.IntN(3)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *builder) burn() error {
	for _, l := range []struct{ id, status string }{{"calaveras-apcd", "green"}, {"tuolumne-apcd", "red"}} {
		at := b.base.Add(110 * 24 * time.Hour)
		if err := b.exec("burn_readings", `INSERT INTO burn_readings (line_id, status, message, transcript, confidence,
			observed_at, received_at, reporter_id) VALUES (?,?,?,?,?,?,?,?)`, l.id, l.status,
			"Today is a permissive burn day", b.prose(40), 92, at.Unix(), at.Unix()+40, "burn-line"); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) roads() error {
	roads := []string{"hwy4-angels-murphys", "hwy4-murphys-arnold", "hwy108-sonora-pinecrest", "hwy49-angels-sonora"}
	perRoad := max(b.spec.RoadSamples/len(roads), 1)
	for _, r := range roads {
		at := b.base
		for n := 0; n < perRoad; n++ {
			at = at.Add(15 * time.Minute)
			var dur, delay any
			if n%17 != 0 {
				dur, delay = 20+b.rng.IntN(15), b.rng.IntN(6)
			}
			if err := b.exec("road_samples", `INSERT INTO road_samples (road_id, at, status, congestion, duration_m, delay_m)
				VALUES (?,?,?,?,?,?)`, r, at.Unix(), 1+b.rng.IntN(3)/2, b.rng.IntN(4), dur, delay); err != nil {
				return err
			}
		}
	}
	return nil
}

type jsonRaw = json.RawMessage

func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }
