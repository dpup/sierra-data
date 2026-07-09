package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UpsertResult reports what UpsertEvent did.
type UpsertResult struct {
	Changed  bool   // a row was written (new event or new revision)
	Revision uint32 // current revision after the call
}

// ContentHash returns the SHA-256 hex of a deterministic marshal of ev with
// volatile fields zeroed: revision, ingested_at, observed_at,
// provenance.fetched_at, enhancement, summary, place_ids. Upstream re-stamps
// without content change produce the same hash (no revision), and
// enhancement output never causes hash churn — the scheduler decides whether
// to spend enhancement budget via NeedsUpdate BEFORE enhancing.
func ContentHash(ev *gridv1.Event) string {
	c := proto.Clone(ev).(*gridv1.Event)
	c.Revision = 0
	c.IngestedAt = nil
	c.ObservedAt = nil
	if c.Provenance != nil {
		c.Provenance.FetchedAt = nil
	}
	c.Enhancement = nil
	c.Summary = ""
	c.PlaceIds = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		// Marshal of a well-formed generated message cannot fail; a sentinel
		// keyed by id keeps the failure visible without panicking ingest.
		b = []byte("marshal-error:" + ev.GetId())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NeedsUpdate reports whether UpsertEvent(ev) would write: true when the
// event is unknown or its normalized content hash differs from the stored
// one. The scheduler pre-checks this before spending AI-enhancement budget.
func (s *Store) NeedsUpdate(ctx context.Context, ev *gridv1.Event) (bool, error) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE id = ?`, ev.GetId()).Scan(&stored)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: needs-update %s: %w", ev.GetId(), err)
	}
	return stored != ContentHash(ev), nil
}

// UpsertEvent inserts or revises an event, gated on ContentHash:
//
//   - no existing row: insert at revision 1;
//   - hash equal to stored: no revision and Changed=false, but the place
//     attachments are still refreshed — place_ids are zeroed out of the
//     hash, so a changed caller preset (e.g. a new zone->area mapping) or a
//     place seeded after the event first arrived must still attach;
//   - hash differs: revision = old+1, row updated.
//
// Every content write also inserts an event_revisions snapshot, recomputes
// event_places, refreshes the R*Tree bbox row, and stamps last_seen_at.
// ingested_at is stamped here; callers own observed_at (nil falls back to
// now for the NOT NULL index column only).
func (s *Store) UpsertEvent(ctx context.Context, ev *gridv1.Event) (UpsertResult, error) {
	if ev.GetId() == "" {
		return UpsertResult{}, fmt.Errorf("store: upsert event with empty id")
	}
	// Hash the event exactly as passed — before any store-side mutation — so
	// the stored hash always matches what the next poll's ContentHash yields.
	hash := ContentHash(ev)

	var res UpsertResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var oldRev uint32
		var oldHash string
		err := tx.QueryRow(`SELECT revision, content_hash FROM events WHERE id = ?`, ev.GetId()).
			Scan(&oldRev, &oldHash)
		switch {
		case err == sql.ErrNoRows:
			res = UpsertResult{Changed: true, Revision: 1}
		case err != nil:
			return fmt.Errorf("store: upsert lookup %s: %w", ev.GetId(), err)
		case oldHash == hash:
			res = UpsertResult{Changed: false, Revision: oldRev}
			// Content unchanged, but the place set may not be: recompute and
			// sync attachments without touching revision/hash/history.
			return s.refreshEventPlaces(tx, ev)
		default:
			res = UpsertResult{Changed: true, Revision: oldRev + 1}
		}

		now := time.Now()
		c := proto.Clone(ev).(*gridv1.Event)
		c.Revision = res.Revision
		c.IngestedAt = timestamppb.New(now)
		ensureGeometryIndex(c)

		placeIDs, err := s.matchPlaces(tx, c)
		if err != nil {
			return err
		}
		// UNION caller-preset place_ids (e.g. NWS zone->area mapping) with
		// geometric matches — preset ids are never dropped.
		c.PlaceIds = unionSorted(ev.GetPlaceIds(), placeIDs)

		blob, err := proto.Marshal(c)
		if err != nil {
			return fmt.Errorf("store: marshal event %s: %w", c.GetId(), err)
		}

		observedAt := now.Unix() // NOT NULL index column fallback; blob keeps caller's value
		if c.GetObservedAt() != nil {
			observedAt = c.GetObservedAt().AsTime().Unix()
		}
		if _, err := tx.Exec(`
			INSERT INTO events (id, layer, severity, status, source_id, effective, expires,
			                    observed_at, ingested_at, revision, content_hash, proto, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			  layer = excluded.layer, severity = excluded.severity, status = excluded.status,
			  source_id = excluded.source_id, effective = excluded.effective,
			  expires = excluded.expires, observed_at = excluded.observed_at,
			  ingested_at = excluded.ingested_at, revision = excluded.revision,
			  content_hash = excluded.content_hash, proto = excluded.proto,
			  last_seen_at = excluded.last_seen_at`,
			c.GetId(), int32(c.GetLayer()), int32(c.GetSeverity()), int32(c.GetStatus()),
			c.GetProvenance().GetSourceId(), unixOrNil(c.GetEffective()), unixOrNil(c.GetExpires()),
			observedAt, now.Unix(), res.Revision, hash, blob, now.Unix(),
		); err != nil {
			return fmt.Errorf("store: upsert event %s: %w", c.GetId(), err)
		}
		if err := insertRevision(tx, c.GetId(), res.Revision, observedAt, now.Unix(), blob); err != nil {
			return err
		}
		if err := replaceEventPlaces(tx, c.GetId(), c.PlaceIds); err != nil {
			return err
		}
		return upsertGeo(tx, c)
	})
	if err != nil {
		return UpsertResult{}, err
	}
	return res, nil
}

// TransitionEvents moves each event to status `to`, bumping its revision and
// writing a revision snapshot — the all-clear IS history. Events already in
// that status (and unknown ids) are skipped, so lifecycle sweeps are
// idempotent. observedAt is when the transition was observed (e.g. the poll
// that noticed the disappearance).
func (s *Store) TransitionEvents(ctx context.Context, ids []string, to gridv1.EventStatus, observedAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			var blob []byte
			err := tx.QueryRow(`SELECT proto FROM events WHERE id = ?`, id).Scan(&blob)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return fmt.Errorf("store: transition lookup %s: %w", id, err)
			}
			ev := &gridv1.Event{}
			if err := proto.Unmarshal(blob, ev); err != nil {
				return fmt.Errorf("store: transition unmarshal %s: %w", id, err)
			}
			if ev.GetStatus() == to {
				continue
			}
			now := time.Now()
			ev.Status = to
			ev.ObservedAt = timestamppb.New(observedAt)
			ev.IngestedAt = timestamppb.New(now)
			ev.Revision++
			newBlob, err := proto.Marshal(ev)
			if err != nil {
				return fmt.Errorf("store: transition marshal %s: %w", id, err)
			}
			// Status is hashed content: recompute so a later reappearance in
			// the feed (status differs) correctly writes a revision.
			if _, err := tx.Exec(`
				UPDATE events SET status = ?, observed_at = ?, ingested_at = ?,
				                  revision = ?, content_hash = ?, proto = ?
				WHERE id = ?`,
				int32(to), observedAt.Unix(), now.Unix(),
				ev.GetRevision(), ContentHash(ev), newBlob, id,
			); err != nil {
				return fmt.Errorf("store: transition update %s: %w", id, err)
			}
			if err := insertRevision(tx, id, ev.GetRevision(), observedAt.Unix(), now.Unix(), newBlob); err != nil {
				return err
			}
		}
		return nil
	})
}

// TouchSeen stamps last_seen_at = t for every id, in one UPDATE. It writes
// no revisions and never touches the content hash — "the source still lists
// this event" is liveness metadata, not content. The scheduler calls it for
// every id a successful poll returned, including hash-equal no-op upserts,
// so the expire grace is anchored to the last successful appearance.
func (s *Store) TouchSeen(ctx context.Context, ids []string, t time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	args := make([]any, 0, len(ids)+1)
	args = append(args, t.Unix())
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE events SET last_seen_at = ? WHERE id IN (`+placeholders(len(ids))+`)`,
		args...); err != nil {
		return fmt.Errorf("store: touch seen: %w", err)
	}
	return nil
}

