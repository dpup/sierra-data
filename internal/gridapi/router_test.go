package gridapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
)

func TestRouterMethodNotAllowed(t *testing.T) {
	s := newTestService(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/events", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		requireStatus(t, rec, http.StatusMethodNotAllowed, 12)
		assert.Equal(t, "GET", rec.Header().Get("Allow"))
	}
}

func TestRouterNotFound(t *testing.T) {
	s := newTestService(t)
	for _, path := range []string{
		"/v1/",                          // bare prefix
		"/v1/nope",                      // unknown collection
		"/v1/events/",                   // trailing slash / empty segment
		"/v1/events/usgs:q1/nothistory", // bad sub-resource
		"/v1/events/usgs:q1/history/x",  // too deep
		"/v1/history/extra",
		"/v1/places/calaveras/other",
		"/v1/places/calaveras/map/wildfire", // missing .geojson
		"/v1/roads/hwy-4/extra",
		"/v1/scanners/extra",
		"/v1/sources/extra",
	} {
		rec := get(t, s, path)
		sb := requireStatus(t, rec, http.StatusNotFound, 5)
		assert.NotEmpty(t, sb.Message, path)
	}
}

// The T12b endpoints are routed but stubbed at 501 until summary.go /
// maplayers.go land.
func TestRouterT12bStubs(t *testing.T) {
	s := newTestService(t)
	rec := get(t, s, "/v1/places/calaveras/summary")
	requireStatus(t, rec, http.StatusNotImplemented, 12)

	rec = get(t, s, "/v1/places/calaveras/map/wildfire.geojson")
	requireStatus(t, rec, http.StatusNotImplemented, 12)
}

// Error bodies are google.rpc.Status protojson and marked non-cacheable.
func TestErrorBodyShape(t *testing.T) {
	s := newTestService(t)
	rec := get(t, s, "/v1/nope")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("ETag"))
	var sb statusBody
	decode(t, rec, &sb)
	assert.Equal(t, 5, sb.Code)
	assert.NotEmpty(t, sb.Message)
}

func TestETagRoundTrip(t *testing.T) {
	s := newTestService(t)

	first := get(t, s, "/v1/sources")
	require.Equal(t, http.StatusOK, first.Code)
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)
	require.Len(t, etag, 34, "16-byte hex digest, quoted") // 32 hex chars + 2 quotes
	assert.Equal(t, "public, max-age=30", first.Header().Get("Cache-Control"))

	// Same resource, same process => same ETag.
	again := get(t, s, "/v1/sources")
	require.Equal(t, etag, again.Header().Get("ETag"))

	t.Run("if-none-match hit", func(t *testing.T) {
		rec := get(t, s, "/v1/sources", "If-None-Match", etag)
		require.Equal(t, http.StatusNotModified, rec.Code)
		assert.Zero(t, rec.Body.Len())
		// 304 keeps the validator + freshness headers.
		assert.Equal(t, etag, rec.Header().Get("ETag"))
		assert.Equal(t, "public, max-age=30", rec.Header().Get("Cache-Control"))
	})
	t.Run("weak compare", func(t *testing.T) {
		rec := get(t, s, "/v1/sources", "If-None-Match", "W/"+etag)
		assert.Equal(t, http.StatusNotModified, rec.Code)
	})
	t.Run("any-of list", func(t *testing.T) {
		rec := get(t, s, "/v1/sources", "If-None-Match", `"deadbeef", `+etag+`, "cafe"`)
		assert.Equal(t, http.StatusNotModified, rec.Code)
	})
	t.Run("star", func(t *testing.T) {
		rec := get(t, s, "/v1/sources", "If-None-Match", "*")
		assert.Equal(t, http.StatusNotModified, rec.Code)
	})
	t.Run("miss", func(t *testing.T) {
		rec := get(t, s, "/v1/sources", "If-None-Match", `"0123456789abcdef0123456789abcdef"`)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotZero(t, rec.Body.Len())
	})
}

// Accept: application/proto negotiates binary proto; its ETag must differ
// from the JSON rendering's (content type is part of the validator input).
func TestEventsProtoContentNegotiation(t *testing.T) {
	s := newTestService(t)

	jsonRec := get(t, s, "/v1/events")
	require.Equal(t, http.StatusOK, jsonRec.Code)
	jsonIDs, _ := eventIDs(t, jsonRec)

	rec := get(t, s, "/v1/events", "Accept", "application/proto")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/proto", rec.Header().Get("Content-Type"))

	var list gridv1.EventList
	require.NoError(t, proto.Unmarshal(rec.Body.Bytes(), &list))
	var protoIDs []string
	for _, ev := range list.GetEvents() {
		protoIDs = append(protoIDs, ev.GetId())
	}
	assert.Equal(t, jsonIDs, protoIDs, "same events on both renderings")

	assert.NotEqual(t, jsonRec.Header().Get("ETag"), rec.Header().Get("ETag"),
		"JSON and proto renderings must not share a validator")

	t.Run("proto 304", func(t *testing.T) {
		etag := rec.Header().Get("ETag")
		rec2 := get(t, s, "/v1/events", "Accept", "application/proto", "If-None-Match", etag)
		assert.Equal(t, http.StatusNotModified, rec2.Code)
	})
}
