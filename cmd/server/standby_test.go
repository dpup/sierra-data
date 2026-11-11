package main

import (
	"context"
	"database/sql"
	"testing"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// A standby is prepareGrid and nothing else, and it must not touch the
// database: not connect to it, not migrate or seed it. The old writer still
// owns it.
func TestPrepareGridDoesNotTouchTheDatabase(t *testing.T) {
	url := storetest.EmptyURL(t)
	cfg := &config.Config{}
	cfg.Grid.Database.URL = url
	cfg.OpenAI.APIKey = "test"

	app, err := prepareGrid(testCtx(), cfg)
	require.NoError(t, err)
	require.NotNil(t, app)
	assert.Nil(t, app.store)

	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer db.Close()
	var tables, others int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`).Scan(&others))
	assert.Zero(t, tables, "preparation must not migrate the database")
	assert.Zero(t, others, "preparation must not even connect")
	assert.NoError(t, app.Quiesce(testCtx()), "releasing a never-activated Grid is a no-op")
}

func TestPrepareGridRejectsBadConfigBeforeAnyHandoff(t *testing.T) {
	cfg := &config.Config{}
	cfg.OpenAI.APIKey = "test"
	_, err := prepareGrid(testCtx(), cfg)
	assert.ErrorContains(t, err, "grid.database.url")

	cfg.Grid.Database.URL = "postgres://unused.invalid/grid"
	cfg.Grid.Ingest.Reporters = []config.Reporter{{ID: "alanpi", TokenSha256: "not-a-hash"}}
	_, err = prepareGrid(testCtx(), cfg)
	assert.ErrorContains(t, err, "grid.ingest", "a bad reporter hash must stop a standby, not its activation")
}

func testCtx() context.Context { return logging.EnsureLogger(context.Background()) }
