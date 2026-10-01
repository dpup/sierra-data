package gridapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dpup/prefab/plugins/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/lib/geojson"
)

// fakeCameras is a canned CameraDirectory.
type fakeCameras struct {
	cams    []cwwp2.Camera
	status  string
	last    time.Time
	version string
}

func (f *fakeCameras) Cameras() ([]cwwp2.Camera, string, time.Time, string) {
	return f.cams, f.status, f.last, f.version
}

// fakeStream lets etag.Guard set response headers outside a real gRPC server.
type fakeStream struct{ hdr metadata.MD }

func (f *fakeStream) Method() string { return "/grid.v1.GridService/ListCameras" }
func (f *fakeStream) SetHeader(md metadata.MD) error {
	f.hdr = metadata.Join(f.hdr, md)
	return nil
}
func (f *fakeStream) SendHeader(md metadata.MD) error { return f.SetHeader(md) }
func (f *fakeStream) SetTrailer(metadata.MD) error    { return nil }

func rpcCtx() (context.Context, *fakeStream) {
	fs := &fakeStream{}
	return grpc.NewContextWithServerTransportStream(context.Background(), fs), fs
}

// cam places a camera at a real coordinate from the 2026-10-01 captures.
func cam(id string, lat, lng float64) cwwp2.Camera {
	return cwwp2.Camera{
		ID:                  id,
		Name:                "camera " + id,
		Location:            cwwp2.Location{Latitude: lat, Longitude: lng, HasPosition: true, Route: "SR-108", County: "Tuolumne", ElevationFt: 2925.6},
		InService:           true,
		ImageURL:            "https://cwwp2.dot.ca.gov/data/" + id + ".jpg",
		ImageRefreshMinutes: 2,
	}
}

// Against testConfig's two bbox areas (calaveras, high-country) at the default
// 25 km radius.
var (
	camSoulsbyville = cam("d10-172", 37.992423, -120.274801) // inside calaveras
	camPineGrove    = cam("d10-136", 38.40774, -120.65039)   // inside calaveras
	camSonoraJct    = cam("d9-47", 38.35058, -119.45038)     // ~4.4 km east of high-country
	camLathrop      = cam("d10-2", 37.84451, -121.28509)     // ~34 km west of calaveras: Valley
	camMammoth      = cam("d9-1", 37.64111, -118.91848)      // far outside
)

func camerasServer(t *testing.T, dir CameraDirectory) *GridServer {
	t.Helper()
	svc := newTestService(t)
	if dir != nil {
		svc.Cameras = dir
	}
	return NewGridServer(svc)
}

func ids(list *gridv1.CameraList) []string {
	var out []string
	for _, c := range list.GetCameras() {
		out = append(out, c.GetId())
	}
	return out
}

func TestListCameras_GeographyAndOrder(t *testing.T) {
	g := camerasServer(t, &fakeCameras{
		cams:    []cwwp2.Camera{camLathrop, camSonoraJct, camSoulsbyville, camMammoth, camPineGrove},
		status:  "OK",
		version: "v1",
	})
	ctx, _ := rpcCtx()
	list, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{})
	require.NoError(t, err)

	// Valley and far-east cameras are outside the geography; inside-area ties
	// order by id; the near-miss camera follows with its distance.
	assert.Equal(t, []string{"d10-136", "d10-172", "d9-47"}, ids(list))
	assert.Equal(t, "OK", list.GetSourceStatus())
	assert.Nil(t, list.GetLastSourceUpdate())
	assert.Equal(t, "Caltrans", list.GetAttribution())

	assert.Zero(t, list.GetCameras()[0].GetDistanceMeters())
	sj := list.GetCameras()[2].GetDistanceMeters()
	assert.InDelta(t, 4400, sj, 300, "Sonora Junction is ~0.05° of longitude past high-country")

	// The wire projection.
	c := list.GetCameras()[1]
	assert.Equal(t, "camera d10-172", c.GetName())
	assert.Equal(t, "SR-108", c.GetRoute())
	assert.Equal(t, "Tuolumne", c.GetCounty())
	assert.Equal(t, int32(2926), c.GetElevationFeet())
	assert.Equal(t, int32(2), c.GetImageRefreshMinutes())
	assert.Equal(t, "https://cwwp2.dot.ca.gov/data/d10-172.jpg", c.GetImageUrl())
	assert.InDelta(t, 37.992423, c.GetLocation().GetLat(), 1e-9)
	assert.InDelta(t, -120.274801, c.GetLocation().GetLng(), 1e-9)
}

