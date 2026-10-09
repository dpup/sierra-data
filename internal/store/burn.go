package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BurnReading is one pushed burn-day reading from one recorded phone line.
//
// This is the ONLY pushed source in the service. Everything else is polled, so
// the shape is deliberately narrow: the push handler validates and lands a row,
// and the ingest scheduler reads it on its own tick. Nothing here writes events.
type BurnReading struct {
	LineID     string    // config line id ("calaveras-apcd")
	Status     string    // upstream vocabulary: green | red | orange
	Message    string    // plain-language answer for display
	Transcript string    // cleaned transcript of the recording
	Confidence int32     // 0-100, the extraction model's own confidence
	ObservedAt time.Time // when the burn line was CALLED — the freshness signal
	ReceivedAt time.Time // when the push was accepted (diagnostic only)
}

// PutBurnReading stores the latest reading for a line, replacing any previous
// one. Goes through inTx, so it takes the same write mutex as every other
// writer — the single-writer discipline is enforced by the lock, not by there
// being one caller.
//
// It is deliberately LAST-WRITE-WINS on ObservedAt rather than blindly
// replacing: a retry or a duplicate delivery must never move a line BACKWARDS
// to an older reading, which would hand the freshness gate a stale stamp and
// blank a facet that was fine.
func (s *Store) PutBurnReading(ctx context.Context, r BurnReading) error {
	if r.LineID == "" {
		return errors.New("store: burn reading needs a line id")
	}
	if r.ObservedAt.IsZero() {
		return errors.New("store: burn reading needs an observed_at")
	}
	received := r.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO burn_readings (line_id, status, message, transcript, confidence, observed_at, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(line_id) DO UPDATE SET
			  status = excluded.status, message = excluded.message,
			  transcript = excluded.transcript, confidence = excluded.confidence,
			  observed_at = excluded.observed_at, received_at = excluded.received_at
			WHERE excluded.observed_at >= burn_readings.observed_at`,
			strings.ToLower(r.LineID), r.Status, r.Message, r.Transcript, r.Confidence,
			r.ObservedAt.Unix(), received.Unix())
		if err != nil {
			return fmt.Errorf("store: put burn reading %s: %w", r.LineID, err)
		}
		return nil
	})
}

// BurnReadings returns the latest reading for every line that has one, keyed by
// line id. An absent line is simply missing from the map — the caller treats
// that as "nothing pushed yet", not as an error.
func (s *Store) BurnReadings(ctx context.Context) (map[string]BurnReading, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT line_id, status, message, transcript, confidence, observed_at, received_at
		FROM burn_readings`)
	if err != nil {
		return nil, fmt.Errorf("store: burn readings: %w", err)
	}
	defer rows.Close()

	out := map[string]BurnReading{}
	for rows.Next() {
		var r BurnReading
		var observed, received int64
		if err := rows.Scan(&r.LineID, &r.Status, &r.Message, &r.Transcript,
			&r.Confidence, &observed, &received); err != nil {
			return nil, fmt.Errorf("store: scan burn reading: %w", err)
		}
		r.ObservedAt = time.Unix(observed, 0).UTC()
		r.ReceivedAt = time.Unix(received, 0).UTC()
		out[r.LineID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: burn readings: %w", err)
	}
	return out, nil
}
