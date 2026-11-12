package sqliteimport

import (
	"strings"
	"time"
)

// The legacy SQLite schema version the importer reads: the last rung of the
// ladder on the legacy-efs-final tag (store.go migrationV14).
const SourceSchemaVersion = 14

// kind is a column's canonical type: what both sides are converted to before
// they are compared and hashed. It follows the PostgreSQL column type, so a
// SQLite value that cannot be represented exactly fails the import instead of
// being rounded.
type kind uint8

const (
	kInt   kind = iota + 1 // integer / bigint
	kFloat                 // double precision
	kText                  // text
	kBytes                 // bytea
)

type column struct {
	name string
	kind kind
}

// table describes how one table moves from SQLite to PostgreSQL and how the
// two sides are compared.
//
// The canonical row is cols, in that order, on both sides. The first keyN
// columns are the primary key; both sides are read ORDER BY those columns
// (SQLite BINARY collation and PostgreSQL "C" collation agree on byte order,
// which is part of what the comparison proves).
type table struct {
	name string // PostgreSQL table (and, except schema_migrations, the SQLite one)
	cols []column
	keyN int

	// srcHash reads the canonical rows from SQLite in key order. Defaults to
	// SELECT <cols> FROM <name> ORDER BY <key>.
	srcHash string
	// srcCopy is the copy pass's SQLite query; defaults to srcHash. It may
	// return extra trailing columns that toPG consumes.
	srcCopy string
	// pgCols is the COPY column list; defaults to cols.
	pgCols []string
	// toPG maps one canonical srcCopy row to a COPY row; nil copies the
	// canonical row as is.
	toPG func(row []any) ([]any, error)
	// pgHash reads the canonical rows (plus any extra columns check uses)
	// from PostgreSQL in key order. Defaults to the srcHash shape.
	pgHash string
	// pgCount counts the PostgreSQL rows that belong to the import; defaults
	// to SELECT count(*) FROM <name>.
	pgCount string
	// extraKinds types the extra trailing columns of srcCopy (copy pass) and
	// of pgHash (verification pass), which are not part of the canonical row.
	srcCopyExtra []kind
	pgHashExtra  []kind
	// check validates one PostgreSQL row (canonical columns then extras):
	// the blob rehydrates and the index columns agree with it.
	check func(c *checker, row []any)
}

func (t *table) colNames() []string {
	out := make([]string, len(t.cols))
	for i, c := range t.cols {
		out[i] = c.name
	}
	return out
}

func (t *table) keyList() string { return strings.Join(t.colNames()[:t.keyN], ", ") }

func (t *table) defaultSelect() string {
	return "SELECT " + strings.Join(t.colNames(), ", ") + " FROM " + t.name + " ORDER BY " + t.keyList()
}

func (t *table) sqliteHashSQL() string {
	if t.srcHash != "" {
		return t.srcHash
	}
	return t.defaultSelect()
}

func (t *table) sqliteCopySQL() string {
	if t.srcCopy != "" {
		return t.srcCopy
	}
	return t.sqliteHashSQL()
}

func (t *table) pgHashSQL() string {
	if t.pgHash != "" {
		return t.pgHash
	}
	return t.defaultSelect()
}

func (t *table) pgCountSQL() string {
	if t.pgCount != "" {
		return t.pgCount
	}
	return "SELECT count(*) FROM " + t.name
}

func (t *table) copyCols() []string {
	if t.pgCols != nil {
		return t.pgCols
	}
	return t.colNames()
}

func (t *table) kinds(extra []kind) []kind {
	out := make([]kind, 0, len(t.cols)+len(extra))
	for _, c := range t.cols {
		out = append(out, c.kind)
	}
	return append(out, extra...)
}

func i(name string) column { return column{name, kInt} }
func f(name string) column { return column{name, kFloat} }
func s(name string) column { return column{name, kText} }
func b(name string) column { return column{name, kBytes} }

// Tables the importer reads from SQLite but deliberately does not carry over
// (the PostgreSQL baseline dropped them). event_geo is an R*Tree virtual table
// and its shadow tables; the bbox now lives on events, derived from the blob.
var skippedTables = map[string]string{
	"event_geo":        "R*Tree, replaced by the events bbox columns (derived from the blob)",
	"event_geo_map":    "R*Tree id map, dropped with event_geo",
	"event_geo_node":   "R*Tree shadow table",
	"event_geo_parent": "R*Tree shadow table",
	"event_geo_rowid":  "R*Tree shadow table",
	"subscriptions":    "never used; dropped by the baseline",
}