func TestListCameras_PlaceRadius(t *testing.T) {
	g := camerasServer(t, &fakeCameras{
		cams:    []cwwp2.Camera{camSonoraJct, camSoulsbyville, camPineGrove, camLathrop},
		status:  "OK",
		version: "v1",
	})

	// A county contains none of them, but two are within the radius — nearest
	// first (Pine Grove sits just over the Mokelumne from Calaveras).
	ctx, _ := rpcCtx()
	list, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: "county:calaveras-county"})
	require.NoError(t, err)
	assert.Equal(t, []string{"d10-136", "d10-172"}, ids(list))
	assert.Less(t, list.GetCameras()[0].GetDistanceMeters(), list.GetCameras()[1].GetDistanceMeters())
	assert.Positive(t, list.GetCameras()[0].GetDistanceMeters(), "outside the county, so not 0")

	// A town is a point: only the radius can match it.
	ctx, _ = rpcCtx()
	list, err = g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: "murphys"})
	require.NoError(t, err)
	require.Equal(t, []string{"d10-172"}, ids(list))
	want := geojson.MetersBetween(38.1377, -120.4610, 37.992423, -120.274801)
	assert.InDelta(t, want, list.GetCameras()[0].GetDistanceMeters(), 1)

	// A place never widens the geography: Lathrop is near no coverage area, so
	// it stays out even when the place is beside it.
	ctx, _ = rpcCtx()
	list, err = g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: "county:san-joaquin-county"})
	require.NoError(t, err)
	assert.NotContains(t, ids(list), "d10-2")
}

func TestListCameras_UnknownPlace(t *testing.T) {
	g := camerasServer(t, &fakeCameras{status: "OK", version: "v1"})
	ctx, _ := rpcCtx()
	_, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: "atlantis"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestListCameras_NotConfiguredIsUnavailable(t *testing.T) {
	g := camerasServer(t, nil)
	ctx, _ := rpcCtx()
	list, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{})
	require.NoError(t, err)
	assert.Empty(t, list.GetCameras())
	assert.Equal(t, "UNAVAILABLE", list.GetSourceStatus(), "no districts configured must not read as 'no cameras here'")

	// The place is still validated first.
	_, err = g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: "atlantis"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestListCameras_StaleCarriesFetchTime(t *testing.T) {
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g := camerasServer(t, &fakeCameras{cams: []cwwp2.Camera{camSoulsbyville}, status: "STALE", last: fetched, version: "v1"})
	ctx, _ := rpcCtx()
	list, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"d10-172"}, ids(list))
	assert.Equal(t, "STALE", list.GetSourceStatus())
	assert.Equal(t, fetched, list.GetLastSourceUpdate().AsTime())
}

func TestListCameras_ETag(t *testing.T) {
	dir := &fakeCameras{cams: []cwwp2.Camera{camSoulsbyville}, status: "OK", version: "v1"}
	g := camerasServer(t, dir)

	ctx, fs := rpcCtx()
	_, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{})
	require.NoError(t, err)
	tags := fs.hdr.Get("grpc-metadata-etag")
	require.Len(t, tags, 1)
	tag := tags[0]

	conditional := func(place string) error {
		ctx, _ := rpcCtx()
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("if-none-match", tag))
		_, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: place})
		return err
	}
	assert.ErrorIs(t, conditional(""), etag.ErrNotModified)
	assert.NoError(t, conditional("murphys"), "the place is part of the validator")

	dir.version = "v2"
	assert.NoError(t, conditional(""), "a refreshed list invalidates the validator")
}

// cameraLayer fetches a place's camera.geojson through the hand-built route.
func cameraLayer(t *testing.T, dir CameraDirectory, place string) hazards.FeatureCollection {
	t.Helper()
	svc := newTestService(t)
	if dir != nil {
		svc.Cameras = dir
	}
	rec := get(t, svc, "/v1/places/"+place+"/map/camera.geojson")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/geo+json", rec.Header().Get("Content-Type"))
	var fc hazards.FeatureCollection
	decode(t, rec, &fc)
	return fc
}

func featureIDs(fc hazards.FeatureCollection) []string {
	var out []string
	for _, f := range fc.Features {
		out = append(out, f.Properties.ID)
	}
	return out
}

// The layer is the place-scoped RPC as GeoJSON: same cameras, same order, same
// distances, same ids.
func TestCameraLayer_MatchesListCameras(t *testing.T) {
	dir := &fakeCameras{
		cams:    []cwwp2.Camera{camSonoraJct, camSoulsbyville, camPineGrove, camLathrop},
		status:  "OK",
		version: "v1",
	}
	fc := cameraLayer(t, dir, "county:calaveras-county")
	require.Equal(t, []string{"d10-136", "d10-172"}, featureIDs(fc))

	g := camerasServer(t, dir)
	ctx, _ := rpcCtx()
	list, err := g.ListCameras(ctx, &gridv1.ListCamerasRequest{Place: "county:calaveras-county"})
	require.NoError(t, err)
	require.Equal(t, ids(list), featureIDs(fc))
	for i, f := range fc.Features {
		assert.Equal(t, list.GetCameras()[i].GetDistanceMeters(), f.Properties.Camera.DistanceMeters)
	}

	md := fc.Metadata
	require.NotNil(t, md)
	assert.Equal(t, "camera", md.Layer)
	assert.Equal(t, "calaveras-county", md.Area)
	assert.Equal(t, "OK", md.SourceStatus)
	assert.Empty(t, md.LastSourceUpdate)
	assert.Equal(t, "Caltrans", md.Attribution)
	assert.Equal(t, mapSchemaVersion, md.SchemaVersion)
}

