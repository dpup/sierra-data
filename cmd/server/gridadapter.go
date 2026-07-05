package main

import (
	"context"
	"fmt"
	"time"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/gridapi"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
	"github.com/dpup/info.ersn.net/server/internal/store"
)

// gridStoreBackend adapts the grid event store into hazards.StoreBackend —
// the T14 strangler seam that re-backs the five event-backed /api/v1 hazards
// layers onto the store. It reuses the exact code path /v1 map layers serve
// through (gridapi.serveEventLayer): store.QueryEvents scoped to the place's
// ACTIVE+SCHEDULED events, projected by gridapi.ProjectEvents (the byte-compat
// gated T13 projection), with layer health from gridapi.LayerSourceStatus over
// the source registry. Errors are returned raw: the hazards side owns the
// fail-loud status mapping (store unreadable -> UNAVAILABLE).
type gridStoreBackend struct {
	store *store.Store
}

var _ hazards.StoreBackend = (*gridStoreBackend)(nil)

// newGridStoreBackend wraps st for hazards.NewServiceWithAPIs.
func newGridStoreBackend(st *store.Store) *gridStoreBackend {
	return &gridStoreBackend{store: st}
}

// gridEventLayers maps each event-backed shipped layer slug onto its store
// layer enum (must cover exactly hazards' eventBackedLayer set; the condition
// layers are absent by design — they are never store-backed).
var gridEventLayers = map[string]gridv1.Layer{
	hazards.LayerWildfire:     gridv1.Layer_WILDFIRE,
	hazards.LayerEvacuation:   gridv1.Layer_EVACUATION,
	hazards.LayerWeatherAlert: gridv1.Layer_WEATHER_ALERT,
	hazards.LayerEarthquake:   gridv1.Layer_EARTHQUAKE,
	hazards.LayerRoadIncident: gridv1.Layer_ROAD_INCIDENT,
}

// QueryActive returns the place's projected ACTIVE+SCHEDULED features for one
// event-backed layer plus the layer's source health (OK | STALE | UNAVAILABLE
// and the most recent successful fetch). RESOLVED/EXPIRED events never reach
// the live map — they belong to /v1/history.
func (b *gridStoreBackend) QueryActive(ctx context.Context, placeID, layer string) ([]hazards.Feature, string, time.Time, error) {
	lyr, ok := gridEventLayers[layer]
	if !ok {
		return nil, "", time.Time{}, fmt.Errorf("layer %q is not event-backed", layer)
	}

	q := store.EventQuery{
		PlaceID:  placeID,
		Layers:   []gridv1.Layer{lyr},
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED},
		PageSize: 200, // the store max; the keyset loop below drains any overflow
	}
	var events []*gridv1.Event
	for {
		page, next, err := b.store.QueryEvents(ctx, q)
		if err != nil {
			return nil, "", time.Time{}, err
		}
		events = append(events, page...)
		if next == "" {
			break
		}
		q.PageToken = next
	}

	sources, err := b.store.ListSources(ctx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	status, lastUpdate := gridapi.LayerSourceStatus(sources, layer)
	return gridapi.ProjectEvents(layer, events), status, lastUpdate, nil
}
