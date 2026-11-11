package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// LayerTotals is one layer's share of Totals.
type LayerTotals struct {
	Layer     gridv1.Layer
	Events    int64
	Revisions int64
}

// Totals is what the store has recorded: events in every status, the
// revisions across them, the distinct sources, and the earliest first ingest.
type Totals struct {
	Events    int64
	Revisions int64
	Sources   int
	Since     time.Time // zero when there are no events
	Layers    []LayerTotals
}

// Totals counts every event (any status), optionally only those attached to
// placeID.
//
// Revisions are summed from events.revision, which is each event's revision
// count, rather than counted in event_revisions (by far the largest table: a
// full proto blob per row). `Since` is the earliest events.first_ingested_at,
// a column written once when revision 1 was stored: the legacy store joined
// revision 1 of every event for it, which was most of /stats taking over 30 s
// on EFS (2026-11-11).
//
// Place attachments are kept after a lifecycle transition (see AGENTS.md), so
// a place's totals include its resolved and expired events. Both statements
// read one REPEATABLE READ snapshot, so the per-layer rows and the totals line
// agree.
func (s *Store) Totals(ctx context.Context, placeID string) (Totals, error) {
	from, where := "events e", ""
	var args sqlArgs
	if placeID != "" {
		from = "event_places ep JOIN events e ON e.id = ep.event_id"
		where = "WHERE ep.place_id = " + args.add(placeID)
	}
	var t Totals
	var since sql.NullInt64
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		// SUM over bigint is numeric in PostgreSQL; cast back for the scan.
		rows, err := tx.QueryContext(ctx, `
			SELECT e.layer, COUNT(*), SUM(e.revision)::bigint
			FROM `+from+` `+where+`
			GROUP BY e.layer`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var lt LayerTotals
			var layer int32
			if err := rows.Scan(&layer, &lt.Events, &lt.Revisions); err != nil {
				return err
			}
			lt.Layer = gridv1.Layer(layer)
			t.Events += lt.Events
			t.Revisions += lt.Revisions
			t.Layers = append(t.Layers, lt)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT e.source_id), MIN(e.first_ingested_at)
			FROM `+from+` `+where, args...).Scan(&t.Sources, &since)
	})
	if err != nil {
		return Totals{}, fmt.Errorf("store: totals: %w", err)
	}
	sort.SliceStable(t.Layers, func(i, j int) bool {
		if t.Layers[i].Events != t.Layers[j].Events {
			return t.Layers[i].Events > t.Layers[j].Events
		}
		return t.Layers[i].Layer < t.Layers[j].Layer
	})
	if since.Valid {
		t.Since = time.Unix(since.Int64, 0).UTC()
	}
	return t, nil
}
