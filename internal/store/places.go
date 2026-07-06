package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"google.golang.org/protobuf/proto"
)

// placeSpecificity orders PlacesContaining results most-specific-first,
// matching the /v1/places/resolve contract (plan §2.4).
var placeSpecificity = map[gridv1.PlaceKind]int{
	gridv1.PlaceKind_SITE:      0,
	gridv1.PlaceKind_EVAC_ZONE: 1,
	gridv1.PlaceKind_TOWN:      2,
	gridv1.PlaceKind_CORRIDOR:  3,
	gridv1.PlaceKind_COUNTY:    4,
	gridv1.PlaceKind_AREA:      5,
}

// UpsertPlace inserts or replaces a place. The blob is canonical; columns
// mirror it for filtering. A slug collision across different ids is a
// seeding bug and surfaces as a UNIQUE constraint error.
func (s *Store) UpsertPlace(ctx context.Context, p *gridv1.Place) error {
	if p.GetId() == "" || p.GetSlug() == "" {
		return fmt.Errorf("store: place requires id and slug")
	}
	blob, err := proto.Marshal(p)
	if err != nil {
		return fmt.Errorf("store: marshal place %s: %w", p.GetId(), err)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var parent any
		if p.GetParentId() != "" {
			parent = p.GetParentId()
		}
		if _, err := tx.Exec(`
			INSERT INTO places (id, kind, name, slug, parent_id, proto)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			  kind = excluded.kind, name = excluded.name, slug = excluded.slug,
			  parent_id = excluded.parent_id, proto = excluded.proto`,
			p.GetId(), int32(p.GetKind()), p.GetName(), p.GetSlug(), parent, blob,
		); err != nil {
			return fmt.Errorf("store: upsert place %s: %w", p.GetId(), err)
		}
		// Invalidate the parsed-geometry cache (held under mu, which inTx owns).
		// Conservative on rollback: a stray invalidation only forces a rebuild.
		s.placesGeoValid = false
		return nil
	})
}

// ListPlaces returns places, optionally filtered by kind
// (PLACE_KIND_UNSPECIFIED = all) and by q, a case-insensitive substring
// match on name. Ordered by kind, name, id.
func (s *Store) ListPlaces(ctx context.Context, kind gridv1.PlaceKind, q string) ([]*gridv1.Place, error) {
	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT proto FROM places WHERE 1=1`)
	if kind != gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED {
		sb.WriteString(` AND kind = ?`)
		args = append(args, int32(kind))
	}
	if q != "" {
		// instr on lowered strings: substring semantics with no LIKE-wildcard
		// escaping concerns.
		sb.WriteString(` AND instr(lower(name), lower(?)) > 0`)
		args = append(args, q)
	}
	sb.WriteString(` ORDER BY kind, name, id`)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list places: %w", err)
	}
	defer rows.Close()
	return scanPlaces(rows)
}

// GetPlace looks up a place by id when the key contains ':' (ids are
// namespaced, e.g. "area:ebbetts-pass"), otherwise by slug. Returns ErrNotFound
// when absent.
func (s *Store) GetPlace(ctx context.Context, slugOrID string) (*gridv1.Place, error) {
	column := "slug"
	if strings.Contains(slugOrID, ":") {
		column = "id"
	}
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT proto FROM places WHERE `+column+` = ?`, slugOrID).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get place %s: %w", slugOrID, err)
	}
	p := &gridv1.Place{}
	if err := proto.Unmarshal(blob, p); err != nil {
		return nil, fmt.Errorf("store: unmarshal place %s: %w", slugOrID, err)
	}
	return p, nil
}

// PlacesContaining returns every place whose polygon contains the point,
// ordered most-specific-first (SITE, EVAC_ZONE, TOWN, CORRIDOR, COUNTY,
// AREA), then by name. Bbox prefilter, then even-odd point-in-polygon; the
// place inventory is small enough (tens of rows) that a full scan is fine.
func (s *Store) PlacesContaining(ctx context.Context, lat, lng float64) ([]*gridv1.Place, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT proto FROM places`)
	if err != nil {
		return nil, fmt.Errorf("store: places containing: %w", err)
	}
	defer rows.Close()
	all, err := scanPlaces(rows)
	if err != nil {
		return nil, err
	}

	var matched []*gridv1.Place
	for _, p := range all {
		raw := p.GetGeometry().GetGeojson()
		if len(raw) == 0 {
			continue
		}
		g, err := geojson.Parse(raw)
		if err != nil {
			continue
		}
		minLat, minLng, maxLat, maxLng := g.Bbox()
		if lat < minLat || lat > maxLat || lng < minLng || lng > maxLng {
			continue
		}
		if geojson.PointInGeometry(lat, lng, g) {
			matched = append(matched, p)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		si, sj := placeSpecificity[matched[i].GetKind()], placeSpecificity[matched[j].GetKind()]
		if si != sj {
			return si < sj
		}
		return matched[i].GetName() < matched[j].GetName()
	})
	return matched, nil
}

func scanPlaces(rows *sql.Rows) ([]*gridv1.Place, error) {
	var out []*gridv1.Place
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("store: scan place: %w", err)
		}
		p := &gridv1.Place{}
		if err := proto.Unmarshal(blob, p); err != nil {
			return nil, fmt.Errorf("store: unmarshal place: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
