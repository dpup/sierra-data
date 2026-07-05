package gridapi

import (
	"encoding/json"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
)

// ProjectEvents projects stored grid events onto the shipped GeoJSON envelope
// (docs/v2-implementation-plan.md T13). The output must stay byte-compatible
// with the live internal/hazards builders — internal/hazards/
// project_compat_test.go is the gate — modulo exactly the plan §5 exclusion
// list. Read the builders before changing any field here.
//
// layer is the shipped layer slug (hazards.Layer*); events not of an
// event-backed layer are skipped (condition layers are never store-backed).
// Lifecycle is the CALLER's concern: this projects whatever it is given, so a
// caller serving the live map must query only ACTIVE/SCHEDULED events.
func ProjectEvents(layer string, events []*gridv1.Event) []hazards.Feature {
	out := make([]hazards.Feature, 0, len(events))
	for _, ev := range events {
		var f hazards.Feature
		switch layer {
		case hazards.LayerWildfire:
			f = projectWildfire(ev)
		case hazards.LayerEvacuation:
			f = projectEvacuation(ev)
		case hazards.LayerWeatherAlert:
			f = projectWeatherAlert(ev)
		case hazards.LayerEarthquake:
			f = projectEarthquake(ev)
		case hazards.LayerRoadIncident:
			f = projectRoadIncident(ev)
		default:
			continue // not an event-backed layer
		}
		out = append(out, f)
	}
	return out
}

// projectWildfire mirrors the shipped wildfires builder. The source block is a
// projection constant derived from the provenance source id (plan §5 item 5):
// CAL FIRE incidents (adopted-perimeter or point) emit the calfire block with
// the event's canonical URL; standalone WFIGS perimeters emit the wfigs block.
func projectWildfire(ev *gridv1.Event) hazards.Feature {
	d := ev.GetWildfire()
	p := baseProps(ev, hazards.LayerWildfire, "Wildfire")
	p.Status = lifecycleStatus(ev) // shipped hardcoded "active"; ACTIVE renders identically
	p.Effective = rfc3339(ev.GetEffective())
	p.UpdatedAt = rfc3339(ev.GetObservedAt())
	if ev.GetProvenance().GetSourceId() == "wfigs" {
		p.Source = hazards.Source{ID: "wfigs", Name: "NIFC WFIGS", Attribution: "NIFC / WFIGS"}
	} else {
		p.Source = hazards.Source{ID: "calfire", Name: "CAL FIRE", URL: safeURL(ev.GetCanonicalUrl()), Attribution: "CAL FIRE / WFIGS"}
	}
	p.Wildfire = &hazards.WildfireProps{
		Acres:        d.GetAcres(),
		Containment:  d.GetContainment(),
		County:       d.GetCounty(),
		Cause:        d.GetCause(),
		HasPerimeter: d.GetHasPerimeter(),
	}
	return feature(ev, p)
}

// projectEvacuation mirrors the shipped evacuations builder. properties.status
// is the coded evacuation LEVEL (ORDER|WARNING|ADVISORY|SHELTER_IN_PLACE), not
// the event lifecycle — that is the shipped contract. Description is the
// Cal OES PUBLIC_INFO carried verbatim (life-safety: never paraphrased), and
// the source block always links the authoritative Genasys viewer.
func projectEvacuation(ev *gridv1.Event) hazards.Feature {
	d := ev.GetEvacuation()
	p := baseProps(ev, hazards.LayerEvacuation, "Evacuation")
	p.Description = ev.GetDescription()
	p.Status = d.GetLevel()
	p.UpdatedAt = rfc3339(ev.GetObservedAt())
	p.Source = hazards.Source{ID: "caloes", Name: "Cal OES", URL: caloes.SourceURL, Attribution: "Cal OES — reference only"}
	p.Evacuation = &hazards.EvacuationProps{
		ZoneID:    d.GetZoneId(),
		Level:     d.GetLevel(),
		EventType: d.GetEventType(),
		County:    d.GetCounty(),
	}
	return feature(ev, p)
}

// projectWeatherAlert mirrors the shipped weatherAlerts builder: null-geometry
// banner features (NWS zone alerts carry zones, not polygons — an event
// without geometry projects a null geometry), no lifecycle status on the
// envelope, source name = the NWS sender from provenance.
func projectWeatherAlert(ev *gridv1.Event) hazards.Feature {
	d := ev.GetWeatherAlert()
	p := baseProps(ev, hazards.LayerWeatherAlert, "Weather alert")
	p.Description = ev.GetDescription()
	p.Effective = rfc3339(ev.GetEffective())
	p.Expires = rfc3339(ev.GetExpires())
	p.Source = hazards.Source{ID: "nws", Name: ev.GetProvenance().GetSourceName()}
	p.Weather = &hazards.WeatherProps{
		Event: d.GetEvent(),
		// The shipped block carried the api.AlertSource enum name; every stored
		// weather alert is NWS-sourced (OpenWeatherMap alerts were removed
		// 2026-07-04), so this is a projection constant.
		Source: "NWS",
		Zones:  d.GetZones(),
	}
	return feature(ev, p)
}

