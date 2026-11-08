package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
)

// A standby is prepareGrid and nothing else, and it must not touch grid.db:
// not open it, not create it, not migrate or seed it. The old task still
// holds it.
func TestPrepareGridDoesNotTouchTheDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "data", "grid.db")
	cfg := &config.Config{}
	cfg.Grid.DBPath = db
	cfg.OpenAI.APIKey = "test"

	app, err := prepareGrid(testCtx(), cfg)
	require.NoError(t, err)
	require.NotNil(t, app)
	_, err = os.Stat(filepath.Dir(db))
	assert.True(t, os.IsNotExist(err), "preparation must not even create the database directory")
	assert.Nil(t, app.store)
	assert.NoError(t, app.Quiesce(testCtx()), "releasing a never-activated Grid is a no-op")
}

func TestPrepareGridRejectsBadConfigBeforeAnyHandoff(t *testing.T) {
	cfg := &config.Config{}
	cfg.OpenAI.APIKey = "test"
	_, err := prepareGrid(testCtx(), cfg)
	assert.ErrorContains(t, err, "grid.dbPath")

	cfg.Grid.DBPath = filepath.Join(t.TempDir(), "grid.db")
	cfg.Grid.Ingest.Reporters = []config.Reporter{{ID: "alanpi", TokenSha256: "not-a-hash"}}
	_, err = prepareGrid(testCtx(), cfg)
	assert.ErrorContains(t, err, "grid.ingest", "a bad reporter hash must stop a standby, not its activation")
}

func testCtx() context.Context { return logging.EnsureLogger(context.Background()) }
