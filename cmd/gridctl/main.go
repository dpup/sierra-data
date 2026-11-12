// Command gridctl carries the legacy SQLite Grid store into PostgreSQL
// (migration plan D11): snapshot a live SQLite file, import a snapshot in one
// verified transaction, and verify an imported database. It runs locally or
// as a one-off ECS task (the image ships it as /app/gridctl).
//
//	gridctl snapshot --db /data/grid.db --out s3://bucket/grid-<ts>.db
//	gridctl import   --from s3://bucket/grid-<ts>.db [--to <postgres url>] [--allow-nonempty]
//	gridctl verify   --from <snapshot path or s3://> [--to <postgres url>]
//	gridctl version
//
// --to defaults to $PF__GRID__DATABASE__URL, the variable the Grid task's
// secret injects, so the URL never has to appear on a command line. It is
// never printed.
//
// import is a writer: it takes the Grid writer lease (store.AcquireLease, the
// path a server's campaign uses) and runs its transaction under the lease's
// epoch fence. If a server holds the lease for --lock-timeout (default 10s)
// it refuses, --allow-nonempty or not; stop, yield or disable that writer
// first. verify takes no lock (read-only).
//
// Exit status: 0 success, 1 failure (for import and verify: any difference;
// the import is rolled back), 2 usage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/sierragridteam/sierra-data/internal/sqliteimport"
)

// version is set at build time (-ldflags "-X main.version=..."); the VCS
// revision from the build info is the fallback.
var version = ""

func gridctlVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: gridctl <command> [flags]

commands:
  snapshot  consistent read-only copy of a live SQLite Grid store, plus a manifest
  import    load a snapshot into PostgreSQL in one verified transaction
  verify    compare an imported PostgreSQL database with a snapshot (read-only)
  version   print the gridctl version

run "gridctl <command> -h" for a command's flags.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "snapshot":
		err = cmdSnapshot(ctx, args[1:], stdout, stderr)
	case "import":
		err = cmdImport(ctx, args[1:], stdout, stderr)
	case "verify":
		err = cmdVerify(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, gridctlVersion())
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "gridctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "gridctl %s: %v\n", args[0], err)
		return 2
	default:
		fmt.Fprintf(stderr, "gridctl %s: %v\n", args[0], err)
		return 1
	}
}

type usageError struct{ error }

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("gridctl "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected arguments %v", fs.Args())}
	}
	return nil
}

