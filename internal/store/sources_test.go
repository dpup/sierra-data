package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
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

func TestDeleteSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{
		{ID: "keep", Name: "Keep", PollInterval: time.Minute, Disappearance: DisappearanceResolve},
		{ID: "gone", Name: "Gone", PollInterval: time.Minute, Disappearance: DisappearanceResolve},
	}))

	require.NoError(t, s.DeleteSource(ctx, "gone"))
	srcs, err := s.ListSources(ctx)
	require.NoError(t, err)
	ids := make([]string, len(srcs))
	for i, src := range srcs {
		ids[i] = src.GetId()
	}
	assert.Contains(t, ids, "keep")
	assert.NotContains(t, ids, "gone")

	// Idempotent: deleting an absent id is a no-op, not an error.
	require.NoError(t, s.DeleteSource(ctx, "gone"))
}

// The Source proto has carried homepage_url since the /api/v1 migration, but
// nothing populated it until migrationV5 added the column — so every row served
// an empty link and the site's source-name anchor was dead code. Pin the whole
// round trip, including the re-seed path, since a config-owned field that does
// not update on re-seed is the same bug in slower motion.
func TestSeedSourcesRoundTripsHomepageURL(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{
		ID:           "pge",
		Name:         "PG&E (electric outages)",
		Attribution:  "Pacific Gas and Electric",
		HomepageURL:  "https://pgealerts.alerts.pge.com/outage-tools/outage-map/",
		PollInterval: 5 * time.Minute,
	}, {
		ID:           "psps",
		Name:         "PG&E (public safety power shutoffs)",
		Attribution:  "Pacific Gas and Electric",
		PollInterval: 5 * time.Minute,
	}}))

	assert.Equal(t, "https://pgealerts.alerts.pge.com/outage-tools/outage-map/",
		getSource(t, s, "pge").GetHomepageUrl())
	assert.Empty(t, getSource(t, s, "psps").GetHomepageUrl(),
		"an unset homepage stays empty rather than borrowing a sibling's")

	// Re-seed with a changed homepage: it is config-owned, so it must update.
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{
		ID:           "psps",
		Name:         "PG&E (public safety power shutoffs)",
		Attribution:  "Pacific Gas and Electric",
		HomepageURL:  "https://pgealerts.alerts.pge.com/psps-updates/",
		PollInterval: 5 * time.Minute,
	}}))
	assert.Equal(t, "https://pgealerts.alerts.pge.com/psps-updates/",
		getSource(t, s, "psps").GetHomepageUrl())
}