// StoredEvent pairs an event with store-side liveness metadata that is not
// part of the proto blob.
type StoredEvent struct {
	Event *gridv1.Event
	// LastSeenAt is when a successful poll last included this event (see
	// TouchSeen). Zero for rows that predate last-seen tracking; callers
	// fall back to observed/ingested times.
	LastSeenAt time.Time
}

// ActiveEventsBySource returns all ACTIVE and SCHEDULED events for a source,
// each with its last-seen time. The lifecycle sweep diffs this set against
// the latest poll to find disappeared events and anchors the expire grace to
// LastSeenAt.
func (s *Store) ActiveEventsBySource(ctx context.Context, sourceID string) ([]StoredEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT proto, last_seen_at FROM events
		WHERE source_id = ? AND status IN (?, ?)
		ORDER BY id`,
		sourceID, int32(gridv1.EventStatus_ACTIVE), int32(gridv1.EventStatus_SCHEDULED))
	if err != nil {
		return nil, fmt.Errorf("store: active by source %s: %w", sourceID, err)
	}
	defer rows.Close()

	var out []StoredEvent
	for rows.Next() {
		var blob []byte
		var lastSeen int64
		if err := rows.Scan(&blob, &lastSeen); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return nil, fmt.Errorf("store: unmarshal event: %w", err)
		}
		se := StoredEvent{Event: ev}
		if lastSeen > 0 {
			se.LastSeenAt = time.Unix(lastSeen, 0)
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// GetEvent returns the current revision of an event, or ErrNotFound.
func (s *Store) GetEvent(ctx context.Context, id string) (*gridv1.Event, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT proto FROM events WHERE id = ?`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get event %s: %w", id, err)
	}
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		return nil, fmt.Errorf("store: unmarshal event %s: %w", id, err)
	}
	return ev, nil
}

