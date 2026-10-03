package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// fakeCameraFetcher serves canned per-district results; errs win over cams.
type fakeCameraFetcher struct {
	mu    sync.Mutex
	cams  map[int][]cwwp2.Camera
	errs  map[int]error
	calls []int
}

func (f *fakeCameraFetcher) Cameras(_ context.Context, district int) ([]cwwp2.Camera, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, district)
	if err := f.errs[district]; err != nil {
		return nil, err
	}
	return f.cams[district], nil
}

func testCamera(id string, inService bool) cwwp2.Camera {
	return cwwp2.Camera{
		ID:        id,
		Name:      "cam " + id,
		Location:  cwwp2.Location{Latitude: 38, Longitude: -120.3, HasPosition: true},
		InService: inService,
		ImageURL:  "https://cwwp2.dot.ca.gov/data/" + id + ".jpg",
	}
}

func cameraIDs(cams []cwwp2.Camera) []string {
	ids := make([]string, 0, len(cams))
	for _, c := range cams {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestCameraService_UnavailableUntilFirstFetch(t *testing.T) {
	s := NewCameraService(&fakeCameraFetcher{}, []int{10}, 0)
	cams, status, last, version := s.Cameras()
	assert.Empty(t, cams)
	assert.Equal(t, "UNAVAILABLE", status)
	assert.True(t, last.IsZero())
	assert.NotEmpty(t, version)
	assert.Equal(t, DefaultCameraRefresh, s.refresh)
	assert.Equal(t, cameraRetryDelay, s.retry)
}

func TestCameraService_FiltersUnservableCameras(t *testing.T) {
	noPos := testCamera("d10-3", true)
	noPos.Location.HasPosition = false
	noImage := testCamera("d10-4", true)
	noImage.ImageURL = ""
	f := &fakeCameraFetcher{cams: map[int][]cwwp2.Camera{
		10: {testCamera("d10-1", true), testCamera("d10-2", false), noPos, noImage, testCamera("d10-5", true)},
	}}
	s := NewCameraService(f, []int{10}, time.Hour)
	require.True(t, s.Refresh(testCtx()))

	cams, status, last, _ := s.Cameras()
	assert.Equal(t, []string{"d10-1", "d10-5"}, cameraIDs(cams), "out of service, unplaceable and imageless cameras are dropped")
	assert.Equal(t, "OK", status)
	assert.True(t, last.IsZero(), "OK carries no freshness caveat")
}

func TestCameraService_DistrictOrderAndDedup(t *testing.T) {
	f := &fakeCameraFetcher{cams: map[int][]cwwp2.Camera{
		10: {testCamera("d10-2", true), testCamera("d10-1", true)},
		9:  {testCamera("d9-7", true), testCamera("d10-1", true)},
	}}
	s := NewCameraService(f, []int{10, 9}, time.Hour)
	require.True(t, s.Refresh(testCtx()))
	cams, _, _, _ := s.Cameras()
	assert.Equal(t, []string{"d10-2", "d10-1", "d9-7"}, cameraIDs(cams))
}

func TestCameraService_FailureServesLastGoodAsStale(t *testing.T) {
	f := &fakeCameraFetcher{cams: map[int][]cwwp2.Camera{10: {testCamera("d10-1", true)}}}
	s := NewCameraService(f, []int{10}, time.Hour)
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fetched }
	require.True(t, s.Refresh(testCtx()))
	_, _, _, okVersion := s.Cameras()

	f.errs = map[int]error{10: errors.New("HTTP 500")}
	s.now = func() time.Time { return fetched.Add(30 * time.Hour) }
	assert.False(t, s.Refresh(testCtx()))

	cams, status, last, version := s.Cameras()
	assert.Equal(t, []string{"d10-1"}, cameraIDs(cams), "a failed refresh never empties the list")
	assert.Equal(t, "STALE", status)
	assert.Equal(t, fetched, last, "lastSourceUpdate is when the served list was fetched")
	assert.NotEqual(t, okVersion, version, "the status change must change the ETag input")

	// Recovery clears the caveat.
	f.errs = nil
	assert.True(t, s.Refresh(testCtx()))
	_, status, last, _ = s.Cameras()
	assert.Equal(t, "OK", status)
	assert.True(t, last.IsZero())
}

func TestCameraService_PartialDistricts(t *testing.T) {
	f := &fakeCameraFetcher{
		cams: map[int][]cwwp2.Camera{10: {testCamera("d10-1", true)}},
		errs: map[int]error{9: errors.New("HTTP 500")},
	}
	s := NewCameraService(f, []int{10, 9}, time.Hour)
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fetched }
	assert.False(t, s.Refresh(testCtx()))

	cams, status, last, _ := s.Cameras()
	assert.Equal(t, []string{"d10-1"}, cameraIDs(cams))
	assert.Equal(t, "STALE", status, "a district that never loaded makes the list incomplete")
	assert.Equal(t, fetched, last)

	f.errs = map[int]error{9: errors.New("HTTP 500"), 10: errors.New("HTTP 500")}
	s2 := NewCameraService(f, []int{10, 9}, time.Hour)
	assert.False(t, s2.Refresh(testCtx()))
	cams, status, last, _ = s2.Cameras()
	assert.Empty(t, cams)
	assert.Equal(t, "UNAVAILABLE", status)
	assert.True(t, last.IsZero())
}

func TestCameraService_VersionTracksContent(t *testing.T) {
	f := &fakeCameraFetcher{cams: map[int][]cwwp2.Camera{10: {testCamera("d10-1", true)}}}
	s := NewCameraService(f, []int{10}, time.Hour)
	require.True(t, s.Refresh(testCtx()))
	_, _, _, v1 := s.Cameras()
	require.True(t, s.Refresh(testCtx()))
	_, _, _, v2 := s.Cameras()
	assert.Equal(t, v1, v2, "an unchanged list keeps its version")

	moved := testCamera("d10-1", true)
	moved.StreamURL = "https://wzmedia.dot.ca.gov/D10/x.stream/playlist.m3u8"
	f.cams[10] = []cwwp2.Camera{moved}
	require.True(t, s.Refresh(testCtx()))
	_, _, _, v3 := s.Cameras()
	assert.NotEqual(t, v1, v3)
}

func TestCameraService_RunRetriesSoonerAfterFailure(t *testing.T) {
	f := &fakeCameraFetcher{errs: map[int]error{10: errors.New("HTTP 500")}}
	s := NewCameraService(f, []int{10}, time.Hour)
	s.retry = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(testCtx())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.calls) >= 3
	}, time.Second, 5*time.Millisecond, "a failing district is retried on the short delay, not the hour")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
