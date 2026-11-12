package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// Query-plan tests for the hot statements. The statements are not copied
// here: a tracer records the SQL and arguments each hot store call really
// issues, and those are EXPLAINed, so the test follows the code.
//
// Two tests share the call list (hotCalls):
//
//   - TestHotReadsUseTheirIndexes runs in every `make test`, on an empty
//     database, with sequential scans, bitmap scans and sorts disabled for the
//     EXPLAIN: at test size a seq scan is always cheapest, so the property
//     pinned is that an index EXISTS that can serve each statement (a missing
//     one leaves the disabled node in the plan, which fails the test).
//   - TestHotQueryPlansAtScale is opt-in (GRID_SCALE_DATABASE_URL, like
//     TestWritePhaseAtScale) and EXPLAIN (ANALYZE, BUFFERS)es the same calls on
//     the imported production-sized database with the planner's own choices
//     and fails if a hot statement scans a growing table sequentially.

// statementRecorder is a pgx.QueryTracer keeping what ran.
type statementRecorder struct {
	mu   sync.Mutex
	list []recorded
}

type recorded struct {
	sql  string
	args []any
}

func (r *statementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, recorded{sql: d.SQL, args: slices.Clone(d.Args)})
	return ctx
}
func (r *statementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take returns the statements recorded since the last call.
func (r *statementRecorder) take() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.list
	r.list = nil
	return out
}

// planTarget is what a hot call needs from its plans.
type planTarget struct {
	name string
	call func(ctx context.Context, s *Store) error
	// want lists index names; EACH entry is "a|b" alternatives of which at
	// least one must appear in the plans of the statements the call issued.
	want []string
}

// keepSort names the calls whose test-size plan leaves sorting enabled: they
// order by a key the primary key also provides, and with sorts off the planner
// walks the whole primary key instead of using the filter index.
var keepSort = map[string]bool{
	"tick: prior active by source":  true,
	"tick: events by source+status": true,
}

// hotCalls is every statement the API and the scheduler run per request or
// per tick, with the index each must be able to use. ids name rows that exist
// at scale (and anything at test size).
type hotIDs struct {
	place, corridor string // place ids
	event           string // an event with several revisions
	source          string
	pubkey          string // a node with telemetry
	road            string
}