// EventVersion returns an event's current revision without rehydrating its proto
// blob — a cheap index-only read (the revision column bumps on every content
// change or lifecycle transition). ok is false for an unknown id. Callers use it
// as an ETag validator to answer a conditional GET before the expensive load.
func (s *Store) EventVersion(ctx context.Context, id string) (revision int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT revision FROM events WHERE id = ?`, id).Scan(&revision)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: event version %s: %w", id, err)
	}
	return revision, true, nil
}

// DataVersion is a cheap, monotonically-increasing global counter of stored
// event revisions. event_revisions is append-only (a snapshot is written on
// every content change and every lifecycle transition, never deleted), so this
// count strictly increases whenever any event's content or status changes — and
// stays put when nothing does. That makes it a safe conservative ETag validator
// for the cross-event list/query endpoints: a query's result cannot change
// without a revision write, so an unchanged count guarantees an unchanged result
// (it never yields a stale 304). It is deliberately coarse — any write anywhere
// invalidates every list validator — which is the correct bias for life-safety
// data.
func (s *Store) DataVersion(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_revisions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: data version: %w", err)
	}
	return n, nil
}

// --- internals ---

func insertRevision(tx *sql.Tx, id string, rev uint32, observedAt, ingestedAt int64, blob []byte) error {
	if _, err := tx.Exec(`
		INSERT INTO event_revisions (event_id, revision, observed_at, ingested_at, proto)
		VALUES (?, ?, ?, ?, ?)`,
		id, rev, observedAt, ingestedAt, blob,
	); err != nil {
		return fmt.Errorf("store: insert revision %s@%d: %w", id, rev, err)
	}
	return nil
}

// refreshEventPlaces syncs an existing event's place attachments on the
// hash-equal upsert path. place_ids are zeroed out of the content hash, so
// hash-equal does NOT mean place-set-equal: the caller's preset ids may have
// changed (config edit) and places seeded after the event first arrived
// (boot-order, new polygons) must still match geometrically. When the
// recomputed set differs from the stored rows, event_places and the stored
// blob's place_ids are updated in place — no new revision, hash and
// revision untouched.
func (s *Store) refreshEventPlaces(tx *sql.Tx, ev *gridv1.Event) error {
	c := proto.Clone(ev).(*gridv1.Event)
	ensureGeometryIndex(c)
	matched, err := s.matchPlaces(tx, c)
	if err != nil {
		return err
	}
	want := unionSorted(ev.GetPlaceIds(), matched)

	rows, err := tx.Query(`SELECT place_id FROM event_places WHERE event_id = ? ORDER BY place_id`, ev.GetId())
	if err != nil {
		return fmt.Errorf("store: load event_places %s: %w", ev.GetId(), err)
	}
	var have []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan event_places %s: %w", ev.GetId(), err)
		}
		have = append(have, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate event_places %s: %w", ev.GetId(), err)
	}
	placesChanged := !slices.Equal(want, have)

	// Load the stored blob to compare the hash-excluded fields (place_ids and the
	// AI enhancement/summary) against the incoming event.
	var blob []byte
	if err := tx.QueryRow(`SELECT proto FROM events WHERE id = ?`, ev.GetId()).Scan(&blob); err != nil {
		return fmt.Errorf("store: load event %s for refresh: %w", ev.GetId(), err)
	}
	stored := &gridv1.Event{}
	if err := proto.Unmarshal(blob, stored); err != nil {
		return fmt.Errorf("store: unmarshal event %s for refresh: %w", ev.GetId(), err)
	}

	// enhancement/summary are excluded from the content hash, so a re-run
	// enhancement (fresh enhanced_at/request/response, or a reworded summary)
	// with otherwise-identical content lands on this hash-equal path. Persist the
	// latest without a new revision. Only when the incoming actually carries a
	// fresh enhancement — never erase a stored enhancement on a plain no-op poll
	// (weather alerts don't re-enhance on hash-equal ticks, so ev.Enhancement is
	// nil then and must not overwrite the stored one).
	enhChanged := ev.GetEnhancement() != nil &&
		(!proto.Equal(stored.GetEnhancement(), ev.GetEnhancement()) || stored.GetSummary() != ev.GetSummary())

	if !placesChanged && !enhChanged {
		return nil
	}
	if placesChanged {
		if err := replaceEventPlaces(tx, ev.GetId(), want); err != nil {
			return err
		}
	}
	// Keep the canonical blob consistent (reads rehydrate from it): place_ids in
	// lockstep with event_places, and the freshest enhancement/summary.
	stored.PlaceIds = want
	if enhChanged {
		stored.Enhancement = ev.GetEnhancement()
		stored.Summary = ev.GetSummary()
	}
	newBlob, err := proto.Marshal(stored)
	if err != nil {
		return fmt.Errorf("store: marshal event %s for refresh: %w", ev.GetId(), err)
	}
	if _, err := tx.Exec(`UPDATE events SET proto = ? WHERE id = ?`, newBlob, ev.GetId()); err != nil {
		return fmt.Errorf("store: update event %s for refresh: %w", ev.GetId(), err)
	}
	return nil
}

func replaceEventPlaces(tx *sql.Tx, id string, placeIDs []string) error {
	if _, err := tx.Exec(`DELETE FROM event_places WHERE event_id = ?`, id); err != nil {
		return fmt.Errorf("store: clear event_places %s: %w", id, err)
	}
	for _, pid := range placeIDs {
		if _, err := tx.Exec(`INSERT INTO event_places (event_id, place_id) VALUES (?, ?)`, id, pid); err != nil {
			return fmt.Errorf("store: insert event_places %s->%s: %w", id, pid, err)
		}
	}
	return nil
}

// ensureGeometryIndex backfills bbox and centroid from the raw GeoJSON when
// the normalizer left them unset. Geometry is hashed content, but the hash is
// computed from the event as passed, so backfilling here cannot cause churn.
func ensureGeometryIndex(ev *gridv1.Event) {
	g := ev.GetGeometry()
	if g == nil || len(g.GetGeojson()) == 0 {
		return
	}
	if g.GetBbox() != nil && g.GetCentroid() != nil {
		return
	}
	parsed, err := geojson.Parse(g.GetGeojson())
	if err != nil {
		return // columns are indexes only; bad geometry must not block ingest
	}
	if g.GetBbox() == nil {
		minLat, minLng, maxLat, maxLng := parsed.Bbox()
		g.Bbox = &gridv1.BoundingBox{MinLat: minLat, MinLng: minLng, MaxLat: maxLat, MaxLng: maxLng}
	}
	if g.GetCentroid() == nil {
		lat, lng := parsed.Centroid()
		g.Centroid = &gridv1.LatLng{Lat: lat, Lng: lng}
	}
}

// upsertGeo keeps exactly one R*Tree row per event with geometry (via the
// event_geo_map rowid indirection) and none for events without.
func upsertGeo(tx *sql.Tx, ev *gridv1.Event) error {
	bbox := ev.GetGeometry().GetBbox()
	if bbox == nil {
		if _, err := tx.Exec(`
			DELETE FROM event_geo WHERE rowid = (SELECT rowid FROM event_geo_map WHERE event_id = ?)`,
			ev.GetId()); err != nil {
			return fmt.Errorf("store: clear event_geo %s: %w", ev.GetId(), err)
		}
		if _, err := tx.Exec(`DELETE FROM event_geo_map WHERE event_id = ?`, ev.GetId()); err != nil {
			return fmt.Errorf("store: clear event_geo_map %s: %w", ev.GetId(), err)
		}
		return nil
	}
	var rowid int64
	err := tx.QueryRow(`SELECT rowid FROM event_geo_map WHERE event_id = ?`, ev.GetId()).Scan(&rowid)
	if err == sql.ErrNoRows {
		r, err := tx.Exec(`INSERT INTO event_geo_map (event_id) VALUES (?)`, ev.GetId())
		if err != nil {
			return fmt.Errorf("store: insert event_geo_map %s: %w", ev.GetId(), err)
		}
		if rowid, err = r.LastInsertId(); err != nil {
			return fmt.Errorf("store: event_geo_map rowid %s: %w", ev.GetId(), err)
		}
	} else if err != nil {
		return fmt.Errorf("store: lookup event_geo_map %s: %w", ev.GetId(), err)
	}
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO event_geo (rowid, min_lat, max_lat, min_lng, max_lng)
		VALUES (?, ?, ?, ?, ?)`,
		rowid, bbox.GetMinLat(), bbox.GetMaxLat(), bbox.GetMinLng(), bbox.GetMaxLng(),
	); err != nil {
		return fmt.Errorf("store: upsert event_geo %s: %w", ev.GetId(), err)
	}
	return nil
}

