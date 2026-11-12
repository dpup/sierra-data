package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/sqliteimport/legacyfixture"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// fakeS3 stands in for the bucket: whole objects in memory.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
}

func (f *fakeS3) Put(_ context.Context, bucket, key, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[bucket+"/"+key] = b
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeS3) Get(_ context.Context, bucket, key, path string) error {
	f.mu.Lock()
	b, ok := f.objects[bucket+"/"+key]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("NoSuchKey: %s/%s", bucket, key)
	}
	return os.WriteFile(path, b, 0o600)
}

func useFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	orig := newObjectStore
	newObjectStore = func(context.Context) (objectStore, error) { return f, nil }
	t.Cleanup(func() { newObjectStore = orig })
	return f
}

func gridctl(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func liveFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grid.db")
	_, err := legacyfixture.Build(context.Background(), path, legacyfixture.Small())
	require.NoError(t, err)
	return path
}

func TestParseLocation(t *testing.T) {
	l, err := parseLocation("s3://bucket/dir/grid.db")
	require.NoError(t, err)
	assert.True(t, l.isS3())
	assert.Equal(t, "bucket", l.bucket)
	assert.Equal(t, "dir/grid.db", l.key)
	m := l.withSuffix(".manifest.json")
	assert.Equal(t, "dir/grid.db.manifest.json", m.key)
	assert.Equal(t, "s3://bucket/dir/grid.db.manifest.json", m.String())

	l, err = parseLocation("/data/grid.db")
	require.NoError(t, err)
	assert.False(t, l.isS3())
	for _, bad := range []string{"", "s3://", "s3://bucket", "s3://bucket/", "s3:///key", "s3://b/dir/"} {
		_, err := parseLocation(bad)
		assert.Error(t, err, bad)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	code, _, _ := gridctl(t)
	assert.Equal(t, 2, code)
	code, _, _ = gridctl(t, "frobnicate")
	assert.Equal(t, 2, code)
	code, _, stderr := gridctl(t, "snapshot", "--db", "x")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "--db and --out are required")
	t.Setenv("PF__GRID__DATABASE__URL", "")
	code, _, stderr = gridctl(t, "import", "--from", "x.db")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "PF__GRID__DATABASE__URL")
	code, _, _ = gridctl(t, "version")
	assert.Equal(t, 0, code)
}

// The full path the cutover runs: snapshot to the bucket, import from it
// (target from the environment, as the task's secret injects it), verify,
// and a re-run that refuses.
func TestSnapshotImportVerifyThroughS3(t *testing.T) {
	s3 := useFakeS3(t)
	live := liveFixture(t)
	url := storetest.URL(t)

	code, out, stderr := gridctl(t, "snapshot", "--db", live, "--out", "s3://migration/grid-1.db", "--tmp", t.TempDir())
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []string{"grid-1.db", "grid-1.db.manifest.json"}, s3.puts, "database first, manifest last")
	assert.Contains(t, out, "quick_check ok")

	t.Setenv("PF__GRID__DATABASE__URL", url)
	reportPath := filepath.Join(t.TempDir(), "report.json")
	code, out, stderr = gridctl(t, "import", "--from", "s3://migration/grid-1.db", "--report", reportPath)
	require.Equal(t, 0, code, out+stderr)
	assert.Contains(t, out, "RESULT: OK")
	assert.NotContains(t, out+stderr, url, "the database URL is never printed")
	assert.FileExists(t, reportPath)

	code, out, _ = gridctl(t, "verify", "--from", "s3://migration/grid-1.db")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "RESULT: OK")

	code, _, stderr = gridctl(t, "import", "--from", "s3://migration/grid-1.db")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "not empty")
}

func TestImportNeedsTheManifestUnlessToldOtherwise(t *testing.T) {
	useFakeS3(t)
	live := liveFixture(t)
	url := storetest.URL(t)
	// A bare file (an EFS backup restore copied as is) has no manifest.
	code, _, stderr := gridctl(t, "import", "--from", live, "--to", url)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "--manifest none")

	code, out, stderr := gridctl(t, "import", "--from", live, "--to", url, "--manifest", "none")
	require.Equal(t, 0, code, out+stderr)
	assert.Contains(t, stderr, "no manifest")
}

func TestVerifyReportsDifferencesAndExitsOne(t *testing.T) {
	live := liveFixture(t)
	url := storetest.URL(t)
	snap := filepath.Join(t.TempDir(), "snap.db")
	code, _, stderr := gridctl(t, "snapshot", "--db", live, "--out", snap)
	require.Equal(t, 0, code, stderr)
	code, _, stderr = gridctl(t, "import", "--from", snap, "--to", url)
	require.Equal(t, 0, code, stderr)

	execSQL(t, url, `DELETE FROM mesh_activity WHERE pubkey = (SELECT min(pubkey) FROM mesh_activity)`)
	code, out, _ := gridctl(t, "verify", "--from", snap, "--to", url)
	assert.Equal(t, 1, code)
	assert.Contains(t, out, "RESULT: FAILED")
	assert.True(t, strings.Contains(out, "mesh_activity") && strings.Contains(out, "DIFFERS"), out)
}

func execSQL(t *testing.T, url, q string) {
	t.Helper()
	c, err := pgx.Connect(context.Background(), url)
	require.NoError(t, err)
	defer c.Close(context.Background())
	_, err = c.Exec(context.Background(), q)
	require.NoError(t, err)
}

// import refuses while a server holds the writer lease (exit 1, the holder
// named); verify is read-only and runs regardless; after the holder lets go
// the import proceeds.
func TestImportRefusedWhileAWriterHoldsTheLease(t *testing.T) {
	live := liveFixture(t)
	snap := filepath.Join(t.TempDir(), "snap.db")
	code, _, stderr := gridctl(t, "snapshot", "--db", live, "--out", snap)
	require.Equal(t, 0, code, stderr)
	url := storetest.URL(t)
	code, _, stderr = gridctl(t, "import", "--from", snap, "--to", url)
	require.Equal(t, 0, code, stderr)

	holder, err := store.OpenReader(url)
	require.NoError(t, err)
	defer holder.Close()
	_, err = holder.AcquireLease(context.Background(), store.LeaseOptions{Holder: "grid-task-1", Address: "10.0.0.9:8080"})
	require.NoError(t, err)

	code, _, stderr = gridctl(t, "import", "--from", snap, "--to", url, "--allow-nonempty", "--lock-timeout", "300ms")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "writer lease")
	assert.Contains(t, stderr, "grid-task-1")
	assert.NotContains(t, stderr, url)

	code, out, _ := gridctl(t, "verify", "--from", snap, "--to", url)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "RESULT: OK", "verify needs no lease")

	require.NoError(t, holder.ReleaseLease())
	code, out, stderr = gridctl(t, "import", "--from", snap, "--to", url, "--allow-nonempty")
	require.Equal(t, 0, code, out+stderr)
	assert.Contains(t, out, "RESULT: OK")
}