func hotCalls(ids hotIDs) []planTarget {
	all := []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED,
		gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED}
	since := time.Now().Add(-7 * 24 * time.Hour)
	long := time.Now().Add(-90 * 24 * time.Hour)
	ev := func(q EventQuery) func(context.Context, *Store) error {
		return func(ctx context.Context, s *Store) error { _, _, err := s.QueryEvents(ctx, q); return err }
	}
	hist := func(q HistoryQuery) func(context.Context, *Store) error {
		return func(ctx context.Context, s *Store) error { _, _, err := s.QueryHistory(ctx, q); return err }
	}
	return []planTarget{
		{"events default", ev(EventQuery{}), []string{"events_pkey"}},
		{"events layer (map layer)", ev(EventQuery{Layers: []gridv1.Layer{gridv1.Layer_WILDFIRE}}),
			[]string{"idx_events_layer|idx_events_active", "events_pkey"}},
		{"events place+layer", ev(EventQuery{PlaceID: ids.corridor, Layers: []gridv1.Layer{gridv1.Layer_ROAD_INCIDENT}}),
			[]string{"idx_event_places_place|event_places_pkey"}},
		{"events place all statuses (lite)", ev(EventQuery{PlaceID: ids.place, Statuses: all, Lite: true}),
			[]string{"idx_event_places_place|event_places_pkey", "event_lite_pkey"}},
		{"events single status (list index)", ev(EventQuery{Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE}}),
			[]string{"idx_events_active"}},
		{"events since", ev(EventQuery{Since: since}), []string{"events_pkey"}},
		{"event by id", func(ctx context.Context, s *Store) error {
			_, err := s.GetEvent(ctx, ids.event)
			if err == ErrNotFound {
				return nil
			}
			return err
		}, []string{"events_pkey"}},
		{"event validator", func(ctx context.Context, s *Store) error { _, _, err := s.EventVersion(ctx, ids.event); return err },
			[]string{"events_pkey"}},
		{"event history", func(ctx context.Context, s *Store) error {
			_, _, err := s.EventHistory(ctx, ids.event, 50, "")
			return err
		}, []string{"event_revisions_pkey"}},
		{"event revision", func(ctx context.Context, s *Store) error {
			_, err := s.EventRevisionAt(ctx, ids.event, 1)
			if err == ErrNotFound {
				return nil
			}
			return err
		}, []string{"event_revisions_pkey"}},
		{"history", hist(HistoryQuery{}), []string{"idx_revisions_observed"}},
		{"history place", hist(HistoryQuery{PlaceID: ids.place}), []string{"idx_revisions_observed"}},
		{"history place+layer", hist(HistoryQuery{PlaceID: ids.place, Layers: []gridv1.Layer{gridv1.Layer_WILDFIRE}}),
			[]string{"idx_revisions_observed"}},
		{"history window", hist(HistoryQuery{From: since}), []string{"idx_revisions_observed"}},
		{"stats place", func(ctx context.Context, s *Store) error { _, err := s.Totals(ctx, ids.place); return err }, nil},
		{"stats all", func(ctx context.Context, s *Store) error { _, err := s.Totals(ctx, ""); return err }, nil},
		{"mesh links", func(ctx context.Context, s *Store) error { _, err := s.MeshLinks(ctx, since); return err },
			[]string{"idx_mesh_link_bucket", "mesh_observations_pkey|idx_mesh_obs_heard"}},
		{"mesh telemetry series", func(ctx context.Context, s *Store) error {
			_, _, err := s.MeshTelemetry(ctx, ids.pubkey, long, time.Now())
			return err
		}, []string{"mesh_telemetry_pkey|idx_mesh_telem_time"}},
		{"mesh telemetry coverage", func(ctx context.Context, s *Store) error {
			_, _, _, err := s.MeshTelemetryCoverage(ctx, ids.pubkey)
			return err
		}, []string{"mesh_telemetry_pkey"}},
		{"mesh activity", func(ctx context.Context, s *Store) error {
			_, err := s.MeshActivityFor(ctx, []string{ids.pubkey})
			return err
		}, []string{"mesh_activity_pkey"}},
		{"road samples", func(ctx context.Context, s *Store) error {
			_, err := s.RoadSamples(ctx, ids.road, long, time.Now())
			return err
		}, []string{"road_samples_pkey"}},
		{"road status since", func(ctx context.Context, s *Store) error {
			_, _, _, _, err := s.RoadStatusSince(ctx, ids.road)
			return err
		}, []string{"road_samples_pkey|idx_road_samples_status"}}, // TestRoadStatusSinceUsesStatusIndex pins the status index on real rows
		{"road typical minutes", func(ctx context.Context, s *Store) error {
			_, _, _, err := s.TypicalRoadMinutes(ctx, ids.road, 1, time.Now(), long, time.Now(), time.UTC, 5)
			return err
		}, []string{"idx_road_samples_status|road_samples_pkey"}},
		// The scheduler's per-tick statements.
		{"tick: prior active by source", func(ctx context.Context, s *Store) error {
			_, err := s.ActiveEventsBySource(ctx, ids.source)
			return err
		}, []string{"idx_events_source_status"}},
		{"tick: events by source+status", func(ctx context.Context, s *Store) error {
			_, err := s.EventsBySourceAndStatus(ctx, ids.source, gridv1.EventStatus_EXPIRED)
			return err
		}, []string{"idx_events_source_status"}},
		{"tick: hash pre-check", func(ctx context.Context, s *Store) error {
			_, err := s.ContentHashes(ctx, []string{ids.event})
			return err
		}, []string{"events_pkey"}},
		{"tick: touch seen", func(ctx context.Context, s *Store) error {
			_, err := s.TouchSeen(ctx, []string{ids.event}, time.Now(), time.Now().Add(-time.Hour))
			return err
		}, []string{"events_pkey"}},
		{"tick: telemetry predecessors", func(ctx context.Context, s *Store) error {
			return s.InsertMeshTelemetry(ctx, []MeshTelemetrySample{{PubKey: ids.pubkey, Reporter: "plan",
				ReportedAt: time.Now().Add(240 * time.Hour), ReceivedAt: time.Now(), UptimeS: 1}})
		}, []string{"mesh_telemetry_pkey"}},
		// Retention and boot.
		{"maintenance: compaction chunk", func(ctx context.Context, s *Store) error {
			_, err := s.CompactMeshObservations(ctx)
			return err
		}, []string{"mesh_observations_pkey"}},
		{"maintenance: prune observations", func(ctx context.Context, s *Store) error {
			_, err := s.PruneMeshObservations(ctx, time.Unix(1, 0))
			return err
		}, []string{"idx_mesh_obs_heard|mesh_observations_pkey"}},
		{"maintenance: prune telemetry", func(ctx context.Context, s *Store) error {
			_, err := s.PruneMeshTelemetry(ctx, time.Unix(1, 0))
			return err
		}, []string{"idx_mesh_telem_time"}},
		{"maintenance: prune rollup", func(ctx context.Context, s *Store) error {
			_, err := s.PruneMeshLinkRollup(ctx, time.Unix(1, 0))
			return err
		}, []string{"idx_mesh_link_bucket"}},
		{"boot: observation paths", func(ctx context.Context, s *Store) error {
			_, err := s.MeshObservationPaths(ctx, since)
			return err
		}, []string{"idx_mesh_obs_heard"}},
		{"boot: node heard times", func(ctx context.Context, s *Store) error {
			_, err := s.MeshNodeHeardTimes(ctx, 50)
			return err
		}, []string{"idx_mesh_obs_pubkey"}},
	}
}

