package gridapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/caloes"
	"github.com/dpup/sierra-data/internal/hazards"
)

// ts is a fixed-instant proto timestamp helper.
func ts(rfc string) *timestamppb.Timestamp {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		panic(err)
	}
	return timestamppb.New(t)
}

// geom wraps raw RFC 7946 geometry bytes as a stored event geometry (the
// projection only reads Geojson; bbox/centroid are store indexes).
func geom(raw string) *gridv1.Geometry {
	return &gridv1.Geometry{Geojson: []byte(raw)}
}

// featJSON marshals one projected feature for whole-envelope assertions.
func featJSON(t *testing.T, f hazards.Feature) string {
	t.Helper()
	b, err := json.Marshal(f)
	require.NoError(t, err)
	return string(b)
}

func TestProjectEvents_Wildfire_CalfireAdoptedPerimeter(t *testing.T) {
	// MultiPolygon perimeter: coordinates must pass through verbatim, never
	// re-encoded.
	const multiPoly = `{"type":"MultiPolygon","coordinates":[[[[-120.45,38.15],[-120.35,38.15],[-120.35,38.25],[-120.45,38.25],[-120.45,38.15]]],[[[-120.5,38.3],[-120.48,38.3],[-120.48,38.32],[-120.5,38.32],[-120.5,38.3]]]]}`

	ev := &gridv1.Event{
		Id:           "calfire:abc-123",
		Layer:        gridv1.Layer_WILDFIRE,
		Category:     "wildfire",
		Severity:     gridv1.Severity_SEVERE,
		Status:       gridv1.EventStatus_ACTIVE,
		Headline:     "Salt Springs Fire — 1200 ac, 35% contained",
		AreaLabel:    "near Hwy 4",
		CanonicalUrl: "https://www.fire.ca.gov/incidents/salt-springs",
		Geometry:     geom(multiPoly),
		Effective:    ts("2026-07-01T10:00:00Z"),
		ObservedAt:   ts("2026-07-04T08:00:00Z"),
		Provenance:   &gridv1.Provenance{SourceId: "calfire", SourceName: "CAL FIRE", FetchedAt: timestamppb.Now()},
		Detail: &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{
			Acres: 1200, Containment: 35, County: "Calaveras", HasPerimeter: true,
		}},
	}

	feats := ProjectEvents(hazards.LayerWildfire, []*gridv1.Event{ev})
	require.Len(t, feats, 1)
	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": `+multiPoly+`,
	  "properties": {
	    "id": "calfire:abc-123",
	    "layer": "WILDFIRE",
	    "kind": "Wildfire",
	    "category": "wildfire",
	    "severity": "SEVERE",
	    "severityRank": 3,
	    "headline": "Salt Springs Fire — 1200 ac, 35% contained",
	    "status": "ACTIVE",
	    "effective": "2026-07-01T10:00:00Z",
	    "updatedAt": "2026-07-04T08:00:00Z",
	    "areaLabel": "near Hwy 4",
	    "source": {
	      "id": "calfire",
	      "name": "CAL FIRE",
	      "url": "https://www.fire.ca.gov/incidents/salt-springs",
	      "attribution": "CAL FIRE / FIRIS"
	    },
	    "wildfire": {"acres": 1200, "containment": 35, "county": "Calaveras", "hasPerimeter": true}
	  }
	}`, featJSON(t, feats[0]))
}

func TestProjectEvents_Wildfire_StandaloneFirisPerimeter(t *testing.T) {
	const poly = `{"type":"Polygon","coordinates":[[[-119.9,38.0],[-119.8,38.0],[-119.8,38.1],[-119.9,38.1],[-119.9,38.0]]]}`
	ev := &gridv1.Event{
		Id:         "firis:lonely",
		Layer:      gridv1.Layer_WILDFIRE,
		Category:   "wildfire",
		Severity:   gridv1.Severity_SEVERE,
		Status:     gridv1.EventStatus_ACTIVE,
		Headline:   "Lonely — 250 ac",
		Geometry:   geom(poly),
		Provenance: &gridv1.Provenance{SourceId: "firis", SourceName: "CAL FIRE / FIRIS"},
		Detail: &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{
			Acres: 250.4, HasPerimeter: true, // combo feed carries no containment/cause
		}},
	}

	feats := ProjectEvents(hazards.LayerWildfire, []*gridv1.Event{ev})
	require.Len(t, feats, 1)
	// firis-sourced fires emit the standalone-perimeter source block (no URL —
	// combo-feed features carry none).
	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": `+poly+`,
	  "properties": {
	    "id": "firis:lonely",
	    "layer": "WILDFIRE",
	    "kind": "Wildfire",
	    "category": "wildfire",
	    "severity": "SEVERE",
	    "severityRank": 3,
	    "headline": "Lonely — 250 ac",
	    "status": "ACTIVE",
	    "source": {"id": "firis", "name": "CAL FIRE / FIRIS", "attribution": "CAL FIRE / FIRIS / NIFC"},
	    "wildfire": {"acres": 250.4, "containment": 0, "hasPerimeter": true}
	  }
	}`, featJSON(t, feats[0]))
}