// importTables lists every imported table in dependency (copy) order.
var importTables = []*table{
	{
		name: "sources",
		cols: []column{s("id"), s("name"), s("attribution"), s("homepage_url"), i("kind"),
			i("poll_interval_seconds"), i("stale_after_seconds"), i("expire_after_seconds"),
			i("last_success_at"), i("last_attempt_at"), s("last_error"), i("status"), s("disappearance")},
		keyN: 1,
	},
	{
		name:  "places",
		cols:  []column{s("id"), i("kind"), s("name"), s("slug"), s("parent_id"), b("proto")},
		keyN:  1,
		check: checkPlace,
	},
	eventsTable,
	{
		name: "event_revisions",
		cols: []column{s("event_id"), i("revision"), i("observed_at"), i("ingested_at"), b("proto")},
		keyN: 2,
		// Copied in insertion (rowid) order so the new identity column
		// (event_revisions.id, GENERATED ALWAYS) numbers revisions in the
		// order they were written; compared in key order.
		srcCopy: "SELECT event_id, revision, observed_at, ingested_at, proto FROM event_revisions ORDER BY rowid",
		check:   checkRevision,
	},
	{
		name: "event_places",
		cols: []column{s("event_id"), s("place_id")},
		keyN: 2,
	},
	{
		name:  "event_lite",
		cols:  []column{s("event_id"), b("proto")},
		keyN:  1,
		check: checkLite,
	},
	{
		// ids preserved: the compaction watermark in mesh_meta refers to them.
		name: "mesh_observations",
		cols: []column{i("id"), s("pubkey"), i("heard_at"), s("broker"), s("gateway"), f("snr"),
			i("rssi"), i("hop_count"), s("path"), s("path_nodes")},
		keyN: 1,
	},
	{
		name: "mesh_link_rollup",
		cols: []column{s("a_pubkey"), s("b_pubkey"), i("bucket"), i("observations"), f("best_snr"),
			i("first_seen"), i("last_seen")},
		keyN: 3,
	},
	{
		name: "mesh_meta",
		cols: []column{s("key"), i("value")},
		keyN: 1,
	},
	{
		name: "mesh_telemetry",
		cols: []column{s("pubkey"), i("reported_at"), i("received_at"), s("reporter"),
			i("last_success_at"), i("last_attempt_at"),
			f("battery_volts"), f("battery_pct"), s("battery_pct_source"), f("temperature_c"),
			f("humidity"), f("pressure"), i("noise_floor_dbm"), f("last_snr_db"), i("last_rssi_dbm"),
			i("tx_queue_len"),
			i("uptime_s"), i("airtime_ms"), i("rx_airtime_ms"), i("packets_sent"), i("packets_recv"),
			i("sent_flood"), i("sent_direct"), i("recv_flood"), i("recv_direct"), i("direct_dups"),
			i("flood_dups"), i("full_evts"), i("recv_errors")},
		keyN: 2,
	},
	{
		name: "mesh_activity",
		cols: []column{s("pubkey"), i("last_heard_at"), i("last_relayed_at")},
		keyN: 1,
	},
	{
		name: "burn_readings",
		cols: []column{s("line_id"), s("status"), s("message"), s("transcript"), i("confidence"),
			i("observed_at"), i("received_at"), s("reporter_id")},
		keyN: 1,
	},
	{
		name: "road_samples",
		cols: []column{s("road_id"), i("at"), i("status"), i("congestion"), i("duration_m"), i("delay_m")},
		keyN: 2,
	},
	{
		// The legacy ladder (1..14) is recorded next to the PostgreSQL
		// versions under source = 'sqlite' (the key is (source, version)).
		name:    "schema_migrations",
		cols:    []column{i("version"), i("applied_at")},
		keyN:    1,
		srcHash: "SELECT version, applied_at FROM schema_migrations ORDER BY version",
		pgCols:  []string{"source", "version", "applied_at"},
		toPG: func(row []any) ([]any, error) {
			at, _ := row[1].(int64)
			return []any{"sqlite", row[0], time.Unix(at, 0).UTC()}, nil
		},
		pgHash: `SELECT version::bigint, extract(epoch FROM applied_at)::bigint FROM schema_migrations
			WHERE source = 'sqlite' ORDER BY version`,
		pgCount: "SELECT count(*) FROM schema_migrations WHERE source = 'sqlite'",
	},
}

// eventsCols are the events columns SQLite has; PostgreSQL adds
// first_ingested_at and the bbox, derived during the copy.
var eventsCols = []column{s("id"), i("layer"), i("severity"), i("status"), s("source_id"),
	i("effective"), i("expires"), i("observed_at"), i("ingested_at"), i("last_seen_at"),
	i("revision"), i("refreshes"), s("content_hash"), b("proto")}

var eventsDerived = []string{"first_ingested_at", "min_lat", "max_lat", "min_lng", "max_lng"}

var eventsTable = func() *table {
	t := &table{name: "events", cols: eventsCols, keyN: 1, check: checkEvent}
	names := t.colNames()
	sel := make([]string, len(names))
	for n, c := range names {
		sel[n] = "e." + c
	}
	// first_ingested_at is when revision 1 was stored. An event without a
	// revision 1 (not expected; reported) falls back to its earliest
	// revision, then to its own ingested_at.
	t.srcCopy = "SELECT " + strings.Join(sel, ", ") + `,
		COALESCE(
		  (SELECT r.ingested_at FROM event_revisions r WHERE r.event_id = e.id AND r.revision = 1),
		  (SELECT MIN(r.ingested_at) FROM event_revisions r WHERE r.event_id = e.id),
		  e.ingested_at)
		FROM events e ORDER BY e.id`
	t.srcCopyExtra = []kind{kInt}
	t.pgCols = append(append([]string{}, names...), eventsDerived...)
	t.toPG = eventToPG
	t.pgHash = "SELECT " + strings.Join(names, ", ") + ", " + strings.Join(eventsDerived, ", ") +
		" FROM events ORDER BY id"
	t.pgHashExtra = []kind{kInt, kFloat, kFloat, kFloat, kFloat}
	return t
}()
