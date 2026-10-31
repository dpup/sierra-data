package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDayStartIsPacificAcrossDST(t *testing.T) {
	// 2026-10-29 15:00 UTC is 08:00 PDT; 2026-11-02 15:00 UTC is 07:00 PST.
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC), "2026-10-29T13:30:00Z"},
		{time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC), "2026-11-02T14:30:00Z"},
		// 03:00 UTC on the 30th is still the 29th in Pacific.
		{time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC), "2026-10-29T13:30:00Z"},
	} {
		got, err := dayStart(tc.now, "06:30")
		require.NoError(t, err)
		assert.Equal(t, tc.want, got.UTC().Format(time.RFC3339), "now=%s", tc.now)
	}

	_, err := dayStart(time.Now(), "6.30am")
	assert.Error(t, err)
}

func TestLineObservations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/events", r.URL.Path)
		assert.Equal(t, "burn_status", r.URL.Query().Get("layer"))
		_, _ = w.Write([]byte(`{"events":[
		  {"burnStatus":{"burnLines":[
		    {"id":"calaveras-apcd","observation":{"observedAt":"2026-10-28T19:41:38Z"}},
		    {"id":"shared-line","observation":{"observedAt":"2026-10-29T14:00:00Z"}}]}},
		  {"burnStatus":{"burnLines":[
		    {"id":"never-read","observation":null},
		    {"id":"shared-line","observation":{"observedAt":"2026-10-29T15:00:00Z"}}]}}]}`))
	}))
	defer srv.Close()

	got, err := lineObservations(context.Background(), srv.URL+"/api/v1/ingest/burn.line")
	require.NoError(t, err)
	assert.Equal(t, map[string]time.Time{
		"calaveras-apcd": time.Date(2026, 10, 28, 19, 41, 38, 0, time.UTC),
		"shared-line":    time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC),
	}, got)
}

func TestLineObservationsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := lineObservations(context.Background(), srv.URL+"/api/v1/ingest/burn.line")
	assert.Error(t, err)
}