func TestCameraLayer_Feature(t *testing.T) {
	c := camSoulsbyville
	c.Location.NearbyPlace = "Soulsbyville"
	c.Location.Direction = "East"
	c.StreamURL = "https://wzmedia.dot.ca.gov/D10/TUO_EB108_WO_Soulsbyville.stream/playlist.m3u8"
	c.Description = "Looking East"
	fc := cameraLayer(t, &fakeCameras{cams: []cwwp2.Camera{c}, status: "OK", version: "v1"}, "calaveras")
	require.Len(t, fc.Features, 1)
	f := fc.Features[0]

	require.NotNil(t, f.Geometry)
	assert.Equal(t, "Point", f.Geometry.Type)
	b, err := json.Marshal(f.Geometry.Coordinates)
	require.NoError(t, err)
	assert.JSONEq(t, `[-120.2748, 37.99242]`, string(b), "[lng, lat], trimmed to 5 decimals")

	p := f.Properties
	assert.Equal(t, "d10-172", p.ID, "the same id as ListCameras")
	assert.Equal(t, "CAMERA", p.Layer)
	assert.Equal(t, "Traffic camera", p.Kind)
	assert.Equal(t, "INFO", p.Severity, "a camera is a view, not a hazard")
	assert.Zero(t, p.SeverityRank)
	assert.Equal(t, "camera d10-172", p.Headline)
	assert.Equal(t, "Looking East", p.Description)
	assert.Equal(t, "Soulsbyville", p.AreaLabel)
	assert.Equal(t, "Caltrans", p.Source.Attribution)
	assert.Equal(t, c.ImageURL, p.Source.URL)

	cp := p.Camera
	require.NotNil(t, cp)
	assert.Equal(t, c.ImageURL, cp.ImageURL)
	assert.Equal(t, c.StreamURL, cp.StreamURL)
	assert.Equal(t, int32(2), cp.ImageRefreshMinutes)
	assert.Equal(t, "SR-108", cp.Route)
	assert.Equal(t, "East", cp.Direction)
	assert.Equal(t, "Tuolumne", cp.County)
	assert.Equal(t, int32(2926), cp.ElevationFeet)
	assert.Zero(t, cp.DistanceMeters, "inside the calaveras area")
}

// distanceMeters is serialized even when 0 ("inside"), and an image-only
// camera omits streamUrl rather than sending "".
func TestCameraLayer_WireShape(t *testing.T) {
	svc := newTestService(t)
	svc.Cameras = &fakeCameras{cams: []cwwp2.Camera{camSoulsbyville}, status: "OK", version: "v1"}
	rec := get(t, svc, "/v1/places/calaveras/map/camera.geojson")
	require.Equal(t, http.StatusOK, rec.Code)
	var raw struct {
		Features []struct {
			Properties struct {
				Camera map[string]any `json:"camera"`
			} `json:"properties"`
		} `json:"features"`
	}
	decode(t, rec, &raw)
	require.Len(t, raw.Features, 1)
	cp := raw.Features[0].Properties.Camera
	assert.Contains(t, cp, "distanceMeters")
	assert.NotContains(t, cp, "streamUrl")
	assert.Equal(t, "https://cwwp2.dot.ca.gov/data/d10-172.jpg", cp["imageUrl"])
}

func TestCameraLayer_EmptyPlaceIsOK(t *testing.T) {
	// Lathrop is outside the geography, so high-country lists nothing: a
	// healthy list with nothing near is OK + 0, not UNAVAILABLE.
	fc := cameraLayer(t, &fakeCameras{cams: []cwwp2.Camera{camLathrop}, status: "OK", version: "v1"}, "high-country")
	assert.Empty(t, fc.Features)
	assert.NotNil(t, fc.Features, "an empty layer is still a FeatureCollection with []")
	assert.Equal(t, "OK", fc.Metadata.SourceStatus)
}

func TestCameraLayer_NotConfiguredIsUnavailable(t *testing.T) {
	fc := cameraLayer(t, nil, "calaveras")
	assert.Empty(t, fc.Features)
	assert.Equal(t, "UNAVAILABLE", fc.Metadata.SourceStatus, "no districts configured must not read as 'no cameras here'")
	assert.Equal(t, "Caltrans", fc.Metadata.Attribution)
}

func TestCameraLayer_StaleCarriesFetchTime(t *testing.T) {
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fc := cameraLayer(t, &fakeCameras{cams: []cwwp2.Camera{camSoulsbyville}, status: "STALE", last: fetched, version: "v1"}, "calaveras")
	assert.Equal(t, []string{"d10-172"}, featureIDs(fc), "a stale list still draws")
	assert.Equal(t, "STALE", fc.Metadata.SourceStatus)
	assert.Equal(t, "2026-10-01T12:00:00Z", fc.Metadata.LastSourceUpdate)
}

func TestCameraLayer_UnknownPlace(t *testing.T) {
	svc := newTestService(t)
	svc.Cameras = &fakeCameras{status: "OK", version: "v1"}
	rec := get(t, svc, "/v1/places/atlantis/map/camera.geojson")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
