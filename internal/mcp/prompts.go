package mcp

import (
	"encoding/json"
	"fmt"
)

// One prompt template: a hazard briefing for a location that steers the model
// through the tools and the honesty contract.

func promptList() []map[string]interface{} {
	return []map[string]interface{}{{
		"name":        "hazard_briefing",
		"description": "Produce a concise, honest hazard briefing for a place, address, or lat,lng.",
		"arguments": []map[string]interface{}{{
			"name":        "location",
			"description": "a place slug, a street address, or \"lat,lng\"",
			"required":    true,
		}},
	}}
}

func getPrompt(params json.RawMessage) rpcResponse {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcResponse{Error: &rpcError{errBadParams, "invalid params: " + err.Error()}}
	}
	if p.Name != "hazard_briefing" {
		return rpcResponse{Error: &rpcError{errBadParams, "prompt not found: " + p.Name}}
	}
	loc := p.Arguments["location"]
	if loc == "" {
		loc = "the requested location"
	}
	text := fmt.Sprintf(`Produce a concise hazard briefing for %q using the grid tools.

Steps:
1. grid_situation(%q) — lead with the area mode and any active evacuations.
2. grid_events(location=%q) — summarize active wildfire/weather/road/earthquake/power events, most severe first.
3. grid_conditions(location=%q) — note road and weather conditions if relevant.

Rules:
- This is reference-only, life-safety data. Never present absence of data as an all-clear.
- If any source is UNAVAILABLE (see grid_sources), say that status is UNKNOWN, not clear.
- For evacuations: null = unknown (tell the user to check Genasys), 0 = no active zones reported (caveated, not a guarantee), N = active. Do not collapse null and 0.
- Cite the canonicalUrl / source links; render evacuation orders verbatim.`, loc, loc, loc, loc)

	return rpcResponse{Result: map[string]interface{}{
		"description": "Hazard briefing for " + loc,
		"messages": []map[string]interface{}{{
			"role":    "user",
			"content": map[string]interface{}{"type": "text", "text": text},
		}},
	}}
}
