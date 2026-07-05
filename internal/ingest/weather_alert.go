package ingest

import (
	"context"
	"time"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/nws"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
	"github.com/dpup/info.ersn.net/server/internal/services"
)

// weatherAlertsAPI is the slice of WeatherService this normalizer consumes
// (decision 6: ingest reuses the cached NWS fetch, no duplicate parsing).
// RawNWSAlerts returns the full nws.Alert records — certainty, urgency,
// instruction, areaDesc, and the unmapped NWS severity vocabulary, all of
// which the api.WeatherAlert projection drops — plus the fetch time and,
// when serving a last-good stale list, the fetch error alongside the data.
type weatherAlertsAPI interface {
	RawNWSAlerts(ctx context.Context) (alerts []nws.Alert, fetchedAt time.Time, err error)
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
func (n *WeatherAlertNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	alerts, _, err := n.weather.RawNWSAlerts(ctx)
	if err != nil && alerts == nil {
		// Fetch failed with no usable last-good list: hard error, every
		// covered source records the failure and nothing sweeps.
		return nil, err
	}
	var perSource map[string]error
	if err != nil {
		// Stale serve: RawNWSAlerts returned the last-good list alongside the
		// fetch error. Emit the events (availability) AND report the source
		// degraded — the scheduler skips the disappearance sweep for a source
		// with a PerSource error, so a stale list can never expire alerts it
		// merely failed to refresh.
		perSource = map[string]error{"nws": err}
	}
	now := n.now()

	events := make([]*gridv1.Event, 0, len(alerts))
	for _, a := range alerts {
		// An NWS watch is SCHEDULED until it takes effect (decision 8).
		status := gridv1.EventStatus_ACTIVE
		if !a.Effective.IsZero() && a.Effective.After(now) {
			status = gridv1.EventStatus_SCHEDULED
		}

		ev := NewEvent(
			// services.NWSAlertID is the exact derivation behind the shipped
			// /weather/alerts ids, so "wx:"+id stays byte-identical to them.
			"wx:"+services.NWSAlertID(a),
			gridv1.Layer_WEATHER_ALERT,
			// NWS-direct severity mapping: unlike the api.AlertSeverity
			// projection (which collapses Extreme into CRITICAL => SEVERE),
			// this keeps NWS "Extreme" as EXTREME — a deliberate accuracy
			// improvement over the shipped collapse.
			SeverityFromLabel(hazards.SeverityFromNWSSeverity(a.Severity)),
			status,
			nonEmpty(a.Headline, a.Event), // shipped headline fallback
		)
		ev.Category = a.Event
		ev.Description = a.Description // original NWS text, verbatim
		ev.Effective = tsProto(a.Effective)
		ev.Expires = tsProto(a.Expires)
		// Attach to every configured area whose zones intersect the alert's
		// (a zoneless alert can't be scoped, so it attaches to all areas).
		for _, area := range n.cfg.Hazards.Areas {
			if zonesMatch(area.Zones, a.Zones) {
				ev.PlaceIds = append(ev.PlaceIds, "area:"+area.ID)
			}
		}
		ev.Provenance = NewProvenance("nws", a.SenderName, "", "")
		ev.Detail = &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{
			Event:       a.Event,
			NwsSeverity: a.Severity, // raw NWS vocabulary: Extreme|Severe|Moderate|Minor|Unknown
			Certainty:   a.Certainty,
			Urgency:     a.Urgency,
			Instruction: a.Instruction,
			SenderName:  a.SenderName,
			AreaDesc:    a.AreaDesc,
			Zones:       a.Zones,
		}}
		events = append(events, ev)
	}
	return &PollResult{Events: events, PerSource: perSource}, nil
}
