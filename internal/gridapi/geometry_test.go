package gridapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

// marshalJSON renders a response exactly as the gateway does.
func marshalJSON(t *testing.T, m proto.Message) map[string]any {
	t.Helper()
	b, err := prefab.JSONMarshalOptions.Marshal(m)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// geometryOf digs the geometry object out of a rendered Event.
func geometryOf(t *testing.T, ev map[string]any) map[string]any {
	t.Helper()
	g, ok := ev["geometry"].(map[string]any)
	require.True(t, ok, "event has no geometry: %v", ev)
	return g
}

// OBJECT carries the geometry as a plain JSON object and drops the base64
// copy; the default carries only the base64 string, with no `object` key at
// all (not even null), which is what keeps it byte-compatible.
func TestGeometryFormatObject(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)

	def, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "usgs:q1"})
	require.NoError(t, err)
	dg := geometryOf(t, marshalJSON(t, def))
	assert.NotContains(t, dg, "object", "the default form must not gain a key")
	assert.Equal(t, "eyJ0eXBlIjoiUG9pbnQiLCJjb29yZGluYXRlcyI6Wy0xMjAuMSwzOC40XX0=", dg["geojson"])

	for _, spelling := range []string{"OBJECT", "object", " Object "} {
		obj, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "usgs:q1", GeometryFormat: spelling})
		require.NoError(t, err, spelling)
		og := geometryOf(t, marshalJSON(t, obj))
		assert.Equal(t, map[string]any{"type": "Point", "coordinates": []any{-120.1, 38.4}}, og["object"], spelling)
		assert.Equal(t, "", og["geojson"], "the base64 copy is not sent twice")
		assert.Equal(t, dg["bbox"], og["bbox"])
		assert.Equal(t, dg["centroid"], og["centroid"])
	}

	base64, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "usgs:q1", GeometryFormat: "base64"})
	require.NoError(t, err)
	assert.True(t, proto.Equal(def, base64), "BASE64 spelled out is the default")
}

// Every Geometry-carrying RPC honours the parameter, and an event with a
// location but no GeoJSON gets no object in either form.
func TestGeometryFormatOnEveryRPC(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	const obj = "OBJECT"
	all := []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}

	list, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Status: all, GeometryFormat: obj})
	require.NoError(t, err)
	hist, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "usgs:q1", GeometryFormat: obj})
	require.NoError(t, err)
	arch, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{GeometryFormat: obj})
	require.NoError(t, err)
	places, err := g.ListPlaces(rpcBG(), &gridv1.ListPlacesRequest{GeometryFormat: obj})
	require.NoError(t, err)
	place, err := g.GetPlace(rpcBG(), &gridv1.GetPlaceRequest{Place: "arnold", GeometryFormat: obj})
	require.NoError(t, err)
	resolved, err := g.ResolvePlace(rpcBG(), &gridv1.ResolvePlaceRequest{Lat: "38.25", Lng: "-120.35", GeometryFormat: obj})
	require.NoError(t, err)

	var geoms []*gridv1.Geometry
	geoms = append(geoms, eventGeometries(list.GetEvents())...)
	geoms = append(geoms, revisionGeometries(hist.GetRevisions())...)
	geoms = append(geoms, revisionGeometries(arch.GetRevisions())...)
	geoms = append(geoms, placeGeometries(places.GetPlaces())...)
	geoms = append(geoms, place.GetGeometry())
	geoms = append(geoms, placeGeometries(resolved.GetPlaces())...)
	require.NotEmpty(t, resolved.GetPlaces())

	withGeoJSON := 0
	for _, gm := range geoms {
		if gm == nil {
			continue // wx:a1 has no geometry at all
		}
		assert.Empty(t, gm.GetGeojson(), "OBJECT clears the base64 form")
		if gm.GetObject() != nil {
			withGeoJSON++
			assert.Contains(t, []string{"Point", "Polygon", "MultiPolygon", "LineString", "MultiLineString"},
				gm.GetObject().GetFields()["type"].GetStringValue())
		}
	}
	assert.Greater(t, withGeoJSON, 10, "places and events all carried objects")

	for name, call := range map[string]func() error{
		"ListEvents": func() error {
			_, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{GeometryFormat: "geojson"})
			return err
		},
		"GetEvent": func() error {
			_, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "usgs:q1", GeometryFormat: "geojson"})
			return err
		},
		"GetEventHistory": func() error {
			_, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "usgs:q1", GeometryFormat: "geojson"})
			return err
		},
		"ListHistory": func() error {
			_, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{GeometryFormat: "geojson"})
			return err
		},
		"ListPlaces": func() error {
			_, err := g.ListPlaces(rpcBG(), &gridv1.ListPlacesRequest{GeometryFormat: "geojson"})
			return err
		},
		"GetPlace": func() error {
			_, err := g.GetPlace(rpcBG(), &gridv1.GetPlaceRequest{Place: "arnold", GeometryFormat: "geojson"})
			return err
		},
		"ResolvePlace": func() error {
			_, err := g.ResolvePlace(rpcBG(), &gridv1.ResolvePlaceRequest{Lat: "38.25", Lng: "-120.35", GeometryFormat: "geojson"})
			return err
		},
	} {
		err := call()
		require.Error(t, err, name)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
		assert.Contains(t, status.Convert(err).Message(), "geometry_format", name)
	}
}

