package cwwp2

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cctv fixtures are trimmed 2026-10-01 captures (rows removed, row content
// untouched); see tests/testdata/cwwp2/README.md for what each one keeps.

func camerasByID(cams []Camera) map[string]Camera {
	out := make(map[string]Camera, len(cams))
	for _, c := range cams {
		out[c.ID] = c
	}
	return out
}

func TestParseCameras_D10(t *testing.T) {
	cams, err := ParseCameras(10, fixture(t, "cctv_d10_20261001.json"))
	require.NoError(t, err)
	require.Len(t, cams, 13)
	by := camerasByID(cams)

	// The camera issue #14 named: Hwy 108 west of Soulsbyville.
	c := by["d10-172"]
	assert.Equal(t, 10, c.District)
	assert.Equal(t, "EB 108 W/O Soulsbyville Rd", c.Name, "D10's sequence prefix is stripped")
	assert.Equal(t, "179 - EB 108 W/O Soulsbyville Rd", c.Location.Name, "the location keeps the verbatim name")
	assert.Equal(t, "Soulsbyville", c.Location.NearbyPlace)
	assert.Equal(t, "Tuolumne", c.Location.County)
	assert.Equal(t, "SR-108", c.Location.Route)
	assert.Equal(t, "", c.Location.Direction, "this row has no direction")
	assert.True(t, c.Location.HasPosition)
	assert.InDelta(t, 37.992423, c.Location.Latitude, 1e-9)
	assert.InDelta(t, -120.274801, c.Location.Longitude, 1e-9)
	assert.InDelta(t, 2926, c.Location.ElevationFt, 1e-9)
	assert.True(t, c.InService)
	assert.Equal(t, "https://cwwp2.dot.ca.gov/data/d10/cctv/image/179eb108wosoulsbyvillerd/179eb108wosoulsbyvillerd.jpg", c.ImageURL)
	assert.Equal(t, "https://wzmedia.dot.ca.gov/D10/TUO_EB108_WO_Soulsbyville.stream/playlist.m3u8", c.StreamURL)
	assert.Equal(t, 2, c.ImageRefreshMinutes)

	// Out of service: the flag is surfaced, not filtered — that is the caller's
	// policy.
	for _, id := range []string{"d10-41", "d10-65", "d10-76"} {
		assert.False(t, by[id].InService, id)
		assert.NotEmpty(t, by[id].ImageURL, id)
	}

	// An image-only D10 camera: blank streamingVideoURL.
	assert.Empty(t, by["d10-47"].StreamURL)
	assert.NotEmpty(t, by["d10-47"].ImageURL)
}

func TestParseCameras_D9ImageOnly(t *testing.T) {
	cams, err := ParseCameras(9, fixture(t, "cctv_d9_20261001.json"))
	require.NoError(t, err)
	require.Len(t, cams, 5)
	by := camerasByID(cams)

	// The east side of Sonora Pass. District 9 publishes no streams.
	c := by["d9-47"]
	assert.Equal(t, "US-395 : SR-108 Sonora Junction", c.Name, "only D10 names carry a sequence prefix")
	assert.Equal(t, "Mono", c.Location.County)
	assert.Equal(t, 5, c.ImageRefreshMinutes)
	for _, cam := range cams {
		assert.Empty(t, cam.StreamURL, cam.ID)
		assert.NotEmpty(t, cam.ImageURL, cam.ID)
	}
	assert.Equal(t, "Looking South", by["d9-22"].Description)
	assert.False(t, by["d9-48"].InService)
}

func TestParseCameras_D3Quirks(t *testing.T) {
	cams, err := ParseCameras(3, fixture(t, "cctv_d3_20261001.json"))
	require.NoError(t, err)
	by := camerasByID(cams)

	// "Not Reported" is not a refresh rate.
	assert.Equal(t, 0, by["d3-238"].ImageRefreshMinutes)
	// The upstream name carries a leading space.
	assert.Equal(t, "Hwy 99 at E Eaton Rd 1", by["d3-238"].Name)
	// Flagged out of service, yet serving live images on 2026-10-01: the flag
	// is conservative. Kept as published.
	assert.False(t, by["d3-328"].InService)
	assert.Equal(t, 1, by["d3-106"].ImageRefreshMinutes)
}

func TestParseCameras_EmptyAndMalformed(t *testing.T) {
	_, err := ParseCameras(10, []byte(`{"data": []}`))
	assert.ErrorIs(t, err, ErrEmptyFeed)
	_, err = ParseCameras(10, []byte(`{"data": [`))
	assert.Error(t, err)
}

func TestParseCameras_RejectsNonHTTPSURLs(t *testing.T) {
	body := []byte(`{"data": [{"cctv": {"index": "1", "location": {"locationName": "x", "latitude": "38", "longitude": "-120"},
		"inService": "true", "imageData": {"streamingVideoURL": "javascript:alert(1)",
		"static": {"currentImageURL": "http://cwwp2.dot.ca.gov/a.jpg", "currentImageUpdateFrequency": "-3"}}}}]}`)
	cams, err := ParseCameras(10, body)
	require.NoError(t, err)
	require.Len(t, cams, 1)
	assert.Empty(t, cams[0].ImageURL)
	assert.Empty(t, cams[0].StreamURL)
	assert.Equal(t, 0, cams[0].ImageRefreshMinutes)
}

func TestHTTPSURL(t *testing.T) {
	assert.Equal(t, "https://a.example/x.jpg", httpsURL(" https://a.example/x.jpg "))
	for _, bad := range []string{"", "http://a.example/x.jpg", "https:///x.jpg", "/data/x.jpg", "javascript:alert(1)", "%zz"} {
		assert.Empty(t, httpsURL(bad), bad)
	}
}

func TestClient_Cameras(t *testing.T) {
	d := &fakeDoer{status: 200, body: fixture(t, "cctv_d9_20261001.json")}
	cams, err := pinnedClient(d).Cameras(context.Background(), 9)
	require.NoError(t, err)
	assert.Len(t, cams, 5)
	assert.Equal(t, []string{"https://cwwp2.dot.ca.gov/data/d9/cctv/cctvStatusD09.json"}, d.urls)

	_, err = pinnedClient(&fakeDoer{status: 500}).Cameras(context.Background(), 10)
	assert.Error(t, err)
	_, err = pinnedClient(&fakeDoer{err: errors.New("dial tcp: timeout")}).Cameras(context.Background(), 10)
	assert.Error(t, err)
	_, err = pinnedClient(&fakeDoer{status: 200, body: []byte(`{"data": []}`)}).Cameras(context.Background(), 10)
	assert.ErrorIs(t, err, ErrEmptyFeed)
}
