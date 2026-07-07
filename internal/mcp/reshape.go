package mcp

import "encoding/json"

// LLM-shaping: /v1 responses carry base64 GeoJSON geometry that is huge and
// useless to a model. We drop it and keep a compact `location` centroid + bbox,
// and trim long fields in list views. The fail-loud fields (source_status,
// evacuation_status/active_evacuations, provenance/source_url, canonical_url)
// are always preserved.

// leanEvent strips geometry from an event and, in list mode, drops the long
// description. Mutates and returns the same map.
func leanEvent(ev map[string]interface{}, full bool) map[string]interface{} {
	if ev == nil {
		return ev
	}
	if g, ok := ev["geometry"].(map[string]interface{}); ok {
		// Replace the geometry object with just the centroid + bbox.
		loc := map[string]interface{}{}
		if c, ok := g["centroid"]; ok {
			loc["centroid"] = c
		}
		if b, ok := g["bbox"]; ok {
			loc["bbox"] = b
		}
		delete(ev, "geometry")
		if len(loc) > 0 {
			ev["location"] = loc
		}
	}
	if !full {
		// List view: the headline + summary carry the gist; the verbatim
		// description can be long. Fetch the full event via grid_event for it.
		delete(ev, "description")
	}
	return ev
}

// leanEvents applies leanEvent across an events array (list mode).
func leanEvents(arr []interface{}) []interface{} {
	for i, e := range arr {
		if m, ok := e.(map[string]interface{}); ok {
			arr[i] = leanEvent(m, false)
		}
	}
	return arr
}

// leanPlace strips a place's geometry down to centroid + bbox.
func leanPlace(p map[string]interface{}) map[string]interface{} {
	if p == nil {
		return p
	}
	if g, ok := p["geometry"].(map[string]interface{}); ok {
		loc := map[string]interface{}{}
		if c, ok := g["centroid"]; ok {
			loc["centroid"] = c
		}
		if b, ok := g["bbox"]; ok {
			loc["bbox"] = b
		}
		delete(p, "geometry")
		if len(loc) > 0 {
			p["location"] = loc
		}
	}
	return p
}

// leanPlaces applies leanPlace across a places array.
func leanPlaces(arr []interface{}) []interface{} {
	for i, e := range arr {
		if m, ok := e.(map[string]interface{}); ok {
			arr[i] = leanPlace(m)
		}
	}
	return arr
}

// asArray coerces a JSON value to a slice (nil-safe).
func asArray(v interface{}) []interface{} {
	if a, ok := v.([]interface{}); ok {
		return a
	}
	return nil
}

// prettyJSON renders a value as indented JSON for the tool's text content.
func prettyJSON(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
