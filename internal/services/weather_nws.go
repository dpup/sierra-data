package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"
	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/nws"
)

// fetchNWSAlerts returns the active NWS alerts for the configured service-area
// zones, caching the raw alert list so it is fetched at most once per weather
// refresh and shared between zone-alert listing, per-location alerts, and
// fire-weather classification. A fetch failure with no usable cache returns an
// error so callers on the alerts path stay fail-loud (the hazards layer maps
// that to source_status UNAVAILABLE rather than a fabricated clear state).
func (s *WeatherService) fetchNWSAlerts(ctx context.Context) ([]nws.Alert, error) {
	if s.nwsClient == nil || len(s.config.Weather.NWS.Zones) == 0 {
		return nil, nil
	}

	var cached []nws.Alert
	if found, _ := s.cache.Get(nwsAlertsCacheKey, &cached); found {
		return cached, nil
	}

	alerts, err := s.nwsClient.GetActiveZoneAlerts(ctx, s.config.Weather.NWS.Zones)
	if err != nil {
		logging.Errorw(ctx, "Failed to fetch NWS zone alerts", "error", err)
		return nil, fmt.Errorf("failed to fetch NWS zone alerts: %w", err)
	}

	if err := s.cache.Set(nwsAlertsCacheKey, alerts, s.config.Weather.RefreshInterval, "nws_alerts"); err != nil {
		logging.Errorw(ctx, "Failed to cache NWS alerts", "error", err)
	}
	logging.Infow(ctx, "Fetched NWS zone alerts", "zones", s.config.Weather.NWS.Zones, "count", len(alerts))
	return alerts, nil
}

const nwsAlertsCacheKey = "nws:alerts"

// getNWSAlerts is fetchNWSAlerts for consumers where alerts are supplementary
// (fire-weather classification, per-location alerts on the current-conditions
// path): a transient NWS failure falls back to the last-good list while it is
// within the very-stale bound (2x refresh interval), and otherwise yields no
// alerts rather than failing the whole response.
func (s *WeatherService) getNWSAlerts(ctx context.Context) []nws.Alert {
	alerts, err := s.fetchNWSAlerts(ctx)
	if err == nil {
		return alerts
	}
	var cached []nws.Alert
	if _, found, _ := s.cache.GetWithMetadata(nwsAlertsCacheKey, &cached); found && !s.cache.IsVeryStale(nwsAlertsCacheKey) {
		return cached
	}
	return nil
}

// RawNWSAlerts returns the raw NWS zone alerts — the full nws.Alert records
// (certainty/urgency/instruction/areaDesc, unmapped NWS severity), which the
// api.WeatherAlert projection drops — plus the time they were fetched from
// NWS. It rides the shared nws:alerts cache (one fetch per refresh interval,
// no second fetch path). Consumers that need honest freshness (the grid
// weather_alert poller) use it as follows:
//
//   - fresh cache hit  => (alerts, cache CreatedAt, nil)
//   - successful fetch => (alerts, now, nil)
//   - fetch failure with a cache entry inside the very-stale bound
//     => (stale alerts, cache CreatedAt, fetch error) — BOTH data and error,
//     so the caller can keep serving last-good while reporting the source
//     degraded rather than either lying about freshness or dropping data
//   - fetch failure with no usable cache => (nil, zero time, error)
func (s *WeatherService) RawNWSAlerts(ctx context.Context) (alerts []nws.Alert, fetchedAt time.Time, err error) {
	// Fresh cache hit: serve it with its actual fetch time.
	var cached []nws.Alert
	if entry, found, cerr := s.cache.GetWithMetadata(nwsAlertsCacheKey, &cached); cerr == nil && found && !s.cache.IsStale(nwsAlertsCacheKey) {
		return cached, entry.CreatedAt, nil
	}

	fresh, err := s.fetchNWSAlerts(ctx)
	if err == nil {
		return fresh, time.Now(), nil
	}

	// Fetch failed: fall back to the last-good list while it is within the
	// very-stale bound, still surfacing the fetch error so the caller can
	// mark the source degraded instead of treating stale data as current.
	var stale []nws.Alert
	if entry, found, cerr := s.cache.GetWithMetadata(nwsAlertsCacheKey, &stale); cerr == nil && found && !s.cache.IsVeryStale(nwsAlertsCacheKey) {
		logging.Errorw(ctx, "NWS fetch failed, returning stale alerts alongside the error",
			"error", err, "fetched_at", entry.CreatedAt.Format(time.RFC3339))
		return stale, entry.CreatedAt, err
	}
	return nil, time.Time{}, err
}