func cmdSnapshot(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("snapshot", stderr)
	db := fs.String("db", "", "the live SQLite file (opened read-only), e.g. /data/grid.db")
	out := fs.String("out", "", "destination: a new local path or s3://bucket/key (the manifest goes to <out>.manifest.json)")
	busy := fs.Duration("busy-timeout", sqliteimport.DefaultBusyTimeout, "how long to wait on the live writer's lock")
	tmp := fs.String("tmp", "", "scratch directory for an s3:// destination (default $TMPDIR)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *db == "" || *out == "" {
		return usageError{errors.New("--db and --out are required")}
	}
	dst, err := parseLocation(*out)
	if err != nil {
		return usageError{fmt.Errorf("--out: %w", err)}
	}

	local := dst.raw
	if dst.isS3() {
		dir, err := os.MkdirTemp(*tmp, "gridctl-snapshot-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		local = filepath.Join(dir, filepath.Base(dst.key))
	}
	start := time.Now()
	m, err := sqliteimport.Snapshot(ctx, *db, local, sqliteimport.SnapshotOptions{BusyTimeout: *busy, Version: gridctlVersion()})
	if err != nil {
		return err
	}
	took := time.Since(start)
	if dst.isS3() {
		store, err := newObjectStore(ctx)
		if err != nil {
			return err
		}
		// The database first: a manifest's presence means the upload is complete.
		if err := store.Put(ctx, dst.bucket, dst.key, local); err != nil {
			return err
		}
		mdst := dst.withSuffix(sqliteimport.ManifestSuffix)
		if err := store.Put(ctx, mdst.bucket, mdst.key, local+sqliteimport.ManifestSuffix); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "snapshot %s -> %s\n  %d bytes, sha256 %s, schema v%d, quick_check %s, %.1fs\n",
		*db, dst, m.Size, m.SHA256, m.SchemaVersion, m.QuickCheck, took.Seconds())
	for _, t := range sortedKeys(m.Tables) {
		fmt.Fprintf(stdout, "  %-20s %10d\n", t, m.Tables[t])
	}
	return nil
}

// fetched is a snapshot (and maybe its manifest) available as local files.
type fetched struct {
	path     string
	manifest *sqliteimport.Manifest
	cleanup  func()
}

// fetchSource resolves --from and --manifest. manifestFlag is "" (the
// default <from>.manifest.json, required), "none", or an explicit location.
func fetchSource(ctx context.Context, from, manifestFlag, tmp string) (*fetched, error) {
	src, err := parseLocation(from)
	if err != nil {
		return nil, usageError{fmt.Errorf("--from: %w", err)}
	}
	f := &fetched{path: src.raw, cleanup: func() {}}
	mloc := src.withSuffix(sqliteimport.ManifestSuffix)
	switch manifestFlag {
	case "none":
		mloc = location{}
	case "":
	default:
		if mloc, err = parseLocation(manifestFlag); err != nil {
			return nil, usageError{fmt.Errorf("--manifest: %w", err)}
		}
	}

	var store objectStore
	var dir string
	needS3 := src.isS3() || mloc.isS3()
	if needS3 {
		if store, err = newObjectStore(ctx); err != nil {
			return nil, err
		}
		if dir, err = os.MkdirTemp(tmp, "gridctl-import-"); err != nil {
			return nil, err
		}
		f.cleanup = func() { os.RemoveAll(dir) }
	}
	fail := func(err error) (*fetched, error) {
		f.cleanup()
		return nil, err
	}
	if mloc.raw != "" {
		mpath := mloc.raw
		if mloc.isS3() {
			mpath = filepath.Join(dir, "manifest.json")
			if err := store.Get(ctx, mloc.bucket, mloc.key, mpath); err != nil {
				return fail(fmt.Errorf("%w (pass --manifest none to import without one)", err))
			}
		}
		if f.manifest, err = sqliteimport.ReadManifest(mpath); err != nil {
			return fail(fmt.Errorf("%w (pass --manifest none to import without one)", err))
		}
	}
	if src.isS3() {
		f.path = filepath.Join(dir, filepath.Base(src.key))
		if err := store.Get(ctx, src.bucket, src.key, f.path); err != nil {
			return fail(err)
		}
	}
	return f, nil
}

func targetURL(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("PF__GRID__DATABASE__URL"); v != "" {
		return v, nil
	}
	return "", usageError{errors.New("--to is required (or set PF__GRID__DATABASE__URL)")}
}

func cmdImport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("import", stderr)
	from := fs.String("from", "", "the snapshot: a local path or s3://bucket/key")
	to := fs.String("to", "", "PostgreSQL URL (default $PF__GRID__DATABASE__URL); the database must be C-collated")
	manifest := fs.String("manifest", "", `the snapshot's manifest (default <from>.manifest.json; "none" to import without one)`)
	allow := fs.Bool("allow-nonempty", false, "replace existing Grid data (rehearsal re-runs only); the replacement is part of the one transaction")
	report := fs.String("report", "", "also write the JSON report to this file")
	tmp := fs.String("tmp", "", "scratch directory for an s3:// source (default $TMPDIR)")
	lockTimeout := fs.Duration("lock-timeout", 10*time.Second,
		"how long to wait for the Grid writer lease (refused if a server still holds it) and the table locks")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *from == "" {
		return usageError{errors.New("--from is required")}
	}
	url, err := targetURL(*to)
	if err != nil {
		return err
	}
	src, err := fetchSource(ctx, *from, *manifest, *tmp)
	if err != nil {
		return err
	}
	defer src.cleanup()
	if src.manifest == nil {
		fmt.Fprintln(stderr, "gridctl import: no manifest; row counts are compared with the source file only")
	}
	r, ierr := sqliteimport.Import(ctx, sqliteimport.ImportOptions{
		SourcePath: src.path, SourceLabel: *from, Manifest: src.manifest, TargetURL: url,
		AllowNonEmpty: *allow, Version: gridctlVersion(), Log: stderr, LockTimeout: *lockTimeout})
	if r != nil {
		printReport(stdout, r, ierr == nil)
		if err := writeReport(*report, r); err != nil {
			return err
		}
	}
	return ierr
}

func cmdVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", stderr)
	from := fs.String("from", "", "the snapshot that was imported: a local path or s3://bucket/key")
	to := fs.String("to", "", "PostgreSQL URL (default $PF__GRID__DATABASE__URL)")
	manifest := fs.String("manifest", "", `the snapshot's manifest (default <from>.manifest.json; "none" to skip)`)
	report := fs.String("report", "", "also write the JSON report to this file")
	tmp := fs.String("tmp", "", "scratch directory for an s3:// source (default $TMPDIR)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *from == "" {
		return usageError{errors.New("--from is required")}
	}
	url, err := targetURL(*to)
	if err != nil {
		return err
	}
	src, err := fetchSource(ctx, *from, *manifest, *tmp)
	if err != nil {
		return err
	}
	defer src.cleanup()
	r, verr := sqliteimport.Verify(ctx, src.path, src.manifest, url)
	if r != nil {
		printReport(stdout, r, verr == nil)
		if err := writeReport(*report, r); err != nil {
			return err
		}
	}
	return verr
}

func writeReport(path string, r *sqliteimport.Report) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func printReport(w io.Writer, r *sqliteimport.Report, ok bool) {
	fmt.Fprintf(w, "source %s (%d bytes, sha256 %s)\n", r.Source, r.SourceSize, r.SourceSHA256)
	fmt.Fprintf(w, "%-20s %10s %10s %8s %8s  %s\n", "table", "source", "target", "copy s", "check s", "sha256 (target)")
	for _, t := range r.Tables {
		mark := ""
		if t.SourceSHA256 != t.TargetSHA256 || t.CheckFailures > 0 || t.SourceRows != t.TargetRows {
			mark = "  DIFFERS"
		}
		fmt.Fprintf(w, "%-20s %10d %10d %8.2f %8.2f  %.16s%s\n", t.Table, t.SourceRows, t.TargetRows,
			t.CopySeconds, t.VerifySeconds, t.TargetSHA256, mark)
		for _, d := range t.Differences {
			fmt.Fprintf(w, "    diff: %s\n", d)
		}
		for _, d := range t.CheckExamples {
			fmt.Fprintf(w, "    check: %s\n", d)
		}
	}
	for _, name := range sortedKeys(r.Skipped) {
		fmt.Fprintf(w, "skipped %s (%d rows)\n", name, r.Skipped[name])
	}
	for _, name := range sortedKeys(r.Checks) {
		fmt.Fprintf(w, "check %-26s %s\n", name, r.Checks[name])
	}
	for _, name := range sortedKeys(r.Info) {
		fmt.Fprintf(w, "info  %-36s %d\n", name, r.Info[name])
	}
	fmt.Fprintf(w, "copy %.1fs, verify %.1fs, total %.1fs\n", r.CopySeconds, r.VerifySeconds,
		r.FinishedAt.Sub(r.StartedAt).Seconds())
	if ok {
		fmt.Fprintln(w, "RESULT: OK")
		return
	}
	fmt.Fprintf(w, "RESULT: FAILED (%d problems)\n", len(r.Problems))
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  - %s\n", p)
	}
}
