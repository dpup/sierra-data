package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// maxTelemetryRows bounds one MeshTelemetry read. A year of 15-minute reports
// for one node is ~35k rows; a chart cannot draw that and a caller asking for it
// wants a narrower window or (eventually) a rollup. The read reports truncation
// rather than silently returning a prefix — a chart drawn from a silently
// truncated series is a chart that lies about when the data stops.
const maxTelemetryRows = 20000

// MeshTelemetrySample is ONE report a monitor made about ONE node — an
// immutable measurement, not event content.
//
// The nullable/plain split is the same one grid.v1.MeshAdminTelemetry makes and
// for the same reason: a gauge the monitor could not read is ABSENT (nil), never
// zero, while a counter is only ever written alongside a successful read, where
// zero is a real measurement. Copying that split here rather than flattening it
// is what keeps "battery unreadable" from becoming "battery flat" somewhere
// between the wire and a chart.
type MeshTelemetrySample struct {
	PubKey     string
	ReportedAt time.Time // monitor's stamp, clamped on arrival; the dedupe key
	ReceivedAt time.Time // our clock
	Reporter   string
	// The monitor's own stamps at the moment of this report. Zero when it sent
	// none — "never reached" is a real state (see the reporter guide's rule 3).
	LastSuccessAt time.Time
	LastAttemptAt time.Time

	// Gauges — nil means "not read".
	BatteryVolts     *float64
	BatteryPct       *float64
	BatteryPctSource string
	TemperatureC     *float64
	Humidity         *float64
	Pressure         *float64
	NoiseFloorDBm    *int64
	LastSNRdB        *float64
	LastRSSIdBm      *int64
	TxQueueLen       *int64

	// Counters — lifetime since the node booted.
	UptimeS     int64
	AirtimeMs   int64
	RxAirtimeMs int64
	PacketsSent int64
	PacketsRecv int64
	SentFlood   int64
	SentDirect  int64
	RecvFlood   int64
	RecvDirect  int64
	DirectDups  int64
	FloodDups   int64
	FullEvts    int64
	RecvErrors  int64
}

// telemetryColumns is the column order shared by the insert and every read, so
// the two cannot drift apart silently.
const telemetryColumns = `pubkey, reported_at, received_at, reporter, last_success_at, last_attempt_at,
	battery_volts, battery_pct, battery_pct_source, temperature_c, humidity, pressure,
	noise_floor_dbm, last_snr_db, last_rssi_dbm, tx_queue_len,
	uptime_s, airtime_ms, rx_airtime_ms, packets_sent, packets_recv,
	sent_flood, sent_direct, recv_flood, recv_direct,
	direct_dups, flood_dups, full_evts, recv_errors`

// InsertMeshTelemetry appends a batch of samples in one transaction, on the
// single writer goroutine (the scheduler drains them from a PollResult, exactly
// as it does mesh observations).
//
// INSERT OR IGNORE, not upsert: the poller re-offers the same sample on every
// tick until the monitor files a new report, and a sample is immutable once
// written. Re-offering is the normal case, not an error — roughly 14 of every
// 15 rows offered here are already present.
//
// A monitor can also report more often than it reads: the SIERRA relay reads
// a shared repeater every 30 minutes and reports every 5, so each report
// carries a new reported_at around the SAME reading. A sample whose
// last_success_at equals the one in the node's previous row from that reporter
// is that repeat, and is skipped: one reading stays one row, and the cadence a
// chart sees is the read cadence. Samples without a last_success_at can't be
// told apart this way and are kept.
//
// A monitor can also re-send a CACHED reading as a fresh one: a new
// reported_at and last_success_at around the same uptime and packet counters
// (seen 2026-11-10 on Arnold Summit, BigPratherMeadow, Lake Alpine and Forest
// Meadows). A node's uptime advances between any two real reads, so a sample
// whose uptime and packet counters equal the previous row's is that repeat
// and is skipped too; charts then show the gap the monitor really had.
func (s *Store) InsertMeshTelemetry(ctx context.Context, samples []MeshTelemetrySample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Drop what a series cannot use before anything is looked up.
		valid := make([]MeshTelemetrySample, 0, len(samples))
		for _, t := range samples {
			if t.PubKey != "" && !t.ReportedAt.IsZero() {
				valid = append(valid, t)
			}
		}
		for start := 0; start < len(valid); start += telemetryChunk {
			if err := insertTelemetryChunk(ctx, tx, valid[start:min(start+telemetryChunk, len(valid))]); err != nil {
				return err
			}
		}
		return nil
	})
}

