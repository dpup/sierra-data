package sqliteimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ManifestFormat identifies the manifest JSON written beside a snapshot.
const ManifestFormat = "gridctl-snapshot/1"

// ManifestSuffix is appended to a snapshot's path (or S3 key) for its manifest.
const ManifestSuffix = ".manifest.json"

// Manifest describes one snapshot file. The importer checks the file against
// SHA256 and Size before reading it, and the imported row counts against
// Tables.
type Manifest struct {
	Format         string           `json:"format"`
	CreatedAt      time.Time        `json:"createdAt"`
	Source         string           `json:"source"`            // the live file it was taken from
	SourceJournal  string           `json:"sourceJournalMode"` // as reported, for the record
	Size           int64            `json:"size"`
	SHA256         string           `json:"sha256"`
	SchemaVersion  int              `json:"schemaVersion"`
	SQLiteVersion  string           `json:"sqliteVersion"`
	QuickCheck     string           `json:"quickCheck"`
	Tables         map[string]int64 `json:"tables"`
	GridctlVersion string           `json:"gridctlVersion"`
}

// ReadManifest parses a manifest file.
func ReadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("sqliteimport: parsing manifest %s: %w", path, err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("sqliteimport: manifest %s has format %q, want %q", path, m.Format, ManifestFormat)
	}
	return &m, nil
}

// WriteManifest writes m as indented JSON.
func WriteManifest(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// FileSHA256 hashes a file.
func FileSHA256(path string) (sum string, size int64, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer fh.Close()
	h := sha256.New()
	size, err = io.Copy(h, fh)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// SnapshotOptions configure Snapshot.
type SnapshotOptions struct {
	BusyTimeout time.Duration // 0 = DefaultBusyTimeout
	Version     string        // gridctl version for the manifest
}

// Snapshot writes a consistent copy of the SQLite database at src to out (a
// new file) with VACUUM INTO through a read-only connection, then describes
// the copy (hash, row counts, quick_check) in a Manifest written to
// out+ManifestSuffix.
//
// VACUUM INTO reads the source inside one read transaction, so the copy is a
// single consistent state even while the legacy writer is live. On a
// rollback-journal database that shared lock makes the writer's commits wait
// (up to its own busy_timeout) for the length of the copy: tens of seconds
// for the production file. A failed poll tick writes nothing and the next one
// recovers, but run it off-peak or after the writer has stopped.
//
// The source is never written: the connection is mode=ro.
func Snapshot(ctx context.Context, src, out string, opts SnapshotOptions) (*Manifest, error) {
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = DefaultBusyTimeout
	}
	if _, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("sqliteimport: source: %w", err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(outAbs); err == nil {
		return nil, fmt.Errorf("sqliteimport: %s already exists; a snapshot never overwrites", outAbs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := openReadOnly(src, opts.BusyTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return nil, fmt.Errorf("sqliteimport: reading journal mode: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, outAbs); err != nil {
		os.Remove(outAbs)
		return nil, fmt.Errorf("sqliteimport: VACUUM INTO %s: %w", outAbs, err)
	}
	m, err := Describe(ctx, outAbs, opts.Version)
	if err != nil {
		os.Remove(outAbs)
		return nil, err
	}
	m.Source = src
	m.SourceJournal = journal
	if err := WriteManifest(outAbs+ManifestSuffix, m); err != nil {
		os.Remove(outAbs)
		return nil, err
	}
	return m, nil
}

// Describe builds the manifest of a database file that nothing is writing
// (a snapshot): hash, size, schema version, row counts and quick_check. It
// fails unless quick_check says "ok" and the schema is the version the
// importer reads.
func Describe(ctx context.Context, path, version string) (*Manifest, error) {
	db, err := openReadOnly(path, DefaultBusyTimeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	m := &Manifest{Format: ManifestFormat, CreatedAt: time.Now().UTC().Truncate(time.Second), GridctlVersion: version}
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&m.QuickCheck); err != nil {
		return nil, fmt.Errorf("sqliteimport: quick_check: %w", err)
	}
	if m.QuickCheck != "ok" {
		return nil, fmt.Errorf("sqliteimport: quick_check of %s: %s", path, m.QuickCheck)
	}
	if err := db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&m.SQLiteVersion); err != nil {
		return nil, err
	}
	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := checkSourceSchema(ctx, db, tables); err != nil {
		return nil, err
	}
	m.SchemaVersion = SourceSchemaVersion
	if m.Tables, err = countTables(ctx, db, tables); err != nil {
		return nil, err
	}
	if m.SHA256, m.Size, err = FileSHA256(path); err != nil {
		return nil, err
	}
	return m, nil
}