// projectEarthquake mirrors the shipped earthquakes builder. updated_at rule
// (plan §5 item 4): ingest falls observed_at back to the event time when USGS
// never revised the record, and the shipped builder omitted updated_at exactly
// when the Updated stamp was zero — so the projection omits it when
// observed_at == effective.
func projectEarthquake(ev *gridv1.Event) hazards.Feature {
	d := ev.GetEarthquake()
	p := baseProps(ev, hazards.LayerEarthquake, "Earthquake")
	p.Effective = rfc3339(ev.GetEffective())
	if obs, eff := ev.GetObservedAt(), ev.GetEffective(); obs != nil && !(eff != nil && obs.AsTime().Equal(eff.AsTime())) {
		p.UpdatedAt = rfc3339(obs)
	}
	p.Source = hazards.Source{ID: "usgs", Name: "USGS", URL: safeURL(ev.GetCanonicalUrl()), Attribution: "U.S. Geological Survey"}
	p.Earthquake = &hazards.EarthquakeProps{
		Magnitude: d.GetMagnitude(),
		DepthKm:   d.GetDepthKm(),
		Felt:      d.GetFelt(),
	}
	return feature(ev, p)
}

// projectRoadIncident mirrors the shipped roadIncidents builder. The source
// block is the shipped per-layer CONSTANT for ALL road incidents — store
// provenance keeps the chp/caltrans split for source health, but the envelope
// never varied by feed (plan §5 item 5). properties.status is the lowercase
// lifecycle ("active" for the live feeds).
func projectRoadIncident(ev *gridv1.Event) hazards.Feature {
	d := ev.GetRoadIncident()
	p := baseProps(ev, hazards.LayerRoadIncident, "Road incident")
	p.Status = lifecycleStatus(ev)
	p.Effective = rfc3339(ev.GetEffective())
	p.UpdatedAt = rfc3339(ev.GetObservedAt())
	p.Source = hazards.Source{ID: "chp", Name: "CHP / Caltrans", Attribution: "quickmap.dot.ca.gov"}
	p.Incident = &hazards.IncidentProps{LogNumber: d.GetLogNumber()}
	return feature(ev, p)
}

// --- shared projection plumbing ---

// baseProps fills the envelope fields every layer derives the same way:
// id/category/headline/area_label pass through from the event, severity is the
// enum name (the proto enum IS the shipped INFO..EXTREME scale) and its rank
// is the enum value (INFO=0 .. EXTREME=4, pinned by the proto).
func baseProps(ev *gridv1.Event, layer, kind string) hazards.Properties {
	return hazards.Properties{
		ID:           ev.GetId(),
		Layer:        layer,
		Kind:         kind,
		Category:     ev.GetCategory(),
		Severity:     ev.GetSeverity().String(),
		SeverityRank: int(ev.GetSeverity().Number()),
		Headline:     ev.GetHeadline(),
		AreaLabel:    ev.GetAreaLabel(),
	}
}

// feature wraps props with the event's geometry as an RFC 7946 Feature.
func feature(ev *gridv1.Event, p hazards.Properties) hazards.Feature {
	return hazards.Feature{Type: "Feature", Geometry: geometryFromEvent(ev), Properties: p}
}

// geometryFromEvent decodes the stored RFC 7946 geometry bytes into the
// envelope Geometry WITHOUT re-encoding coordinates: the coordinates stay a
// json.RawMessage passed through verbatim (the shipped RawGeom convention), so
// upstream number formatting survives the store round-trip byte-for-byte. A
// missing geometry projects as null (banner feature, valid per the model).
func geometryFromEvent(ev *gridv1.Event) *hazards.Geometry {
	raw := ev.GetGeometry().GetGeojson()
	if len(raw) == 0 {
		return nil
	}
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		// Ingest validated these bytes; unparseable here means a corrupt row —
		// degrade to a null-geometry banner rather than emit broken GeoJSON.
		return nil
	}
	return hazards.RawGeom(g.Type, g.Coordinates)
}

// lifecycleStatus renders the event lifecycle as the shipped lowercase status
// slug ("active", "scheduled", "resolved", "expired").
func lifecycleStatus(ev *gridv1.Event) string {
	return strings.ToLower(ev.GetStatus().String())
}

// rfc3339 formats a proto timestamp as the shipped RFC 3339 string, "" for
// nil/zero (matching the builders' tsToRFC3339/tsOrEmpty omit-when-zero).
func rfc3339(ts *timestamppb.Timestamp) string {
	if ts == nil || ts.GetSeconds() == 0 {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// safeURL keeps only http(s) URLs — the shipped builders scrub untrusted
// upstream URLs (javascript:/data: XSS vectors) and the projection re-applies
// the rule as defense in depth even though ingest already scrubbed.
func safeURL(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}