// nwsAlertsForZone filters the shared NWS alert list down to alerts active in a
// single forecast zone, for attaching to the weather location in that zone.
// An empty zone returns nothing (an unzoned location gets no alerts).
func (s *WeatherService) nwsAlertsForZone(ctx context.Context, zone string) []*api.WeatherAlert {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return nil
	}
	var out []*api.WeatherAlert
	for _, a := range nwsAlertsToProto(s.getNWSAlerts(ctx)) {
		for _, z := range a.Zones {
			if strings.EqualFold(strings.TrimSpace(z), zone) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// nwsAlertsToProto converts NWS alerts into API WeatherAlerts tagged with the
// NWS source. NWS provides its own authoritative headline + description, so we
// surface those two distinct fields as-is and leave the AI-enhancement slots
// (summary/details) empty rather than copying the same text four times. This
// keeps the official wording and avoids per-alert OpenAI cost.
func nwsAlertsToProto(alerts []nws.Alert) []*api.WeatherAlert {
	var out []*api.WeatherAlert
	for _, a := range alerts {
		wa := &api.WeatherAlert{
			Id:          NWSAlertID(a),
			SenderName:  a.SenderName,
			Event:       a.Event,
			Headline:    a.Headline,    // short, authoritative one-liner
			Description: a.Description, // full, authoritative text
			Source:      api.AlertSource_NWS,
			Severity:    mapNWSSeverity(a.Severity),
			Zones:       a.Zones,
		}
		if !a.Effective.IsZero() {
			wa.StartTime = timestamppb.New(a.Effective)
		}
		if !a.Expires.IsZero() {
			wa.EndTime = timestamppb.New(a.Expires)
		}
		out = append(out, wa)
	}
	return out
}

// NWSAlertID is the stable alert-id derivation used for every NWS alert this
// service ships (nwsAlertsToProto): the alert's own ID when present, else a
// synthesized "nws_<event>_<unix>". Exported so other consumers of the raw
// alerts (the grid poller's "wx:"+id namespace) derive exactly the ids that
// already exist on the wire.
func NWSAlertID(a nws.Alert) string {
	if a.ID != "" {
		return a.ID
	}
	return fmt.Sprintf("nws_%s_%d", a.Event, a.Effective.Unix())
}

// computeRegionFireWeather classifies fire-weather risk for the whole service
// area from the shared NWS alert list. Fire-weather products are regional, so a
// single classification applies to every monitored location.
func (s *WeatherService) computeRegionFireWeather(ctx context.Context) *api.FireWeather {
	fw := nws.ClassifyFireWeather(s.getNWSAlerts(ctx), s.config.Weather.NWS.Zones)
	out := &api.FireWeather{
		State:       mapFireWeatherState(fw.State),
		SourceEvent: fw.SourceEvent,
		Headline:    fw.Headline,
		SenderName:  fw.SenderName,
		Zones:       fw.Zones,
	}
	if !fw.Effective.IsZero() {
		out.Effective = timestamppb.New(fw.Effective)
	}
	if !fw.Expires.IsZero() {
		out.Expires = timestamppb.New(fw.Expires)
	}
	return out
}

// mapNWSSeverity maps NWS severity terms onto the shared AlertSeverity scale.
func mapNWSSeverity(s string) api.AlertSeverity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "extreme", "severe":
		return api.AlertSeverity_CRITICAL
	case "moderate":
		return api.AlertSeverity_WARNING
	case "minor":
		return api.AlertSeverity_INFO
	default:
		return api.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED
	}
}

// mapFireWeatherState maps the nws package's string state to the proto enum.
func mapFireWeatherState(s string) api.FireWeatherState {
	switch s {
	case nws.FireWeatherNormal:
		return api.FireWeatherState_NORMAL
	case nws.FireWeatherElevated:
		return api.FireWeatherState_ELEVATED
	case nws.FireWeatherRedFlag:
		return api.FireWeatherState_RED_FLAG
	default:
		return api.FireWeatherState_FIRE_WEATHER_STATE_UNSPECIFIED
	}
}
