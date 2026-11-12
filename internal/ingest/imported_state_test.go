package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/sqliteimport"
	"github.com/sierragridteam/sierra-data/internal/sqliteimport/legacyfixture"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// The writer continues from an imported legacy store: a scheduler tick over
// a database loaded by the SQLite importer revises an imported event (its
// revision counter, first_ingested_at and history continue), inserts a new
// one, sweeps the rest of the source, and appends mesh observations past the
// compaction watermark, with the versions counter equal to the revisions
// actually stored throughout.
func TestSchedulerTickContinuesFromImportedState(t *testing.T) {
	ctx := testCtx()
	dir := t.TempDir()
	live := filepath.Join(dir, "grid.db")
	sum, err := legacyfixture.Build(ctx, live, legacyfixture.Small())
	require.NoError(t, err)
	snap := filepath.Join(dir, "snap.db")
	m, err := sqliteimport.Snapshot(ctx, live, snap, sqliteimport.SnapshotOptions{})
	require.NoError(t, err)
	url := storetest.URL(t)
	_, err = sqliteimport.Import(ctx, sqliteimport.ImportOptions{SourcePath: snap, Manifest: m, TargetURL: url})
	require.NoError(t, err)

	st, err := store.Open(url)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	pg, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { pg.Close(context.Background()) })
	count := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		require.NoError(t, pg.QueryRow(ctx, q, args...).Scan(&n))
		return n
	}
	maxRevID := count(`SELECT max(id) FROM event_revisions`)
	importedRevs := count(`SELECT count(*) FROM event_revisions`)

	old, err := st.GetEvent(ctx, sum.ActiveEventID)
	require.NoError(t, err)
	firstBefore, err := st.FirstIngested(ctx, []string{old.GetId()})
	require.NoError(t, err)
	require.Contains(t, firstBefore, old.GetId())

	changed := proto.Clone(old).(*gridv1.Event)
	changed.Headline = "Lane reopened, traffic slow"
	changed.ObservedAt = timestamppb.Now()
	fresh := schedEvent("chp:imported-state-new", "chp", gridv1.Layer_ROAD_INCIDENT)
	fresh.ObservedAt = timestamppb.Now()
	key := "ab" + sum.SourceIDs[0] // any key: an observation is a measurement
	fn := &fakeNormalizer{ids: []string{"chp"}, result: &PollResult{
		Events:           []*gridv1.Event{changed, fresh},
		MeshObservations: []store.MeshObservation{{PubKey: key, HeardAt: time.Now()}},
	}}
	activeCHP := count(`SELECT count(*) FROM events WHERE source_id = 'chp' AND status = $1`, int32(gridv1.EventStatus_ACTIVE))

	sched := NewScheduler(st, SchedulerConfig{})
	sched.tick(ctx, PollerSpec{Normalizer: fn, Interval: time.Minute}, &pollerState{})

	got, err := st.GetEvent(ctx, old.GetId())
	require.NoError(t, err)
	assert.Equal(t, old.GetRevision()+1, got.GetRevision(), "the revision counter continues")
	assert.Equal(t, "Lane reopened, traffic slow", got.GetHeadline())
	hist, _, err := st.EventHistory(ctx, old.GetId(), 200, "")
	require.NoError(t, err)
	assert.Len(t, hist, int(got.GetRevision()), "history continues from the imported revisions")
	firstAfter, err := st.FirstIngested(ctx, []string{old.GetId(), fresh.GetId()})
	require.NoError(t, err)
	assert.Equal(t, firstBefore[old.GetId()], firstAfter[old.GetId()], "first_ingested_at is never rewritten")
	assert.WithinDuration(t, time.Now(), firstAfter[fresh.GetId()], time.Minute)

	n, err := st.GetEvent(ctx, fresh.GetId())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n.GetRevision())

	// The other imported ACTIVE chp events left the feed: resolved, each a revision.
	newRevs := count(`SELECT count(*) FROM event_revisions WHERE id > $1`, maxRevID)
	assert.Equal(t, 2+(activeCHP-1), newRevs, "one update, one insert, one resolve per other active chp event")
	dv, err := st.DataVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, importedRevs+newRevs, dv, "versions.revisions keeps counting from the imported total")
	assert.Equal(t, count(`SELECT count(*) FROM event_revisions`), dv)

	// The new observation is past the watermark, so compaction rolls it up.
	assert.Greater(t, count(`SELECT id FROM mesh_observations WHERE pubkey = $1`, key), sum.Watermark)
	_, err = st.CompactMeshObservations(ctx)
	require.NoError(t, err)
	assert.Equal(t, count(`SELECT max(id) FROM mesh_observations`),
		count(`SELECT value FROM mesh_meta WHERE key = 'compaction_watermark'`))
	src := sourceByID(t, st, "chp")
	assert.Equal(t, gridv1.SourceStatus_OK, src.GetStatus())
}