// corridorBufferMeters is how close a point event must be to a corridor
// LineString to attach to it. Corridors are seeded as the straight
// origin->destination chord (not the road's true path), so the buffer has to
// absorb both the chord-vs-road deviation on these short mountain sections and
// the few-hundred-metre slop in CHP/Caltrans incident coordinates. Tune here.
// corridorBufferDeg is a loose degree-margin (>= the buffer, ~1° ≈ 111 km) used
// only to widen the cheap bbox prefilter so a point near a zero-width line still
// reaches the exact PointNearLine test.
const (
	corridorBufferMeters = 1500
	corridorBufferDeg    = corridorBufferMeters/111000.0*1.3 + 0.0
)

// matchPlaces computes the geometric event->place attachments.
//
// Rule (over-attach beats missing a perimeter that crosses a boundary):
//   - point events (or events whose GeoJSON won't parse but carry a
//     centroid): attach every place whose polygon contains the centroid;
//   - polygon/multipolygon events: attach a place when the bboxes intersect
//     AND (the event centroid is in the place, OR the place's bbox center is
//     in the event geometry, OR both geometries are polygons — permissive
//     bbox-overlap);
//   - other event types (linestrings): bbox intersect AND centroid in place.
//
// Zone-carrying weather alerts are handled by the caller pre-setting
// ev.place_ids; UpsertEvent unions those with these matches.
func (s *Store) matchPlaces(tx *sql.Tx, ev *gridv1.Event) ([]string, error) {
	g := ev.GetGeometry()
	if g == nil {
		return nil, nil
	}
	var evGeom *geojson.Geom
	if len(g.GetGeojson()) > 0 {
		if parsed, err := geojson.Parse(g.GetGeojson()); err == nil {
			evGeom = parsed
		}
	}
	centroid := g.GetCentroid()
	if evGeom == nil && centroid == nil {
		return nil, nil
	}

	var evMinLat, evMinLng, evMaxLat, evMaxLng float64
	pointLike := evGeom == nil || evGeom.Type == "Point"
	if evGeom != nil {
		evMinLat, evMinLng, evMaxLat, evMaxLng = evGeom.Bbox()
		if centroid == nil {
			lat, lng := evGeom.Centroid()
			centroid = &gridv1.LatLng{Lat: lat, Lng: lng}
		}
	} else {
		evMinLat, evMinLng, evMaxLat, evMaxLng = centroid.GetLat(), centroid.GetLng(), centroid.GetLat(), centroid.GetLng()
	}
	evPolygonal := evGeom != nil && (evGeom.Type == "Polygon" || evGeom.Type == "MultiPolygon")

	places, err := s.loadPlaceGeoms(tx)
	if err != nil {
		return nil, err
	}

	var matched []string
	for _, pl := range places {
		if pointLike {
			// Polygon places: exact point-in-polygon. Corridor (LineString) places:
			// attach when the point is within corridorBufferMeters of the road line
			// (PointNearLine is a no-op for non-line geoms, so this is safe for all).
			if geojson.PointInGeometry(centroid.GetLat(), centroid.GetLng(), pl.geom) ||
				geojson.PointNearLine(centroid.GetLat(), centroid.GetLng(), pl.geom, corridorBufferMeters) {
				matched = append(matched, pl.id)
			}
			continue
		}
		if !geojson.BboxIntersects(evMinLat, evMinLng, evMaxLat, evMaxLng,
			pl.minLat, pl.minLng, pl.maxLat, pl.maxLng) {
			continue
		}
		if geojson.PointInGeometry(centroid.GetLat(), centroid.GetLng(), pl.geom) {
			matched = append(matched, pl.id)
			continue
		}
		if !evPolygonal {
			continue
		}
		if geojson.PointInGeometry(pl.centLat, pl.centLng, evGeom) {
			matched = append(matched, pl.id)
			continue
		}
		if pl.polygonal {
			matched = append(matched, pl.id) // permissive polygon-polygon bbox overlap
		}
	}
	return matched, nil
}

