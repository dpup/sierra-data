package ingest

import (
	"context"
	"fmt"
	"time"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/usgs"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
)

// Query scope matches the shipped earthquakes builder: M2.5+ over 7 days.
const (
	quakeMinMagnitude = 2.5
	quakeWindow       = 7 * 24 * time.Hour
)

// EarthquakeNormalizer ingests USGS FDSN events (id namespace "usgs:") over
// the union bbox of the configured hazard areas.
type EarthquakeNormalizer struct {
	cfg    *config.Config
	client *usgs.Client
}

// NewEarthquakeNormalizer wires the normalizer to a USGS client (tests inject
// one built with usgs.NewClientWithHTTPDoer).
func NewEarthquakeNormalizer(cfg *config.Config, client *usgs.Client) *EarthquakeNormalizer {
	return &EarthquakeNormalizer{cfg: cfg, client: client}
}

// SourceIDs implements Normalizer.
func (n *EarthquakeNormalizer) SourceIDs() []string { return []string{"usgs"} }

// Poll implements Normalizer.
func (n *EarthquakeNormalizer) Poll(ctx context.Context) (*PollResult, error) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(n.cfg.Hazards.Areas)
	if !ok {
		return &PollResult{}, nil
	}
	quakes, err := n.client.GetEarthquakes(ctx, usgs.Bounds{
		MinLatitude:  minLat,
		MaxLatitude:  maxLat,
		MinLongitude: minLng,
		MaxLongitude: maxLng,
	}, quakeMinMagnitude, quakeWindow)
	if err != nil {
		return nil, err
	}

	events := make([]*gridv1.Event, 0, len(quakes))
	for _, q := range quakes {
		ev := NewEvent(
			"usgs:"+q.ID,
			gridv1.Layer_EARTHQUAKE,
			SeverityFromLabel(hazards.SeverityFromMagnitude(q.Magnitude)),
			gridv1.EventStatus_ACTIVE,
			fmt.Sprintf("M%.1f — %s", q.Magnitude, q.Place), // shipped headline format
		)
		ev.Category = "earthquake"
		ev.AreaLabel = q.Place
		ev.CanonicalUrl = safeURL(q.URL)
		ev.Geometry = GeometryFromPoint(q.Lat, q.Lng)
		ev.Effective = tsProto(q.Time)
		// observed_at is the upstream update stamp, falling back to the event
		// time when USGS hasn't revised the record.
		if q.Updated.IsZero() {
			ev.ObservedAt = tsProto(q.Time)
		} else {
			ev.ObservedAt = tsProto(q.Updated)
		}
		ev.Provenance = NewProvenance("usgs", "USGS", "U.S. Geological Survey", safeURL(q.URL))
		ev.Detail = &gridv1.Event_Earthquake{Earthquake: &gridv1.EarthquakeDetail{
			Magnitude: q.Magnitude,
			DepthKm:   q.DepthKm,
			Felt:      q.Felt,
			Url:       safeURL(q.URL),
		}}
		events = append(events, ev)
	}
	return &PollResult{Events: events}, nil
}