// The format changes the body, so it must change the validator: a client
// that cached the base64 body must not get a 304 for the object one, and the
// same format must still revalidate.
func TestETagFollowsGeometryFormat(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)

	calls := map[string]func(format string) func(ctx context.Context) error{
		"ListEvents": func(f string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				_, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{GeometryFormat: f})
				return err
			}
		},
		"GetEvent": func(f string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				_, err := g.GetEvent(ctx, &gridv1.GetEventRequest{Id: "usgs:q1", GeometryFormat: f})
				return err
			}
		},
		"GetEventHistory": func(f string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				_, err := g.GetEventHistory(ctx, &gridv1.GetEventHistoryRequest{Id: "usgs:q1", GeometryFormat: f})
				return err
			}
		},
		"ListHistory": func(f string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				_, err := g.ListHistory(ctx, &gridv1.ListHistoryRequest{GeometryFormat: f})
				return err
			}
		},
		"ListPlaces": func(f string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				_, err := g.ListPlaces(ctx, &gridv1.ListPlacesRequest{GeometryFormat: f})
				return err
			}
		},
		"GetPlace": func(f string) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				_, err := g.GetPlace(ctx, &gridv1.GetPlaceRequest{Place: "arnold", GeometryFormat: f})
				return err
			}
		},
	}
	for name, call := range calls {
		b64 := rpcTag(t, call(""))
		obj := rpcTag(t, call("OBJECT"))
		assert.NotEqual(t, b64, obj, "%s: the two forms share a validator", name)
		assert.Equal(t, b64, rpcTag(t, call("BASE64")), "%s: BASE64 spelled out is the default's tag", name)
		assert.Equal(t, obj, rpcTag(t, call("object")), "%s: the format is case-insensitive in the tag too", name)

		assert.True(t, rpcNotModified(t, obj, call("OBJECT")), "%s: same format revalidates", name)
		assert.False(t, rpcNotModified(t, b64, call("OBJECT")), "%s: a base64 tag must not 304 an object body", name)
		assert.False(t, rpcNotModified(t, obj, call("")), "%s: an object tag must not 304 a base64 body", name)
	}
}

// The byte budget measures the form actually sent. A Struct encodes each
// coordinate as its own double (9 bytes plus framing), so short coordinates
// cost about twice what their JSON text does: a set that fits one page as
// base64 needs several as objects, and every object page still fits.
func TestByteBudgetMeasuresTheChosenGeometryFormat(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	g.pageBudget = testBudget
	ctx := context.Background()

	var sb strings.Builder
	sb.WriteString(`{"type":"MultiPoint","coordinates":[`)
	for i := range 300 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "[-120.%d,38.%d]", i%10, i%7) // short numbers: cheap as text
	}
	sb.WriteString("]}")
	for i := range 10 {
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: fmt.Sprintf("usgs:m%02d", i), Layer: gridv1.Layer_EARTHQUAKE,
			Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE,
			Headline: "aftershock swarm",
			Geometry: &gridv1.Geometry{
				Geojson:  []byte(sb.String()),
				Bbox:     &gridv1.BoundingBox{MinLat: 38, MinLng: -120.9, MaxLat: 38.6, MaxLng: -120},
				Centroid: &gridv1.LatLng{Lat: 38.3, Lng: -120.45},
			},
			ObservedAt: timestamppb.New(base.Add(-time.Duration(i) * time.Minute)),
			Provenance: &gridv1.Provenance{SourceId: "usgs"},
		})
		require.NoError(t, err)
	}
	req := &gridv1.ListEventsRequest{Layer: []string{"EARTHQUAKE"}, PageSize: 200}

	one, err := g.ListEvents(rpcBG(), req)
	require.NoError(t, err)
	require.Empty(t, one.GetNextPageToken(), "base64: all 11 quakes fit one page (%d bytes)", proto.Size(one))

	req.GeometryFormat = "OBJECT"
	whole := NewGridServer(s)
	all, err := whole.ListEvents(rpcBG(), req)
	require.NoError(t, err)
	require.Greater(t, proto.Size(all), testBudget, "the same events as objects are over budget (base64 %d B, objects %d B)", proto.Size(one), proto.Size(all))
	ids, pages := walkEvents(t, g, req) // asserts every page against the budget
	assert.Greater(t, pages, 1, "objects are bigger and must split")
	assert.Equal(t, eventIDs(one.GetEvents()), ids, "same events, same order")
}
