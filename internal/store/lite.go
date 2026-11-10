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
	c.Geometry.Geojson = nil
	c.Geometry.Object = nil
	return proto.Marshal(c)
}

// putLite writes ev's lite copy. Every write of events.proto calls it in the
// same transaction, so the two never describe different revisions.
func putLite(tx *sql.Tx, ev *gridv1.Event) error {
	blob, err := liteBlob(ev)
	if err != nil {
		return fmt.Errorf("store: marshal lite event %s: %w", ev.GetId(), err)
	}
	if _, err := tx.Exec(`INSERT INTO event_lite (event_id, proto) VALUES (?, ?)
		ON CONFLICT(event_id) DO UPDATE SET proto = excluded.proto`, ev.GetId(), blob); err != nil {
		return fmt.Errorf("store: write lite event %s: %w", ev.GetId(), err)
	}
	return nil
}

// BackfillEventLite writes lite copies for up to batch events that have none
// (events stored before migrationV12) and returns how many it wrote. Callers
// loop until it returns 0. Each batch is one short write transaction on the
// single writer.
func (s *Store) BackfillEventLite(ctx context.Context, batch int) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT e.id, e.proto FROM events e
			WHERE NOT EXISTS (SELECT 1 FROM event_lite l WHERE l.event_id = e.id)
			LIMIT ?`, batch)
		if err != nil {
			return fmt.Errorf("store: lite backfill scan: %w", err)
		}
		var evs []*gridv1.Event
		for rows.Next() {
			var id string
			var blob []byte
			if err := rows.Scan(&id, &blob); err != nil {
				rows.Close()
				return fmt.Errorf("store: lite backfill scan: %w", err)
			}
			ev := &gridv1.Event{}
			if err := proto.Unmarshal(blob, ev); err != nil {
				rows.Close()
				return fmt.Errorf("store: lite backfill unmarshal %s: %w", id, err)
			}
			evs = append(evs, ev)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, ev := range evs {
			if err := putLite(tx, ev); err != nil {
				return err
			}
		}
		n = len(evs)
		return nil
	})
	return n, err
}
