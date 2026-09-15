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
func (s *Store) InsertMeshTelemetry(ctx context.Context, samples []MeshTelemetrySample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO mesh_telemetry (`+telemetryColumns+`)
			VALUES (`+strings.TrimSuffix(strings.Repeat("?, ", 29), ", ")+`)`)
		if err != nil {
			return fmt.Errorf("store: prepare mesh telemetry insert: %w", err)
		}
		defer stmt.Close()
		for _, t := range samples {
			if t.PubKey == "" || t.ReportedAt.IsZero() {
				continue // no identity or no time: nothing a series could use
			}
			if _, err := stmt.ExecContext(ctx,
				t.PubKey, t.ReportedAt.Unix(), t.ReceivedAt.Unix(), t.Reporter,
				unixTimeOrNil(t.LastSuccessAt), unixTimeOrNil(t.LastAttemptAt),
				t.BatteryVolts, t.BatteryPct, t.BatteryPctSource, t.TemperatureC, t.Humidity, t.Pressure,
				t.NoiseFloorDBm, t.LastSNRdB, t.LastRSSIdBm, t.TxQueueLen,
				t.UptimeS, t.AirtimeMs, t.RxAirtimeMs, t.PacketsSent, t.PacketsRecv,
				t.SentFlood, t.SentDirect, t.RecvFlood, t.RecvDirect,
				t.DirectDups, t.FloodDups, t.FullEvts, t.RecvErrors,
			); err != nil {
				return fmt.Errorf("store: inserting mesh telemetry for %s: %w", t.PubKey, err)
			}
		}
		return nil
	})
}

// MeshTelemetry returns one node's samples in [from, to), oldest first, plus
// whether the result was truncated at maxTelemetryRows.
func (s *Store) MeshTelemetry(ctx context.Context, pubkey string, from, to time.Time) ([]MeshTelemetrySample, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+telemetryColumns+`
		FROM mesh_telemetry
		WHERE pubkey = ? AND reported_at >= ? AND reported_at < ?
		ORDER BY reported_at ASC
		LIMIT ?`, pubkey, from.Unix(), to.Unix(), maxTelemetryRows+1)
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
		`SELECT MIN(reported_at), MAX(reported_at), COUNT(*) FROM mesh_telemetry WHERE pubkey = ?`, pubkey)
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
		res, err := tx.ExecContext(ctx, `DELETE FROM mesh_telemetry WHERE reported_at < ?`, cutoff.Unix())
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
// ON CONFLICT DO NOTHING via the INSERT-less UPDATE OR IGNORE: if both keys
// somehow hold a sample for the same instant, the full-key row wins and the
// prefix row is dropped by the delete below.
func (s *Store) RenameMeshTelemetryPubKey(ctx context.Context, from, to string) (int64, error) {
	if from == "" || to == "" || from == to {
		return 0, nil
	}
	var affected int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE OR IGNORE mesh_telemetry SET pubkey = ? WHERE pubkey = ?`, to, from)
		if err != nil {
			return fmt.Errorf("store: renaming mesh telemetry key: %w", err)
		}
		affected, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM mesh_telemetry WHERE pubkey = ?`, from); err != nil {
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