func TestProjectEvents_Evacuation_LevelBecomesStatus(t *testing.T) {
	const poly = `{"type":"Polygon","coordinates":[[[-120.4,38.1],[-120.3,38.1],[-120.3,38.2],[-120.4,38.2],[-120.4,38.1]]]}`
	order := &gridv1.Event{
		Id:          "evac:CAL-E-046",
		Layer:       gridv1.Layer_EVACUATION,
		Category:    "order",
		Severity:    gridv1.Severity_EXTREME,
		Status:      gridv1.EventStatus_ACTIVE,
		Headline:    "Evacuation Order — Zone A",
		Description: "Leave now via Hwy 4. Do not delay.",
		AreaLabel:   "Zone A",
		Geometry:    geom(poly),
		ObservedAt:  ts("2026-06-25T15:06:40Z"),
		Provenance:  &gridv1.Provenance{SourceId: "caloes", SourceName: "Cal OES", SourceUrl: caloes.SourceURL},
		Detail: &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{
			ZoneId: "CAL-E-046", Level: "ORDER", EventType: "Fire", County: "Calaveras",
		}},
	}
	warning := &gridv1.Event{
		Id:       "evac:TUO-E-101",
		Layer:    gridv1.Layer_EVACUATION,
		Category: "warning",
		Severity: gridv1.Severity_SEVERE,
		Status:   gridv1.EventStatus_ACTIVE,
		Headline: "Evacuation Warning — Zone C",
		Geometry: geom(poly),
		Detail: &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{
			ZoneId: "TUO-E-101", Level: "WARNING", County: "Tuolumne",
		}},
	}

	feats := ProjectEvents(hazards.LayerEvacuation, []*gridv1.Event{order, warning})
	require.Len(t, feats, 2)

	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": `+poly+`,
	  "properties": {
	    "id": "evac:CAL-E-046",
	    "layer": "EVACUATION",
	    "kind": "Evacuation",
	    "category": "order",
	    "severity": "EXTREME",
	    "severityRank": 4,
	    "headline": "Evacuation Order — Zone A",
	    "description": "Leave now via Hwy 4. Do not delay.",
	    "status": "ORDER",
	    "updatedAt": "2026-06-25T15:06:40Z",
	    "areaLabel": "Zone A",
	    "source": {
	      "id": "caloes",
	      "name": "Cal OES",
	      "url": "https://protect.genasys.com/",
	      "attribution": "Cal OES — reference only"
	    },
	    "evacuation": {"zoneId": "CAL-E-046", "level": "ORDER", "eventType": "Fire", "county": "Calaveras"}
	  }
	}`, featJSON(t, feats[0]))

	// The envelope status is the coded evacuation LEVEL, not the lifecycle.
	assert.Equal(t, "WARNING", feats[1].Properties.Status)
	assert.Equal(t, "SEVERE", feats[1].Properties.Severity)
	assert.Equal(t, 3, feats[1].Properties.SeverityRank)
}

func TestProjectEvents_WeatherAlert_ZonelessNullGeometry(t *testing.T) {
	ev := &gridv1.Event{
		Id:          "wx:urn:oid:2.49.0.1.840.0.abc",
		Layer:       gridv1.Layer_WEATHER_ALERT,
		Category:    "Red Flag Warning",
		Severity:    gridv1.Severity_SEVERE,
		Status:      gridv1.EventStatus_ACTIVE,
		Headline:    "Red Flag Warning until 8 PM PDT",
		Description: "* WHAT...Gusty winds and low humidity.",
		Effective:   ts("2026-07-04T10:00:00Z"),
		Expires:     ts("2026-07-04T20:00:00Z"),
		Provenance:  &gridv1.Provenance{SourceId: "nws", SourceName: "NWS Sacramento CA"},
		Detail: &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{
			NwsSeverity: "Severe", Certainty: "Likely",
			Urgency: "Expected", Instruction: "Avoid outdoor burning.",
			AreaDesc: "West Slope", Zones: []string{"CAZ064"},
		}},
	}

	feats := ProjectEvents(hazards.LayerWeatherAlert, []*gridv1.Event{ev})
	require.Len(t, feats, 1)
	// Zone alerts carry no geometry: the feature must render "geometry": null
	// (banner feature), and no lifecycle status (the shipped envelope has none).
	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": null,
	  "properties": {
	    "id": "wx:urn:oid:2.49.0.1.840.0.abc",
	    "layer": "WEATHER_ALERT",
	    "kind": "Weather alert",
	    "category": "Red Flag Warning",
	    "severity": "SEVERE",
	    "severityRank": 3,
	    "headline": "Red Flag Warning until 8 PM PDT",
	    "description": "* WHAT...Gusty winds and low humidity.",
	    "effective": "2026-07-04T10:00:00Z",
	    "expires": "2026-07-04T20:00:00Z",
	    "source": {"id": "nws", "name": "NWS Sacramento CA"},
	    "weather": {"event": "Red Flag Warning", "source": "NWS", "zones": ["CAZ064"]}
	  }
	}`, featJSON(t, feats[0]))
}