// loadPlaceGeoms returns the parsed place geometries, rebuilding the cache from
// the places table only when it has been invalidated (UpsertPlace) or never
// built. Called under the store mutex (matchPlaces runs inside inTx), so the
// cache needs no separate lock. This avoids re-SELECTing and re-parsing every
// place polygon on every per-event upsert — including hash-equal no-ops, which
// dominate a steady-state tick.
func (s *Store) loadPlaceGeoms(tx *sql.Tx) ([]parsedPlace, error) {
	if s.placesGeoValid {
		return s.placesGeo, nil
	}
	rows, err := tx.Query(`SELECT id, proto FROM places`)
	if err != nil {
		return nil, fmt.Errorf("store: load places: %w", err)
	}
	defer rows.Close()

	var out []parsedPlace
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("store: scan place: %w", err)
		}
		place := &gridv1.Place{}
		if err := proto.Unmarshal(blob, place); err != nil {
			return nil, fmt.Errorf("store: unmarshal place %s: %w", id, err)
		}
		raw := place.GetGeometry().GetGeojson()
		if len(raw) == 0 {
			continue
		}
		g, err := geojson.Parse(raw)
		if err != nil {
			continue
		}
		minLat, minLng, maxLat, maxLng := g.Bbox()
		clat, clng := g.Centroid()
		out = append(out, parsedPlace{
			id: id, geom: g,
			minLat: minLat, minLng: minLng, maxLat: maxLat, maxLng: maxLng,
			centLat: clat, centLng: clng,
			polygonal: g.Type == "Polygon" || g.Type == "MultiPolygon",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate places: %w", err)
	}
	s.placesGeo = out
	s.placesGeoValid = true
	return out, nil
}

// unionSorted merges two id lists, deduped and sorted for deterministic blobs.
func unionSorted(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, list := range [][]string{a, b} {
		for _, id := range list {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