// telemetryChunk bounds one INSERT: 29 parameters a row against PostgreSQL's
// 65,535, with headroom.
const telemetryChunk = 1000

// telemetryPrev is the row a sample is compared with: the node's previous
// sample from the same reporter.
type telemetryPrev struct {
	reportedAt  int64
	lastSuccess sql.NullInt64
	uptime      int64
	sent, recv  int64
}

// insertTelemetryChunk applies the skip rules (see InsertMeshTelemetry) to
// every sample against what precedes it, then inserts the survivors, in two
// round trips for the chunk instead of two per sample: one LATERAL lookup of
// every sample's previous stored row, one multi-row INSERT.
//
// "What precedes it" is the stored row with the greatest reported_at below the
// sample's, or an earlier sample of this same chunk that was inserted and is
// newer than that row: exactly what the per-sample loop saw, where each insert
// was visible to the next lookup. A tie goes to the stored row. A sample only
// counts as inserted if it can be: the primary key is (pubkey, reported_at)
// whatever the reporter, so a sample colliding with a stored row, or with an
// earlier one in the chunk, is dropped by ON CONFLICT and precedes nothing.
func insertTelemetryChunk(ctx context.Context, tx *sql.Tx, chunk []MeshTelemetrySample) error {
	n := len(chunk)
	pubkey, reporter, reported := make([]string, n), make([]string, n), make([]int64, n)
	for i, t := range chunk {
		pubkey[i], reporter[i], reported[i] = t.PubKey, t.Reporter, t.ReportedAt.Unix()
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.ord,
		       EXISTS (SELECT 1 FROM mesh_telemetry c WHERE c.pubkey = t.pubkey AND c.reported_at = t.reported_at),
		       p.reported_at, p.last_success_at, p.uptime_s, p.packets_sent, p.packets_recv
		FROM unnest($1::text[], $2::text[], $3::bigint[]) WITH ORDINALITY AS t(pubkey, reporter, reported_at, ord)
		LEFT JOIN LATERAL (
		  SELECT m.reported_at, m.last_success_at, m.uptime_s, m.packets_sent, m.packets_recv
		  FROM mesh_telemetry m
		  WHERE m.pubkey = t.pubkey AND m.reporter = t.reporter AND m.reported_at < t.reported_at
		  ORDER BY m.reported_at DESC LIMIT 1) p ON true`, pubkey, reporter, reported)
	if err != nil {
		return fmt.Errorf("store: looking up mesh telemetry predecessors: %w", err)
	}
	stored := make(map[int]telemetryPrev, n)
	collides := make(map[int]bool)
	for rows.Next() {
		var ord int
		var exists bool
		var at, up, sent, recv, last sql.NullInt64
		if err := rows.Scan(&ord, &exists, &at, &last, &up, &sent, &recv); err != nil {
			rows.Close()
			return fmt.Errorf("store: scanning mesh telemetry predecessor: %w", err)
		}
		collides[ord-1] = exists
		if at.Valid {
			stored[ord-1] = telemetryPrev{reportedAt: at.Int64, lastSuccess: last, uptime: up.Int64, sent: sent.Int64, recv: recv.Int64}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: looking up mesh telemetry predecessors: %w", err)
	}

	type seriesKey struct{ pubkey, reporter string }
	type pointKey struct {
		pubkey string
		at     int64
	}
	kept := map[seriesKey][]int{} // indexes into chunk of the samples inserted so far
	taken := map[pointKey]bool{}  // (pubkey, reported_at) inserted so far, any reporter
	var keep []MeshTelemetrySample
	for i, t := range chunk {
		prev, have := stored[i]
		for _, j := range kept[seriesKey{t.PubKey, t.Reporter}] {
			if r := chunk[j].ReportedAt.Unix(); r < reported[i] && (!have || r > prev.reportedAt) {
				prev, have = telemetryPrev{reportedAt: r, uptime: chunk[j].UptimeS,
					sent: chunk[j].PacketsSent, recv: chunk[j].PacketsRecv}, true
				if !chunk[j].LastSuccessAt.IsZero() {
					prev.lastSuccess = sql.NullInt64{Int64: chunk[j].LastSuccessAt.Unix(), Valid: true}
				}
			}
		}
		switch {
		case have && !t.LastSuccessAt.IsZero() && prev.lastSuccess.Valid && prev.lastSuccess.Int64 == t.LastSuccessAt.Unix():
			continue // the same read, reported again
		case have && t.UptimeS > 0 && t.UptimeS == prev.uptime && t.PacketsSent == prev.sent && t.PacketsRecv == prev.recv:
			continue // a cached reading re-sent as new: the node's clock did not move
		}
		pk := pointKey{t.PubKey, reported[i]}
		if collides[i] || taken[pk] {
			continue // ON CONFLICT DO NOTHING would drop it: it precedes nothing
		}
		taken[pk] = true
		keep = append(keep, t)
		k := seriesKey{t.PubKey, t.Reporter}
		kept[k] = append(kept[k], i)
	}
	if len(keep) == 0 {
		return nil
	}

	args := make([]any, 0, len(keep)*29)
	var values strings.Builder
	for i, t := range keep {
		if i > 0 {
			values.WriteString(", ")
		}
		values.WriteString("(" + params(i*29+1, 29) + ")")
		args = append(args,
			t.PubKey, t.ReportedAt.Unix(), t.ReceivedAt.Unix(), t.Reporter,
			unixTimeOrNil(t.LastSuccessAt), unixTimeOrNil(t.LastAttemptAt),
			t.BatteryVolts, t.BatteryPct, t.BatteryPctSource, t.TemperatureC, t.Humidity, t.Pressure,
			t.NoiseFloorDBm, t.LastSNRdB, t.LastRSSIdBm, t.TxQueueLen,
			t.UptimeS, t.AirtimeMs, t.RxAirtimeMs, t.PacketsSent, t.PacketsRecv,
			t.SentFlood, t.SentDirect, t.RecvFlood, t.RecvDirect,
			t.DirectDups, t.FloodDups, t.FullEvts, t.RecvErrors)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mesh_telemetry (`+telemetryColumns+`) VALUES `+
		values.String()+` ON CONFLICT (pubkey, reported_at) DO NOTHING`, args...); err != nil {
		return fmt.Errorf("store: inserting %d mesh telemetry samples: %w", len(keep), err)
	}
	return nil
}

