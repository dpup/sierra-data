package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/config"
)

// A standby relay must not open, create or lock relay.db.
func TestPrepareRelayDoesNotTouchTheDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "data", "relay.db")
	app, err := prepareRelay(testCtx(), &config.MeshRelayConfig{DBPath: db})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Dir(db))
	assert.True(t, os.IsNotExist(err))
	assert.NoError(t, app.Quiesce(testCtx()), "releasing a never-activated relay is a no-op")
}

func testCtx() context.Context { return logging.EnsureLogger(context.Background()) }
