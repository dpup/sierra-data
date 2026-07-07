package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGrid stands in for the gridapi /v1 handler with canned responses.
type fakeGrid struct{}

func (fakeGrid) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/events/e1":
		_, _ = w.Write([]byte(`{"id":"e1","layer":"wildfire","severity":"EXTREME","headline":"Big Fire — 5000 ac","description":"verbatim long text","geometry":{"geojson":"HUGE_BASE64_BLOB","centroid":{"lat":38.1,"lng":-120.4},"bbox":{"min_lat":38}},"canonical_url":"https://fire.ca.gov/x"}`))
	case r.URL.Path == "/v1/events":
		_, _ = w.Write([]byte(`{"events":[{"id":"e1","layer":"wildfire","headline":"Big Fire","description":"long","geometry":{"geojson":"HUGE","centroid":{"lat":38,"lng":-120}}}],"next_page_token":""}`))
	case r.URL.Path == "/v1/places/ebbetts-pass":
		_, _ = w.Write([]byte(`{"id":"area:ebbetts-pass","slug":"ebbetts-pass","kind":"AREA","name":"Ebbetts Pass","geometry":{"geojson":"POLY","centroid":{"lat":38.2,"lng":-120.1}}}`))
	case strings.HasPrefix(r.URL.Path, "/v1/places/") && strings.HasSuffix(r.URL.Path, "/summary"):
		_, _ = w.Write([]byte(`{"mode":"ACTIVE","summary":{"active_evacuations":null,"evacuation_status":"UNAVAILABLE"}}`))
	case r.URL.Path == "/v1/places/resolve":
		_, _ = w.Write([]byte(`{"places":[{"slug":"arnold","kind":"TOWN","name":"Arnold","geometry":{"geojson":"PT"}},{"slug":"calaveras-county","kind":"COUNTY","name":"Calaveras County"}]}`))
	case r.URL.Path == "/v1/sources":
		_, _ = w.Write([]byte(`{"sources":[{"id":"nws","status":"OK"}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":5,"message":"not found"}`))
	}
}

func call(t *testing.T, s *Server, method string, params string) map[string]interface{} {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	body += `}`
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("%s: HTTP %d", method, rr.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: bad json: %v", method, err)
	}
	if e, ok := resp["error"]; ok {
		t.Fatalf("%s: rpc error: %v", method, e)
	}
	return resp["result"].(map[string]interface{})
}

// structured pulls the structuredContent object out of a tools/call result.
func structured(t *testing.T, s *Server, tool, args string) map[string]interface{} {
	t.Helper()
	res := call(t, s, "tools/call", `{"name":"`+tool+`","arguments":`+args+`}`)
	if res["isError"] == true {
		t.Fatalf("%s returned isError: %v", tool, res["structuredContent"])
	}
	return res["structuredContent"].(map[string]interface{})
}

func TestInitializeAndList(t *testing.T) {
	s := NewHandler(fakeGrid{})
	init := call(t, s, "initialize", "")
	if init["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	tools := call(t, s, "tools/list", "")["tools"].([]interface{})
	if len(tools) != 8 {
		t.Errorf("got %d tools, want 8", len(tools))
	}
	if len(call(t, s, "resources/list", "")["resources"].([]interface{})) == 0 {
		t.Error("no resources")
	}
	if len(call(t, s, "prompts/list", "")["prompts"].([]interface{})) == 0 {
		t.Error("no prompts")
	}
}

func TestGetMethodIs405(t *testing.T) {
	s := NewHandler(fakeGrid{})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", rr.Code)
	}
}

func TestGridEvent_GeometryStripped_DescriptionKept(t *testing.T) {
	s := NewHandler(fakeGrid{})
	ev := structured(t, s, "grid_event", `{"id":"e1"}`)
	if _, ok := ev["geometry"]; ok {
		t.Error("geometry should be stripped")
	}
	if _, ok := ev["location"]; !ok {
		t.Error("location (centroid) should be present")
	}
	if ev["description"] == nil {
		t.Error("full event should keep description")
	}
	if ev["disclaimer"] == nil {
		t.Error("disclaimer must be present")
	}
	// The base64 blob must not survive anywhere.
	if strings.Contains(prettyJSON(ev), "HUGE_BASE64_BLOB") {
		t.Error("base64 geometry leaked into output")
	}
}