func TestProjectEvents_Earthquake_UpdatedAtRule(t *testing.T) {
	const point = `{"type":"Point","coordinates":[-120.45,38.2]}`
	revised := &gridv1.Event{
		Id:           "usgs:nc75095123",
		Layer:        gridv1.Layer_EARTHQUAKE,
		Category:     "earthquake",
		Severity:     gridv1.Severity_MODERATE,
		Status:       gridv1.EventStatus_ACTIVE,
		Headline:     "M4.2 — 10km NE of Murphys, CA",
		AreaLabel:    "10km NE of Murphys, CA",
		CanonicalUrl: "https://earthquake.usgs.gov/earthquakes/eventpage/nc75095123",
		Geometry:     geom(point),
		Effective:    ts("2026-06-25T15:06:40Z"),
		ObservedAt:   ts("2026-06-25T15:15:00Z"), // upstream revised the record
		Provenance:   &gridv1.Provenance{SourceId: "usgs", SourceName: "USGS"},
		Detail: &gridv1.Event_Earthquake{Earthquake: &gridv1.EarthquakeDetail{
			Magnitude: 4.2, DepthKm: 7.6, Felt: 37,
		}},
	}
	// Ingest falls observed_at back to the event time when USGS never revised
	// the record; the projection must then OMIT updated_at (plan §5 item 4).
	unrevised := &gridv1.Event{
		Id:         "usgs:nc75095124",
		Layer:      gridv1.Layer_EARTHQUAKE,
		Category:   "earthquake",
		Severity:   gridv1.Severity_MINOR,
		Status:     gridv1.EventStatus_ACTIVE,
		Headline:   "M2.6 — 5km SW of Arnold, CA",
		AreaLabel:  "5km SW of Arnold, CA",
		Geometry:   geom(point),
		Effective:  ts("2026-06-24T11:20:00Z"),
		ObservedAt: ts("2026-06-24T11:20:00Z"),
		Provenance: &gridv1.Provenance{SourceId: "usgs", SourceName: "USGS"},
		Detail:     &gridv1.Event_Earthquake{Earthquake: &gridv1.EarthquakeDetail{Magnitude: 2.6, DepthKm: 3}},
	}

	feats := ProjectEvents(hazards.LayerEarthquake, []*gridv1.Event{revised, unrevised})
	require.Len(t, feats, 2)

	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": `+point+`,
	  "properties": {
	    "id": "usgs:nc75095123",
	    "layer": "EARTHQUAKE",
	    "kind": "Earthquake",
	    "category": "earthquake",
	    "severity": "MODERATE",
	    "severityRank": 2,
	    "headline": "M4.2 — 10km NE of Murphys, CA",
	    "effective": "2026-06-25T15:06:40Z",
	    "updatedAt": "2026-06-25T15:15:00Z",
	    "areaLabel": "10km NE of Murphys, CA",
	    "source": {
	      "id": "usgs",
	      "name": "USGS",
	      "url": "https://earthquake.usgs.gov/earthquakes/eventpage/nc75095123",
	      "attribution": "U.S. Geological Survey"
	    },
	    "earthquake": {"magnitude": 4.2, "depthKm": 7.6, "felt": 37}
	  }
	}`, featJSON(t, feats[0]))

	assert.Empty(t, feats[1].Properties.UpdatedAt,
		"observed_at == effective means USGS never revised the record: updated_at omitted")
	assert.Equal(t, "2026-06-24T11:20:00Z", feats[1].Properties.Effective)
	assert.Empty(t, feats[1].Properties.Source.URL, "no canonical URL projects no source URL")
}

func TestProjectEvents_RoadIncident_ConstantSourceBlock(t *testing.T) {
	const point = `{"type":"Point","coordinates":[-120.35,38.2]}`
	chp := &gridv1.Event{
		Id:          "chp:250916ST0066",
		Layer:       gridv1.Layer_ROAD_INCIDENT,
		Category:    "incident",
		Severity:    gridv1.Severity_SEVERE,
		Status:      gridv1.EventStatus_ACTIVE,
		Headline:    "Vehicle fire on Hwy 4",                // short condensed line
		Summary:     "Vehicle fire blocking the right lane", // AI narrative → GeoJSON description
		Description: "1141 VEH FIRE RHS SR4",                // verbatim original — /v1-event only, not projected
		AreaLabel:   "Hwy 4 at Avery",
		Geometry:    geom(point),
		Effective:   ts("2026-07-04T06:24:00Z"),
		ObservedAt:  ts("2026-07-04T07:00:00Z"),
		Provenance:  &gridv1.Provenance{SourceId: "chp", SourceName: "CHP / Caltrans"},
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
			LogNumber: "250916ST0066", Impact: "severe",
		}},
	}
	// Lane closures carry caltrans provenance in the STORE (source health), but
	// the shipped envelope always used the one constant block — the projection
	// must too (plan §5 item 5).
	closure := &gridv1.Event{
		Id:         "chp:closure-hwy-4-avery",
		Layer:      gridv1.Layer_ROAD_INCIDENT,
		Category:   "closure",
		Severity:   gridv1.Severity_MODERATE,
		Status:     gridv1.EventStatus_ACTIVE,
		Headline:   "One-way traffic control for utility work",
		AreaLabel:  "Hwy 4 EB near Avery",
		Geometry:   geom(point),
		Provenance: &gridv1.Provenance{SourceId: "caltrans", SourceName: "Caltrans"},
		Detail:     &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{}},
	}

	feats := ProjectEvents(hazards.LayerRoadIncident, []*gridv1.Event{chp, closure})
	require.Len(t, feats, 2)

	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": `+point+`,
	  "properties": {
	    "id": "chp:250916ST0066",
	    "layer": "ROAD_INCIDENT",
	    "kind": "Road incident",
	    "category": "incident",
	    "severity": "SEVERE",
	    "severityRank": 3,
	    "headline": "Vehicle fire on Hwy 4",
	    "description": "Vehicle fire blocking the right lane",
	    "status": "ACTIVE",
	    "effective": "2026-07-04T06:24:00Z",
	    "updatedAt": "2026-07-04T07:00:00Z",
	    "areaLabel": "Hwy 4 at Avery",
	    "source": {"id": "chp", "name": "CHP / Caltrans", "attribution": "quickmap.dot.ca.gov"},
	    "incident": {"logNumber": "250916ST0066"}
	  }
	}`, featJSON(t, feats[0]))

	// caltrans-sourced closure still emits the constant chp block, an empty
	// incident kind block (no log number), and no effective (no dispatch time).
	cl := feats[1]
	assert.Equal(t, hazards.Source{ID: "chp", Name: "CHP / Caltrans", Attribution: "quickmap.dot.ca.gov"}, cl.Properties.Source)
	require.NotNil(t, cl.Properties.Incident)
	assert.Empty(t, cl.Properties.Incident.LogNumber)
	assert.Empty(t, cl.Properties.Effective)
	assert.Equal(t, "ACTIVE", cl.Properties.Status)
}

