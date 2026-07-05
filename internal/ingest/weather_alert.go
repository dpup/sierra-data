package ingest

import (
	"context"
	"time"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	api "github.com/dpup/info.ersn.net/server/api/v1"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
)

// weatherAlertsAPI is the slice of WeatherService this normalizer consumes
// (decision 6: ingest reuses the cached NWS fetch, no duplicate parsing).
type weatherAlertsAPI interface {
	ListWeatherAlerts(ctx context.Context, req *api.ListWeatherAlertsRequest) (*api.ListWeatherAlertsResponse, error)
}

// WeatherAlertNormalizer ingests NWS zone alerts (id namespace "wx:") via the
// weather service. Alerts carry NWS forecast zones, not geometry, so
// place_ids are precomputed here from each configured area's zones.
type WeatherAlertNormalizer struct {
	cfg     *config.Config
	weather weatherAlertsAPI
	now     func() time.Time // injectable for the SCHEDULED/ACTIVE cutover tests
}

// NewWeatherAlertNormalizer wires the normalizer to the weather service (the
// concrete *services.WeatherService satisfies weatherAlertsAPI).
func NewWeatherAlertNormalizer(cfg *config.Config, weather weatherAlertsAPI) *WeatherAlertNormalizer {
	return &WeatherAlertNormalizer{cfg: cfg, weather: weather, now: time.Now}
}

// SourceIDs implements Normalizer.
func (n *WeatherAlertNormalizer) SourceIDs() []string { return []string{"nws"} }

// Poll implements Normalizer.
func (n *WeatherAlertNormalizer) Poll(ctx context.Context) (*PollResult, error) {
	resp, err := n.weather.ListWeatherAlerts(ctx, &api.ListWeatherAlertsRequest{})
	if err != nil {
		return nil, err
	}
	now := n.now()

	events := make([]*gridv1.Event, 0, len(resp.GetAlerts()))
	for _, a := range resp.GetAlerts() {
		// An NWS watch is SCHEDULED until it takes effect (decision 8).
		status := gridv1.EventStatus_ACTIVE
		if start := a.GetStartTime(); start != nil && start.AsTime().After(now) {
			status = gridv1.EventStatus_SCHEDULED
		}

		ev := NewEvent(
			"wx:"+a.GetId(),
			gridv1.Layer_WEATHER_ALERT,
			SeverityFromLabel(hazards.SeverityFromAlertSeverity(a.GetSeverity())),
			status,
			nonEmpty(a.GetHeadline(), a.GetEvent()), // shipped headline fallback
		)
		ev.Category = a.GetEvent()
		ev.Description = a.GetDescription() // original NWS text, verbatim
		ev.Effective = a.GetStartTime()
		ev.Expires = a.GetEndTime()
		// Attach to every configured area whose zones intersect the alert's
		// (a zoneless alert can't be scoped, so it attaches to all areas).
		for _, area := range n.cfg.Hazards.Areas {
			if zonesMatch(area.Zones, a.GetZones()) {
				ev.PlaceIds = append(ev.PlaceIds, "area:"+area.ID)
			}
		}
		ev.Provenance = NewProvenance("nws", a.GetSenderName(), "", "")
		ev.Detail = &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{
			Event:       a.GetEvent(),
			NwsSeverity: a.GetSeverity().String(),
			SenderName:  a.GetSenderName(),
			Zones:       a.GetZones(),
		}}
		events = append(events, ev)
	}
	return &PollResult{Events: events}, nil
}