func TestGridEvents_ListDropsDescription(t *testing.T) {
	s := NewHandler(fakeGrid{})
	out := structured(t, s, "grid_events", `{}`)
	events := out["events"].([]interface{})
	e0 := events[0].(map[string]interface{})
	if _, ok := e0["geometry"]; ok {
		t.Error("list geometry should be stripped")
	}
	if _, ok := e0["description"]; ok {
		t.Error("list view should drop description")
	}
}

func TestGridSituation_ResolvesAndPreservesFailLoud(t *testing.T) {
	s := NewHandler(fakeGrid{})
	// A place slug resolves directly.
	out := structured(t, s, "grid_situation", `{"location":"ebbetts-pass"}`)
	if out["resolved_place"] != "ebbetts-pass" {
		t.Errorf("resolved_place = %v", out["resolved_place"])
	}
	sum := out["summary"].(map[string]interface{})
	// Fail-loud: active_evacuations null (unknown) must survive as JSON null.
	if v, ok := sum["active_evacuations"]; !ok || v != nil {
		t.Errorf("active_evacuations should be explicit null, got %v (present=%v)", v, ok)
	}
	if sum["evacuation_status"] != "UNAVAILABLE" {
		t.Errorf("evacuation_status = %v", sum["evacuation_status"])
	}
}

// failGrid returns HTTP 500 with a google.rpc.Status body for every path.
type failGrid struct{}

func (failGrid) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"code":13,"message":"boom"}`))
}

// callRaw returns the full JSON-RPC response (result or error).
func callRaw(t *testing.T, s *Server, body string) map[string]interface{} {
	t.Helper()
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return resp
}

func TestUpstreamErrorSurfacesAsToolError(t *testing.T) {
	s := NewHandler(failGrid{})
	// grid_sources must NOT return an empty/success body when /v1/sources fails —
	// it must report the failure (fail-loud: an error is never a clean result).
	res := call(t, s, "tools/call", `{"name":"grid_sources","arguments":{}}`)
	if res["isError"] != true {
		t.Fatalf("failed /v1/sources should yield isError:true, got %v", res["isError"])
	}
	sc := res["structuredContent"].(map[string]interface{})
	if sc["error"] == nil {
		t.Error("error message should be present")
	}
	// grid_events must not collapse an upstream 500 into events:[] / count:0.
	ev := call(t, s, "tools/call", `{"name":"grid_events","arguments":{}}`)
	if ev["isError"] != true {
		t.Errorf("failed /v1/events should be isError, not an empty list; got %v", ev["isError"])
	}
}

func TestConditionsMarksSubfetchUnavailable(t *testing.T) {
	s := NewHandler(failGrid{})
	out := structured(t, s, "grid_conditions", `{}`)
	roads, ok := out["roads"].(map[string]interface{})
	if !ok || roads["status"] != "unavailable" {
		t.Errorf("roads should be marked unavailable, got %v", out["roads"])
	}
}

func TestParseErrorHasNullId(t *testing.T) {
	s := NewHandler(fakeGrid{})
	resp := callRaw(t, s, `{not valid json`)
	if _, ok := resp["error"]; !ok {
		t.Fatal("parse error should return an error object")
	}
	// JSON-RPC 2.0: id MUST be present and null when undeterminable.
	if v, ok := resp["id"]; !ok || v != nil {
		t.Errorf("parse-error id should be explicit null, got %v (present=%v)", v, ok)
	}
}

func TestUnknownToolIsJSONRPCError(t *testing.T) {
	s := NewHandler(fakeGrid{})
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"grid_nope","arguments":{}}}`)
	e, ok := resp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("unknown tool should be a JSON-RPC error, not a result; got %v", resp)
	}
	if int(e["code"].(float64)) != errBadParams {
		t.Errorf("unknown tool code = %v, want %d", e["code"], errBadParams)
	}
}

func TestNotificationReturns202(t *testing.T) {
	s := NewHandler(fakeGrid{})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	if rr.Code != http.StatusAccepted {
		t.Errorf("notification = %d, want 202", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("notification should have empty body, got %q", rr.Body.String())
	}
}

func TestNoManualCORSHeader(t *testing.T) {
	// CORS is prefab's securityMiddleware's job; the handler must not set it.
	s := NewHandler(fakeGrid{})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("handler set Access-Control-Allow-Origin=%q; should defer to middleware", got)
	}
}

func TestGridSituation_AddressResolves(t *testing.T) {
	s := NewHandler(fakeGrid{})
	out := structured(t, s, "grid_situation", `{"location":"1225 Main St, Angels Camp"}`)
	if out["resolved_place"] != "arnold" { // fake resolve returns arnold first (most-specific)
		t.Errorf("resolved_place = %v", out["resolved_place"])
	}
}
