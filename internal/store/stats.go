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
// count, rather than counted in event_revisions: that table holds a full
// proto blob per row and is by far the largest, and on EFS a scan of it is
// a long walk of network reads. `Since` is the earliest revision-1
// ingested_at, a primary-key prefix lookup per event that reads only the
// row's leading columns, never its blob.
//
// Place attachments are kept after a lifecycle transition (see AGENTS.md), so
// a place's totals include its resolved and expired events.
func (s *Store) Totals(ctx context.Context, placeID string) (Totals, error) {
	from, args := "events e", []any{}
	if placeID != "" {
		from = "event_places ep JOIN events e ON e.id = ep.event_id"
		args = append(args, placeID)
	}
	where := ""
	if placeID != "" {
		where = "WHERE ep.place_id = ?"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.layer, COUNT(*), SUM(e.revision)
		FROM `+from+` `+where+`
		GROUP BY e.layer`, args...)
	if err != nil {
		return Totals{}, fmt.Errorf("store: totals: %w", err)
	}
	var t Totals
	for rows.Next() {
		var lt LayerTotals
		var layer int32
		if err := rows.Scan(&layer, &lt.Events, &lt.Revisions); err != nil {
			rows.Close()
			return Totals{}, fmt.Errorf("store: totals scan: %w", err)
		}
		lt.Layer = gridv1.Layer(layer)
		t.Events += lt.Events
		t.Revisions += lt.Revisions
		t.Layers = append(t.Layers, lt)
	}
	if err := rows.Close(); err != nil {
		return Totals{}, fmt.Errorf("store: totals: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Totals{}, fmt.Errorf("store: totals: %w", err)
	}
	sort.SliceStable(t.Layers, func(i, j int) bool {
		if t.Layers[i].Events != t.Layers[j].Events {
			return t.Layers[i].Events > t.Layers[j].Events
		}
		return t.Layers[i].Layer < t.Layers[j].Layer
	})

	var since sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT e.source_id), MIN(r.ingested_at)
		FROM `+from+`
		LEFT JOIN event_revisions r ON r.event_id = e.id AND r.revision = 1 `+where, args...).Scan(&t.Sources, &since); err != nil {
		return Totals{}, fmt.Errorf("store: totals since: %w", err)
	}
	if since.Valid {
		t.Since = time.Unix(since.Int64, 0).UTC()
	}
	return t, nil
}
