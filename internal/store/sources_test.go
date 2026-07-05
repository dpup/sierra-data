package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
)

func getSource(t *testing.T, s *Store, id string) *gridv1.Source {
	t.Helper()
	srcs, err := s.ListSources(context.Background())
	require.NoError(t, err)
	for _, src := range srcs {
		if src.GetId() == id {
			return src
		}
	}
	t.Fatalf("source %q not found", id)
	return nil
}

func TestSeedSourcesPreservesRuntimeFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{
		ID:            "nws",
		Name:          "National Weather Service",
		Attribution:   "NWS",
		PollInterval:  5 * time.Minute,
		ExpireAfter:   time.Hour,
		Disappearance: DisappearanceExpire,
	}}))
	require.NoError(t, s.RecordAttempt(ctx, "nws", nil))

	// Config change re-seeds; health fields must survive.
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{
		ID:            "nws",
		Name:          "NWS (renamed)",
		Attribution:   "NWS",
		PollInterval:  10 * time.Minute,
		StaleAfter:    time.Hour,
		Disappearance: DisappearanceExpire,
	}}))

	src := getSource(t, s, "nws")
	assert.Equal(t, "NWS (renamed)", src.GetName())
	assert.Equal(t, uint32(600), src.GetPollIntervalSeconds())
	assert.Equal(t, uint32(3600), src.GetStaleAfterSeconds())
	assert.Equal(t, uint32(0), src.GetExpireAfterSeconds(), "cleared override returns to never")
	assert.Equal(t, gridv1.SourceStatus_OK, src.GetStatus())
	assert.NotNil(t, src.GetLastSuccessAt(), "runtime fields preserved across seeding")

	var disappearance string
	require.NoError(t, s.db.QueryRow(
		`SELECT disappearance FROM sources WHERE id = 'nws'`).Scan(&disappearance))
	assert.Equal(t, DisappearanceExpire, disappearance)
}

func TestSeedSourcesDefaults(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{
		ID: "usgs", Name: "USGS", PollInterval: time.Minute,
	}}))

	src := getSource(t, s, "usgs")
	assert.Equal(t, uint32(180), src.GetStaleAfterSeconds(), "default stale_after = 3x poll interval")
	assert.Equal(t, gridv1.SourceStatus_SOURCE_STATUS_UNSPECIFIED, src.GetStatus(), "no attempts yet")

	var disappearance string
	require.NoError(t, s.db.QueryRow(
		`SELECT disappearance FROM sources WHERE id = 'usgs'`).Scan(&disappearance))
	assert.Equal(t, DisappearanceResolve, disappearance)
}

func TestRecordAttemptStatusLadder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{
		ID: "usgs", Name: "USGS", PollInterval: time.Minute, // stale window = 180s
	}}))

	// Failure before any success: straight to UNAVAILABLE.
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("connect timeout")))
	src := getSource(t, s, "usgs")
	assert.Equal(t, gridv1.SourceStatus_UNAVAILABLE, src.GetStatus())
	assert.Equal(t, "connect timeout", src.GetLastError())
	assert.Nil(t, src.GetLastSuccessAt())
	assert.NotNil(t, src.GetLastAttemptAt())

	// Success: OK, error cleared, success stamped.
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	src = getSource(t, s, "usgs")
	assert.Equal(t, gridv1.SourceStatus_OK, src.GetStatus())
	assert.Empty(t, src.GetLastError())
	assert.NotNil(t, src.GetLastSuccessAt())

	// Failure with a recent success: STALE (last-good is still served).
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")))
	src = getSource(t, s, "usgs")
	assert.Equal(t, gridv1.SourceStatus_STALE, src.GetStatus())
	assert.Equal(t, "HTTP 503", src.GetLastError())

	// Success gone stale beyond the window (3x poll = 180s): UNAVAILABLE.
	_, err := s.db.Exec(`UPDATE sources SET last_success_at = ? WHERE id = 'usgs'`,
		time.Now().Add(-10*time.Minute).Unix())
	require.NoError(t, err)
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")))
	src = getSource(t, s, "usgs")
	assert.Equal(t, gridv1.SourceStatus_UNAVAILABLE, src.GetStatus())

	// Recovery: back to OK.
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	src = getSource(t, s, "usgs")
	assert.Equal(t, gridv1.SourceStatus_OK, src.GetStatus())
	assert.Empty(t, src.GetLastError())
}

func TestRecordAttemptUnseededSource(t *testing.T) {
	s := newTestStore(t)
	err := s.RecordAttempt(context.Background(), "ghost", nil)
	assert.Error(t, err)
}
