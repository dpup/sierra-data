package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
)

const (
	// zoneGeometryTTL: NWS zone boundaries change a few times a decade.
	zoneGeometryTTL = 7 * 24 * time.Hour
	// AlertZoneTolerance is the simplification tolerance for alert areas,
	// in degrees (~1 km). A Sacramento forecast zone is ~8,000 vertices
	// (190 KB) raw; an advisory can name 40 of them.
	AlertZoneTolerance = 0.01
)

// alertAreas memoizes the combined shape per zone set, so a tick does not
// re-parse and re-simplify the same zones every minute.
type alertAreas struct {
	mu sync.Mutex
	m  map[string][]byte
}

// AlertZoneGeometry returns the simplified, combined area of NWS zones (refs
// as nws.Alert.ZoneRefs, e.g. "fire/CAZ138") as one GeoJSON MultiPolygon.
// Zones are fetched once and cached for zoneGeometryTTL.
//
// All or nothing: if any zone cannot be read the error is returned and no
// partial shape is. Geometry is hashed in the store, so a shape that grew and
// shrank with each fetch failure would mint revisions, and a partial area
// understates what the alert covers. Refs are sorted, so the same set always
// gives the same bytes.
func (s *WeatherService) AlertZoneGeometry(ctx context.Context, refs []string) ([]byte, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if s.nwsClient == nil {
		return nil, fmt.Errorf("nws client not configured")
	}
	sorted := append([]string(nil), refs...)
	sort.Strings(sorted)
	key := strings.Join(sorted, ",")

	s.areas.mu.Lock()
	if g, ok := s.areas.m[key]; ok {
		s.areas.mu.Unlock()
		return g, nil
	}
	s.areas.mu.Unlock()

	var parts []*geojson.Geom
	for _, ref := range sorted {
		raw, err := s.zoneGeometry(ctx, ref)
		if err != nil {
			return nil, err
		}
		g, err := geojson.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("NWS zone %s: %w", ref, err)
		}
		parts = append(parts, g)
	}
	out := geojson.SimplifiedMultiPolygon(AlertZoneTolerance, parts...)
	if out == nil {
		return nil, fmt.Errorf("NWS zones %s have no areal geometry", key)
	}
	s.areas.mu.Lock()
	if s.areas.m == nil {
		s.areas.m = map[string][]byte{}
	}
	s.areas.m[key] = out
	s.areas.mu.Unlock()
	return out, nil
}

// zoneGeometry reads one zone's raw geometry through the cache. A failed
// fetch falls back to a stale copy: a boundary does not go out of date.
func (s *WeatherService) zoneGeometry(ctx context.Context, ref string) ([]byte, error) {
	key := "nws:zone:" + ref
	var raw string
	if found, _ := s.cache.Get(key, &raw); found && raw != "" {
		return []byte(raw), nil
	}
	b, err := s.nwsClient.GetZoneGeometry(ctx, ref)
	if err != nil {
		var stale string
		if _, found, _ := s.cache.GetWithMetadata(key, &stale); found && stale != "" {
			return []byte(stale), nil
		}
		return nil, err
	}
	_ = s.cache.Set(key, string(b), zoneGeometryTTL, "nws_zone")
	return b, nil
}