// MeshTelemetry returns one node's samples in [from, to), oldest first, plus
// whether the result was truncated at maxTelemetryRows.
func (s *Store) MeshTelemetry(ctx context.Context, pubkey string, from, to time.Time) ([]MeshTelemetrySample, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+telemetryColumns+`
		FROM mesh_telemetry
		WHERE pubkey = $1 AND reported_at >= $2 AND reported_at < $3
		ORDER BY reported_at ASC
		LIMIT $4`, pubkey, from.Unix(), to.Unix(), maxTelemetryRows+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: querying mesh telemetry: %w", err)
	}
	defer rows.Close()

	var out []MeshTelemetrySample
	for rows.Next() {
		var (
			t                            MeshTelemetrySample
			reported, received           int64
			lastSuccess, lastAttempt     sql.NullInt64
			volts, pct, temp, hum, press sql.NullFloat64
			snr                          sql.NullFloat64
			noise, rssi, queue           sql.NullInt64
		)
		if err := rows.Scan(
			&t.PubKey, &reported, &received, &t.Reporter, &lastSuccess, &lastAttempt,
			&volts, &pct, &t.BatteryPctSource, &temp, &hum, &press,
			&noise, &snr, &rssi, &queue,
			&t.UptimeS, &t.AirtimeMs, &t.RxAirtimeMs, &t.PacketsSent, &t.PacketsRecv,
			&t.SentFlood, &t.SentDirect, &t.RecvFlood, &t.RecvDirect,
			&t.DirectDups, &t.FloodDups, &t.FullEvts, &t.RecvErrors,
		); err != nil {
			return nil, false, fmt.Errorf("store: scanning mesh telemetry: %w", err)
		}
		t.ReportedAt = time.Unix(reported, 0).UTC()
		t.ReceivedAt = time.Unix(received, 0).UTC()
		if lastSuccess.Valid {
			t.LastSuccessAt = time.Unix(lastSuccess.Int64, 0).UTC()
		}
		if lastAttempt.Valid {
			t.LastAttemptAt = time.Unix(lastAttempt.Int64, 0).UTC()
		}
		t.BatteryVolts, t.BatteryPct = nullFloat(volts), nullFloat(pct)
		t.TemperatureC, t.Humidity, t.Pressure = nullFloat(temp), nullFloat(hum), nullFloat(press)
		t.LastSNRdB = nullFloat(snr)
		t.NoiseFloorDBm, t.LastRSSIdBm, t.TxQueueLen = nullInt(noise), nullInt(rssi), nullInt(queue)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: iterating mesh telemetry: %w", err)
	}
	if len(out) > maxTelemetryRows {
		return out[:maxTelemetryRows], true, nil
	}
	return out, false, nil
}

