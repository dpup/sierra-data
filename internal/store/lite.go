package store

import (
	"context"
	"database/sql"
	"fmt"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"google.golang.org/protobuf/proto"
)

// liteBlob is ev without its GeoJSON geometry: what a list read that does not
// draw shapes needs. The bbox and centroid stay, so a client can still place
// and frame the event. The caller's event is not modified.
func liteBlob(ev *gridv1.Event) ([]byte, error) {
	g := ev.GetGeometry()
	if len(g.GetGeojson()) == 0 && g.GetObject() == nil {
		return proto.Marshal(ev)
	}
	c := proto.Clone(ev).(*gridv1.Event)
	stripShape(c)
	return proto.Marshal(c)
}

// stripShape removes ev's GeoJSON in place, keeping bbox and centroid.
func stripShape(ev *gridv1.Event) {
	if g := ev.GetGeometry(); g != nil {
		g.Geojson = nil
		g.Object = nil
	}
}

// putLite writes ev's lite copy. Every write of events.proto calls it in the
// same transaction, so the two never describe different revisions.
func putLite(tx *sql.Tx, ev *gridv1.Event) error {
	blob, err := liteBlob(ev)
	if err != nil {
		return fmt.Errorf("store: marshal lite event %s: %w", ev.GetId(), err)
	}
	if _, err := tx.Exec(`INSERT INTO event_lite (event_id, proto) VALUES ($1, $2)
		ON CONFLICT (event_id) DO UPDATE SET proto = excluded.proto`, ev.GetId(), blob); err != nil {
		return fmt.Errorf("store: write lite event %s: %w", ev.GetId(), err)
	}
	return nil
}

// LiteBatch is one BackfillEventLite step.
type LiteBatch struct {
	// Next is the cursor for the following call; empty when the walk is done.
	Next string
	// Written counts lite rows inserted, Skipped events whose blob would not
	// unmarshal (left without a lite row; their list read falls back).
	Written, Skipped int
}

// BackfillEventLite walks events in id order from after (exclusive), up to
// batch rows, and writes a lite copy for each that has none (events stored
// before the lite copy existed; only an import from the legacy SQLite store
// can produce them now). Callers loop with Next until it is empty.
//
// The read runs outside the writer lock, keyed on the primary key so each
// call reads only its own rows (no rescan from the start). The write is one
// short transaction that inserts only rows still missing: an upsert that
// wrote a newer lite copy in between wins. A blob that will not unmarshal is
// skipped and counted, never retried forever.
func (s *Store) BackfillEventLite(ctx context.Context, after string, batch int) (LiteBatch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.proto FROM events e
		WHERE e.id > $1 AND NOT EXISTS (SELECT 1 FROM event_lite l WHERE l.event_id = e.id)
		ORDER BY e.id LIMIT $2`, after, batch)
	if err != nil {
		return LiteBatch{}, fmt.Errorf("store: lite backfill scan: %w", err)
	}
	var res LiteBatch
	var evs []*gridv1.Event
	n := 0
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			rows.Close()
			return LiteBatch{}, fmt.Errorf("store: lite backfill scan: %w", err)
		}
		n++
		res.Next = id
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			res.Skipped++
			continue
		}
		evs = append(evs, ev)
	}
	if err := rows.Close(); err != nil {
		return LiteBatch{}, err
	}
	if n < batch {
		res.Next = ""
	}
	if len(evs) == 0 {
		return res, nil
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		for _, ev := range evs {
			blob, err := liteBlob(ev)
			if err != nil {
				return fmt.Errorf("store: marshal lite event %s: %w", ev.GetId(), err)
			}
			r, err := tx.Exec(`INSERT INTO event_lite (event_id, proto) VALUES ($1, $2)
				ON CONFLICT (event_id) DO NOTHING`, ev.GetId(), blob)
			if err != nil {
				return fmt.Errorf("store: write lite event %s: %w", ev.GetId(), err)
			}
			if k, _ := r.RowsAffected(); k > 0 {
				res.Written++
			}
		}
		return nil
	})
	if err != nil {
		return LiteBatch{}, err
	}
	return res, nil
}