func TestProjectEvents_Network_MeshNode(t *testing.T) {
	const point = `{"type":"Point","coordinates":[-120.4579,38.1374]}`
	node := &gridv1.Event{
		Id:           "meshcore:aa11bb22",
		Layer:        gridv1.Layer_MESH,
		Category:     "repeater",
		Severity:     gridv1.Severity_INFO,
		Status:       gridv1.EventStatus_ACTIVE,
		Headline:     "Murphys Ridge (repeater)",
		AreaLabel:    "Murphys Ridge",
		CanonicalUrl: "https://map.meshcore.io",
		Geometry:     geom(point),
		ObservedAt:   ts("2026-07-15T10:00:00Z"),
		Provenance:   &gridv1.Provenance{SourceId: "meshcore", SourceName: "MeshCore Mesh"},
		Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{
			PublicKey: "aa11bb22", NodeType: "repeater", Name: "Murphys Ridge",
			Telemetry: &gridv1.MeshTelemetry{
				Snr: 4.5, Rssi: -93, HopCount: 2, Gateways: []string{"ag loft rpt"},
			},
		}},
	}

	feats := ProjectEvents(hazards.LayerMesh, []*gridv1.Event{node})
	require.Len(t, feats, 1)
	assert.JSONEq(t, `{
	  "type": "Feature",
	  "geometry": `+point+`,
	  "properties": {
	    "id": "meshcore:aa11bb22",
	    "layer": "MESH_NODE",
	    "kind": "Mesh node",
	    "category": "repeater",
	    "severity": "INFO",
	    "severityRank": 0,
	    "headline": "Murphys Ridge (repeater)",
	    "status": "ACTIVE",
	    "updatedAt": "2026-07-15T10:00:00Z",
	    "areaLabel": "Murphys Ridge",
	    "source": {"id": "meshcore", "name": "MeshCore Mesh", "url": "https://map.meshcore.io", "attribution": "MeshCore community mesh"},
	    "mesh": {"publicKey": "aa11bb22", "nodeType": "repeater", "name": "Murphys Ridge", "snr": 4.5, "rssi": -93, "hopCount": 2, "gateways": ["ag loft rpt"]}
	  }
	}`, featJSON(t, feats[0]))
}