// MeshTelemetryCoverage reports what we actually HOLD for a node: the oldest and
// newest sample, and how many.
//
// This is not a nicety. A chart that draws an empty range as a flat line, or as
// nothing at all, is asserting the node was quiet — when the truth may be that
// the range predates our retention. The caller ships this alongside the points
// so the difference is renderable. Zero samples returns a zero time pair.
func (s *Store) MeshTelemetryCoverage(ctx context.Context, pubkey string) (first, last time.Time, n int, err error) {
	var lo, hi sql.NullInt64
	row := s.db.QueryRowContext(ctx,
		`SELECT MIN(reported_at), MAX(reported_at), COUNT(*) FROM mesh_telemetry WHERE pubkey = $1`, pubkey)
	if err := row.Scan(&lo, &hi, &n); err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("store: mesh telemetry coverage: %w", err)
	}
	if lo.Valid {
		first = time.Unix(lo.Int64, 0).UTC()
	}
	if hi.Valid {
		last = time.Unix(hi.Int64, 0).UTC()
	}
	return first, last, n, nil
}

// PruneMeshTelemetry deletes samples older than cutoff. Returns rows removed.
//
// Unlike mesh_observations, these do NOT re-accumulate from the live feed if
// lost: a monitor reports the present and never replays. So retention here is a
// real decision about how far back a battery curve can go, not a cache size.
func (s *Store) PruneMeshTelemetry(ctx context.Context, cutoff time.Time) (int64, error) {
	var affected int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM mesh_telemetry WHERE reported_at < $1`, cutoff.Unix())
		if err != nil {
			return fmt.Errorf("store: pruning mesh telemetry: %w", err)
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	return affected, err
}

// RenameMeshTelemetryPubKey moves a node's samples from a provisional
// prefix-derived key onto its full public key.
//
// A monitor identifies a node by a PREFIX of its public key, so until an advert
// supplies the full key the node's samples are filed under the prefix. When the
// promotion happens the event id changes and the provisional record is retired
// — and without this the battery history would be orphaned under a key nothing
// refers to any more, silently, exactly once per node, with no way to notice.
//
// If both keys somehow hold a sample for the same instant, the full-key row
// wins (ON CONFLICT DO NOTHING) and the prefix row is dropped by the delete.
func (s *Store) RenameMeshTelemetryPubKey(ctx context.Context, from, to string) (int64, error) {
	if from == "" || to == "" || from == to {
		return 0, nil
	}
	var affected int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Copy under the new key, skipping a (pubkey, reported_at) the full key
		// already holds (that read was filed under both: keep the full key's),
		// then drop the provisional rows. PostgreSQL has no UPDATE OR IGNORE.
		cols := strings.Replace(telemetryColumns, "pubkey, ", "", 1)
		res, err := tx.ExecContext(ctx, `
			INSERT INTO mesh_telemetry (pubkey, `+cols+`)
			SELECT $1, `+cols+` FROM mesh_telemetry WHERE pubkey = $2
			ON CONFLICT (pubkey, reported_at) DO NOTHING`, to, from)
		if err != nil {
			return fmt.Errorf("store: renaming mesh telemetry key: %w", err)
		}
		affected, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM mesh_telemetry WHERE pubkey = $1`, from); err != nil {
			return fmt.Errorf("store: clearing superseded mesh telemetry key: %w", err)
		}
		return nil
	})
	return affected, err
}

// unixTimeOrNil writes a zero time as SQL NULL: "the monitor has never reached
// this node" is not the epoch. (store.go's unixOrNil takes a proto timestamp.)
func unixTimeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
