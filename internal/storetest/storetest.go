// Package storetest gives each test its own PostgreSQL database.
//
// TEST_DATABASE_URL names an admin connection (any database the user can
// connect to; `make test` points it at the Docker server from
// docker-compose.test.yml). When it is unset, every caller is skipped with a
// loud message, so plain `go test ./...` stays Docker-free.
//
// The schema is migrated ONCE into a template database (sierra_template); a
// test then gets `CREATE DATABASE t_<random> TEMPLATE sierra_template`, which
// is a file copy, far cheaper than migrating, and the database is dropped
// WITH (FORCE) when the test ends. Databases are created with C collation,
// like production must be. Advisory locks are per database, so writer-lock
// tests in different databases never contend and t.Parallel() stays safe.
//
// The template is keyed on a hash of the migration files (stored as the
// database comment), so editing a migration rebuilds it on the next run.
package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the migration runner

	"github.com/sierragridteam/sierra-data/internal/store/migrations"
)

// EnvVar names the admin connection URL.
const EnvVar = "TEST_DATABASE_URL"

// TemplateName is the migrated template every test database is copied from.
const TemplateName = "sierra_template"

// templateLockKey serializes template (re)builds across test binaries: go
// test runs packages in parallel, and each would otherwise race to build it.
const templateLockKey int64 = 0x5349455252415431 // "SIERRAT1"

var (
	templateOnce sync.Once
	templateErr  error
)

// URL returns a connection URL for a fresh, migrated, empty database that is
// dropped when t ends. It skips t when TEST_DATABASE_URL is unset.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		t.Skip("SKIP: TEST_DATABASE_URL unset; run make db-up (make test sets it)")
	}
	templateOnce.Do(func() { templateErr = ensureTemplate(admin) })
	if templateErr != nil {
		t.Fatalf("storetest: preparing %s: %v", TemplateName, templateErr)
	}
	name := "t_" + randomHex(8)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := withAdmin(ctx, admin, func(c *pgx.Conn) error {
		return createFromTemplate(ctx, c, name)
	}); err != nil {
		t.Fatalf("storetest: creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := withAdmin(ctx, admin, func(c *pgx.Conn) error {
			_, err := c.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
			return err
		}); err != nil {
			t.Logf("storetest: dropping %s: %v", name, err)
		}
	})
	return withDatabase(admin, name)
}

// EmptyURL is URL without the schema: a fresh database created from
// template0 (C collation), for tests of the migration runner itself.
func EmptyURL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		t.Skip("SKIP: TEST_DATABASE_URL unset; run make db-up (make test sets it)")
	}
	name := "t_" + randomHex(8)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := withAdmin(ctx, admin, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()+
			` TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C' ENCODING 'UTF8'`)
		return err
	}); err != nil {
		t.Fatalf("storetest: creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = withAdmin(ctx, admin, func(c *pgx.Conn) error {
			_, err := c.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
			return err
		})
	})
	return withDatabase(admin, name)
}

// AdminURL returns TEST_DATABASE_URL, skipping t when it is unset.
func AdminURL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		t.Skip("SKIP: TEST_DATABASE_URL unset; run make db-up (make test sets it)")
	}
	return admin
}

func createFromTemplate(ctx context.Context, c *pgx.Conn, name string) error {
	stmt := `CREATE DATABASE ` + pgx.Identifier{name}.Sanitize() + ` TEMPLATE ` + TemplateName +
		` LC_COLLATE 'C' LC_CTYPE 'C' ENCODING 'UTF8'`
	var err error
	// Concurrent copies of one template can collide on its lock ("source
	// database is being accessed by other users") for a moment; retry.
	for attempt := 0; attempt < 50; attempt++ {
		if _, err = c.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "being accessed by other users") {
			return err
		}
		time.Sleep(time.Duration(20+attempt*10) * time.Millisecond)
	}
	return err
}

// ensureTemplate builds sierra_template if it is missing or was built from
// different migration files. Runs under a session advisory lock on the admin
// database, so concurrent test binaries build it once.
func ensureTemplate(admin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	want := migrationsHash()
	return withAdmin(ctx, admin, func(c *pgx.Conn) error {
		if _, err := c.Exec(ctx, `SELECT pg_advisory_lock($1)`, templateLockKey); err != nil {
			return err
		}
		defer c.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, templateLockKey)

		var have *string
		err := c.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`,
			TemplateName).Scan(&have)
		if err == nil && have != nil && *have == want {
			return nil
		}
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		tmpl := pgx.Identifier{TemplateName}.Sanitize()
		if err == nil {
			if _, err := c.Exec(ctx, `ALTER DATABASE `+tmpl+` IS_TEMPLATE false`); err != nil {
				return err
			}
			if _, err := c.Exec(ctx, `DROP DATABASE `+tmpl+` WITH (FORCE)`); err != nil {
				return err
			}
		}
		if _, err := c.Exec(ctx, `CREATE DATABASE `+tmpl+
			` TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C' ENCODING 'UTF8'`); err != nil {
			return err
		}
		db, err := sql.Open("pgx", withDatabase(admin, TemplateName))
		if err != nil {
			return err
		}
		_, merr := migrations.Apply(ctx, db)
		db.Close()
		if merr != nil {
			return merr
		}
		if _, err := c.Exec(ctx, `ALTER DATABASE `+tmpl+` IS_TEMPLATE true`); err != nil {
			return err
		}
		_, err = c.Exec(ctx, `COMMENT ON DATABASE `+tmpl+` IS `+quoteLiteral(want))
		return err
	})
}

func migrationsHash() string {
	h := sha256.New()
	for _, m := range migrations.All() {
		fmt.Fprintf(h, "%d\x00%s\x00%s\x00", m.Version, m.Name, m.SQL)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func withAdmin(ctx context.Context, admin string, fn func(*pgx.Conn) error) error {
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w (is the database up? make db-up)", EnvVar, err)
	}
	defer c.Close(context.Background())
	return fn(c)
}

// withDatabase returns admin with its database name replaced.
func withDatabase(admin, name string) string {
	u, err := url.Parse(admin)
	if err != nil || u.Scheme == "" {
		// A keyword/value DSN: a later dbname wins.
		return admin + " dbname=" + name
	}
	u.Path = "/" + name
	return u.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
