package main

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/store/migrations"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// Every process migrates at boot (internal/store/migrations): a reader-only
// process (grid.writer.enabled=false) on a database an older release left at
// the baseline brings it to this build's schema and becomes ready without any
// writer existing. It never takes the lease.
func TestReaderOnlyProcessMigratesAndIsReady(t *testing.T) {
	blockOutbound(t)
	ctx := context.Background()
	url := storetest.EmptyURL(t)
	setup, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer setup.Close()
	v, err := migrations.ApplyThrough(ctx, setup, 1)
	require.NoError(t, err)
	require.Equal(t, 1, v, "the fixture is the baseline only")

	app, port := preparedGrid(t, url)
	off := false
	app.cfg.Grid.Writer.Enabled = &off
	h := runGrid(t, app, false)

	assert.Equal(t, http.StatusOK, httpCode(t, port, "/readyz"), "ready without a writer")
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/api/v1/sources"))
	v, err = migrations.Current(ctx, setup)
	require.NoError(t, err)
	assert.Equal(t, migrations.Latest(), v, "the reader migrated")

	time.Sleep(300 * time.Millisecond) // a campaign, were there one, polls every 250 ms
	assert.False(t, app.DatabaseHeld(), "a reader-only process never takes the lease")
	var epoch int64
	var holder string
	require.NoError(t, setup.QueryRowContext(ctx, `SELECT epoch, holder FROM writer_lease WHERE id = 1`).Scan(&epoch, &holder))
	assert.Zero(t, epoch)
	assert.Empty(t, holder)
	assertLockFree(t, url)
	assert.Equal(t, 0, h.terminate())
}

// The other direction of the policy: a process whose build is OLDER than the
// database (a newer release migrated past it) keeps serving.
func TestOlderBuildServesANewerSchema(t *testing.T) {
	blockOutbound(t)
	ctx := context.Background()
	url := storetest.URL(t)
	setup, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer setup.Close()
	_, err = setup.ExecContext(ctx, `CREATE TABLE from_the_future (id bigint)`)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, `INSERT INTO schema_migrations (source, version) VALUES ('postgres', $1)`, migrations.Latest()+1)
	require.NoError(t, err)

	app, port := preparedGrid(t, url)
	h := runGrid(t, app, false)
	assert.Equal(t, http.StatusOK, httpCode(t, port, "/readyz"))
	require.Eventually(t, app.DatabaseHeld, 20*time.Second, 20*time.Millisecond,
		"an older build still takes the writer role on a newer schema")
	st, err := store.OpenReader(url)
	require.NoError(t, err)
	defer st.Close()
	s, err := st.Settings(ctx)
	require.NoError(t, err)
	assert.Equal(t, migrations.Latest()+1, s.SchemaVersion)
	assert.Equal(t, 0, h.terminate())
}