func TestProjectEvents_SkipsUnknownLayer(t *testing.T) {
	ev := &gridv1.Event{Id: "x", Layer: gridv1.Layer_WILDFIRE}
	assert.Empty(t, ProjectEvents("chain_control", []*gridv1.Event{ev}),
		"condition-backed layers are never store-projected")
	assert.Empty(t, ProjectEvents("nope", []*gridv1.Event{ev}))
	assert.NotNil(t, ProjectEvents(hazards.LayerWildfire, nil), "non-nil empty slice keeps features [] not null")
}

func TestProjectEvents_ScrubsUnsafeURLs(t *testing.T) {
	ev := &gridv1.Event{
		Id:           "usgs:evil",
		Layer:        gridv1.Layer_EARTHQUAKE,
		Severity:     gridv1.Severity_MINOR,
		Status:       gridv1.EventStatus_ACTIVE,
		CanonicalUrl: "javascript:alert(1)",
		Detail:       &gridv1.Event_Earthquake{Earthquake: &gridv1.EarthquakeDetail{Magnitude: 3}},
	}
	feats := ProjectEvents(hazards.LayerEarthquake, []*gridv1.Event{ev})
	require.Len(t, feats, 1)
	assert.Empty(t, feats[0].Properties.Source.URL, "non-http(s) URLs never reach the envelope")
	assert.Nil(t, feats[0].Geometry, "missing stored geometry projects as null geometry")
}
