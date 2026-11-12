package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Conditional GET end to end, through the real gateway of two real Grid
// processes on one database (a writer and a reader-only process): every read
// RPC with a validator answers 200 with an ETag and Cache-Control, the same
// ETag from both processes, and 304 with no body on If-None-Match; no
// gRPC-metadata header leaks onto the response. Until 2026-11-12 neither
// header reached HTTP clients at all (gridapi.OutgoingHeaderMatcher says why).
func TestConditionalGetThroughTheGateway(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	eventID := seedPlaceAndEvent(t, dbURL)

	a, portA := preparedGrid(t, dbURL)
	ha := runGrid(t, a, false)
	require.Eventually(t, a.DatabaseHeld, 20*time.Second, 20*time.Millisecond)
	b, portB := preparedGrid(t, dbURL)
	off := false
	b.cfg.Grid.Writer.Enabled = &off
	hb := runGrid(t, b, false)
	require.Equal(t, http.StatusOK, httpCode(t, portB, "/readyz"))

	guarded := []string{
		"/api/v1/events",
		"/api/v1/events?layer=mesh&page_size=5",
		"/api/v1/events/" + eventID,
		"/api/v1/events/" + eventID + "/history",
		"/api/v1/events/" + eventID + "/timeline",
		"/api/v1/events/" + eventID + "/related",
		"/api/v1/history",
		"/api/v1/places",
		"/api/v1/places/etag-test",
		// /api/v1/cameras is guarded too, but only with the camera directory
		// configured; this config has none (an UNAVAILABLE list, no tag).
	}
	for _, path := range guarded {
		ra := get(t, portA, path, "")
		rb := get(t, portB, path, "")
		require.Equal(t, http.StatusOK, ra.code, "%s on the writer: %s", path, ra.body)
		require.Equal(t, http.StatusOK, rb.code, "%s on the reader: %s", path, rb.body)
		for _, r := range []resp{ra, rb} {
			assert.NotEmpty(t, r.header.Get("ETag"), "%s: ETag", path)
			assert.Equal(t, "public, max-age=30", r.header.Get("Cache-Control"), "%s: Cache-Control", path)
			assertNoMetadataHeaders(t, path, r.header)
		}
		assert.Equal(t, ra.header.Get("ETag"), rb.header.Get("ETag"), "%s: both processes agree", path)

		// The tag one process gave is honoured by the other.
		nm := get(t, portB, path, ra.header.Get("ETag"))
		assert.Equal(t, http.StatusNotModified, nm.code, "%s: If-None-Match", path)
		assert.Empty(t, nm.body, "%s: a 304 has no body", path)
		assert.Equal(t, ra.header.Get("ETag"), nm.header.Get("ETag"), "%s: the 304 repeats the tag", path)
		assert.Equal(t, "public, max-age=30", nm.header.Get("Cache-Control"), "%s: the 304 carries Cache-Control", path)
		assertNoMetadataHeaders(t, path, nm.header)

		assert.Equal(t, http.StatusOK, get(t, portA, path, `W/"something-else"`).code, "%s: a stale tag gets the body", path)
	}

	// An RPC without a validator still gets Cache-Control, and no ETag.
	for _, path := range []string{"/api/v1/sources", "/api/v1/cameras"} {
		r := get(t, portB, path, "")
		require.Equal(t, http.StatusOK, r.code, path)
		assert.Empty(t, r.header.Get("ETag"), path)
		assert.Equal(t, "public, max-age=30", r.header.Get("Cache-Control"), path)
		assertNoMetadataHeaders(t, path, r.header)
	}

	// A write on the writer moves the tag on both processes: the old tag is
	// a 200 again on the reader (LISTEN/NOTIFY, or at worst the 10 s poll).
	const path = "/api/v1/events"
	before := get(t, portB, path, "").header.Get("ETag")
	_, err := a.store.UpsertEvent(context.Background(), meshEvent("bb", "Second node"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return get(t, portB, path, before).code == http.StatusOK },
		15*time.Second, 50*time.Millisecond, "the reader saw the write")
	after := get(t, portA, path, "").header.Get("ETag")
	assert.NotEqual(t, before, after)
	assert.Equal(t, after, get(t, portB, path, "").header.Get("ETag"))

	assert.Equal(t, 0, hb.terminate())
	assert.Equal(t, 0, ha.terminate())
}

type resp struct {
	code   int
	header http.Header
	body   string
}

func get(t *testing.T, port int, path, ifNoneMatch string) resp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	require.NoError(t, err)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	r, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return resp{code: r.StatusCode, header: r.Header, body: string(body)}
}

// Only the headers the RPCs mean to send: no "Grpc-Metadata-*" duplicate and
// none of the "Grpcgateway-*" names prefab's matcher gave gRPC's own metadata.
func assertNoMetadataHeaders(t *testing.T, path string, h http.Header) {
	t.Helper()
	for k := range h {
		lk := strings.ToLower(k)
		assert.False(t, strings.HasPrefix(lk, "grpc-") || strings.HasPrefix(lk, "grpcgateway-") ||
			lk == "x-http-code" || lk == "prefab-not-modified", "%s: leaked header %s", path, k)
	}
}

// seedPlaceAndEvent adds a place (slug etag-test) to the seeded database and
// returns the id of its mesh node.
func seedPlaceAndEvent(t *testing.T, dbURL string) string {
	t.Helper()
	st, err := store.Open(dbURL)
	require.NoError(t, err)
	defer st.Close()
	ctx := context.Background()
	require.NoError(t, st.UpsertPlace(ctx, &gridv1.Place{Id: "area:etag-test", Slug: "etag-test", Name: "ETag test",
		Kind: gridv1.PlaceKind_AREA, Geometry: &gridv1.Geometry{Centroid: &gridv1.LatLng{Lat: 38.2, Lng: -120.4}}}))
	return "meshcore:aa" + strings.Repeat("0", 62)
}

func meshEvent(prefix, name string) *gridv1.Event {
	pk := prefix + strings.Repeat("0", 62)
	return &gridv1.Event{
		Id: "meshcore:" + pk, Layer: gridv1.Layer_MESH, Severity: gridv1.Severity_INFO,
		Status: gridv1.EventStatus_ACTIVE, Headline: name,
		Provenance: &gridv1.Provenance{SourceId: "meshcore"},
		Geometry:   &gridv1.Geometry{Centroid: &gridv1.LatLng{Lat: 38.21, Lng: -120.41}},
		Detail:     &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: pk, Name: name}},
	}
}