// planOf EXPLAINs one recorded statement in its own transaction (rolled back:
// DML is only planned, never run, unless analyze, which scale uses for SELECTs
// only). settings are SET LOCAL before it.
func planOf(ctx context.Context, s *Store, r recorded, analyze bool, settings ...string) (planNode, float64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return planNode{}, 0, err
	}
	defer tx.Rollback()
	for _, st := range settings {
		if _, err := tx.ExecContext(ctx, "SET LOCAL "+st); err != nil {
			return planNode{}, 0, err
		}
	}
	opts := "VERBOSE, FORMAT JSON"
	if analyze {
		opts = "ANALYZE, BUFFERS, FORMAT JSON"
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "EXPLAIN ("+opts+") "+r.sql, r.args...).Scan(&raw); err != nil {
		return planNode{}, 0, fmt.Errorf("%w\nstatement: %s", err, r.sql)
	}
	var doc []struct {
		Plan      planNode `json:"Plan"`
		Execution float64  `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		return planNode{}, 0, fmt.Errorf("unreadable plan: %v: %s", err, raw)
	}
	return doc[0].Plan, doc[0].Execution, nil
}

// explainable reports whether EXPLAIN accepts the statement (transaction
// control and the like do not).
func explainable(sqlText string) bool {
	t := strings.ToUpper(strings.TrimSpace(sqlText))
	for _, p := range []string{"SELECT", "WITH", "UPDATE", "DELETE", "INSERT"} {
		if strings.HasPrefix(t, p) {
			return !strings.Contains(t, "WRITER_LEASE") && !strings.Contains(t, "FROM VERSIONS")
		}
	}
	return false
}

var testIDs = hotIDs{place: "county:calaveras", corridor: "corridor:hwy4", event: "usgs:e1", source: "usgs",
	pubkey: "aa11", road: "hwy4"}

// indexesUsed runs h and returns the indexes the test-size plans of its
// statements read, and the plans for a failure message.
func indexesUsed(t *testing.T, s *Store, rec *statementRecorder, h planTarget) (map[string]bool, string) {
	t.Helper()
	ctx := context.Background()
	rec.take()
	require.NoError(t, h.call(ctx, s))
	used := map[string]bool{}
	var plans []string
	for _, st := range rec.take() {
		if !explainable(st.sql) {
			continue
		}
		// Sequential scans, bitmap scans and sorts off: see the file comment.
		knobs := []string{"enable_seqscan = off", "enable_bitmapscan = off", "enable_sort = off"}
		if keepSort[h.name] {
			knobs = knobs[:2]
		}
		plan, _, err := planOf(ctx, s, st, false, knobs...)
		require.NoError(t, err)
		for _, ix := range plan.indexes() {
			used[ix] = true
		}
		plans = append(plans, plan.String())
	}
	return used, strings.Join(plans, "\n---\n")
}

func TestHotReadsUseTheirIndexes(t *testing.T) {
	rec := &statementRecorder{}
	s := newTestStore(t, WithQueryTracer(rec))

	for _, h := range hotCalls(testIDs) {
		t.Run(h.name, func(t *testing.T) {
			used, plans := indexesUsed(t, s, rec, h)
			for _, w := range h.want {
				ok := false
				for _, alt := range strings.Split(w, "|") {
					ok = ok || used[alt]
				}
				assert.True(t, ok, "%s must be able to use %s; indexes used %v; plans:\n%s", h.name, w, keys(used), plans)
			}
		})
	}
}

// The check is not vacuous: without the index a statement's plan no longer
// shows it.
func TestHotReadsPlanCheckSeesAMissingIndex(t *testing.T) {
	rec := &statementRecorder{}
	s := newTestStore(t, WithQueryTracer(rec))
	var h planTarget
	for _, c := range hotCalls(testIDs) {
		if c.name == "tick: prior active by source" {
			h = c
		}
	}
	require.NotNil(t, h.call)
	used, _ := indexesUsed(t, s, rec, h)
	require.True(t, used["idx_events_source_status"])

	_, err := s.db.Exec(`DROP INDEX idx_events_source_status`)
	require.NoError(t, err)
	used, _ = indexesUsed(t, s, rec, h)
	assert.False(t, used["idx_events_source_status"])
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// growing are the tables that only get bigger with time. A hot statement that
// reads one sequentially is a latency bomb that a test-size database never
// shows.
var growing = []string{"event_revisions", "mesh_observations", "mesh_telemetry", "mesh_link_rollup", "road_samples"}

// seqScanned lists the relations a plan reads sequentially.
func seqScanned(p planNode) []string {
	var out []string
	p.walk(func(n planNode) {
		if n.NodeType == "Seq Scan" {
			out = append(out, n.Relation)
		}
	})
	return out
}

// TestHotQueryPlansAtScale: opt-in, on the production-sized database
// (GRID_SCALE_DATABASE_URL, a throwaway clone). Every hot call is EXPLAIN
// (ANALYZE, BUFFERS)ed with the planner's own choices, under the settings a
// gp3-backed RDS instance should have (random_page_cost 1.1; the default of 4
// models spinning disks), and must not scan a growing table sequentially. The
// plans and times are logged for the P5 note. -v shows them.
func TestHotQueryPlansAtScale(t *testing.T) {
	url := scaleURL(t)
	ctx := context.Background()
	rec := &statementRecorder{}
	s, err := Open(url, WithQueryTracer(rec))
	require.NoError(t, err)
	defer s.Close()

	var ids hotIDs
	one := func(dst *string, q string) {
		t.Helper()
		require.NoError(t, s.db.QueryRowContext(ctx, q).Scan(dst), q)
	}
	one(&ids.place, `SELECT place_id FROM event_places GROUP BY place_id ORDER BY COUNT(*) DESC LIMIT 1`)
	one(&ids.corridor, `SELECT id FROM places WHERE kind = 5 ORDER BY id LIMIT 1`)
	one(&ids.event, `SELECT event_id FROM event_revisions GROUP BY event_id ORDER BY COUNT(*) DESC, event_id LIMIT 1`)
	one(&ids.source, `SELECT source_id FROM events GROUP BY source_id ORDER BY COUNT(*) DESC LIMIT 1`)
	one(&ids.pubkey, `SELECT pubkey FROM mesh_telemetry GROUP BY pubkey ORDER BY COUNT(*) DESC LIMIT 1`)
	one(&ids.road, `SELECT road_id FROM road_samples GROUP BY road_id ORDER BY COUNT(*) DESC LIMIT 1`)
	t.Logf("sampled: %+v", ids)
	rec.take()

	// Statistics as autovacuum would have them.
	_, err = s.db.ExecContext(ctx, `ANALYZE`)
	require.NoError(t, err)

	t.Logf("%-38s %9s  %s", "call", "ms", "plans (top node, indexes, seq scans)")
	for _, h := range hotCalls(ids) {
		require.NoError(t, h.call(ctx, s), h.name)
		for _, st := range rec.take() {
			if !explainable(st.sql) {
				continue
			}
			analyze := strings.HasPrefix(strings.ToUpper(strings.TrimSpace(st.sql)), "SELECT") ||
				strings.HasPrefix(strings.ToUpper(strings.TrimSpace(st.sql)), "WITH")
			plan, ms, err := planOf(ctx, s, st, analyze, "random_page_cost = 1.1")
			require.NoError(t, err, h.name)
			seq := seqScanned(plan)
			t.Logf("%-38s %9.2f  %s idx=%v seq=%v", h.name, ms, plan.NodeType, plan.indexes(), seq)
			for _, rel := range seq {
				assert.NotContains(t, growing, rel, "%s reads %s sequentially; plan:\n%s\nstatement: %s", h.name, rel, plan, st.sql)
			}
		}
	}
}
