package sqliteimport_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/sqliteimport"
	"github.com/sierragridteam/sierra-data/internal/sqliteimport/legacyfixture"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// TestImportAtProductionScale times snapshot, import and verify on a fixture
// the size of the production grid.db (about 150 MiB, dominated by
// event_revisions and mesh_observations). Opt-in, a few minutes:
//
//	GRIDCTL_SCALE_TEST=1 make test   (or go test -run AtProductionScale -v ./internal/sqliteimport/)
//
// GRIDCTL_SCALE_DIR keeps the fixture in a directory of your choice
// (native storage, not a bind mount, for representative numbers).
func TestImportAtProductionScale(t *testing.T) {
	if os.Getenv("GRIDCTL_SCALE_TEST") == "" {
		t.Skip("SKIP: set GRIDCTL_SCALE_TEST=1 to time the importer on a production-sized fixture")
	}
	ctx := context.Background()
	url := storetest.URL(t)
	dir := os.Getenv("GRIDCTL_SCALE_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else {
		var err error
		dir, err = os.MkdirTemp(dir, "gridctl-scale-")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	live := filepath.Join(dir, "grid.db")

	start := time.Now()
	sum, err := legacyfixture.Build(ctx, live, legacyfixture.ProductionScale())
	require.NoError(t, err)
	st, err := os.Stat(live)
	require.NoError(t, err)
	t.Logf("fixture: %.1f MiB in %s; rows %v", float64(st.Size())/(1<<20), time.Since(start).Round(time.Millisecond), sum.Rows)

	snap := filepath.Join(dir, "snap.db")
	start = time.Now()
	m, err := sqliteimport.Snapshot(ctx, live, snap, sqliteimport.SnapshotOptions{})
	require.NoError(t, err)
	t.Logf("snapshot: %.1f MiB in %s (VACUUM INTO + quick_check + counts + sha256)", float64(m.Size)/(1<<20), time.Since(start).Round(time.Millisecond))

	start = time.Now()
	r, err := sqliteimport.Import(ctx, sqliteimport.ImportOptions{SourcePath: snap, Manifest: m, TargetURL: url})
	require.NoError(t, err)
	t.Logf("import: %s total (copy %.1fs, verify %.1fs)", time.Since(start).Round(time.Millisecond), r.CopySeconds, r.VerifySeconds)
	for _, tr := range r.Tables {
		t.Logf("  %-18s %8d rows  copy %6.2fs  verify %6.2fs", tr.Table, tr.TargetRows, tr.CopySeconds, tr.VerifySeconds)
	}

	start = time.Now()
	_, err = sqliteimport.Verify(ctx, snap, m, url)
	require.NoError(t, err)
	t.Logf("verify (read-only, separate run): %s", time.Since(start).Round(time.Millisecond))
}
