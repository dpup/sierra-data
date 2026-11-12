package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// TransitionEvents is batched (one read and four writes per chunk of events,
// not five statements per event); this pins what the per-event loop did.
func TestTransitionEventsBatch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "chp")
	n := transitionChunk*2 + 7 // three chunks
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("chp:%03d", i)
		ev := testEvent(ids[i], gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "incident")
		ev.Provenance.SourceId = "chp"
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	// Already resolved ahead of the sweep: skipped, no new revision.
	require.NoError(t, s.TransitionEvents(ctx, ids[:1], gridv1.EventStatus_RESOLVED, baseTime))
	v0, err := s.DataVersion(ctx)
	require.NoError(t, err)

	at := baseTime.Add(2 * time.Hour)
	call := append(append([]string{"chp:missing", ids[3]}, ids...), ids[5], "chp:missing") // repeats and unknown ids
	require.NoError(t, s.TransitionEvents(ctx, call, gridv1.EventStatus_RESOLVED, at))

	for i, id := range ids {
		got, err := s.GetEvent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, gridv1.EventStatus_RESOLVED, got.GetStatus(), id)
		assert.Equal(t, uint32(2), got.GetRevision(), "%s: one transition, one revision", id)
		assert.Equal(t, 2, revisionCount(t, s, id), id)
		if i > 0 {
			assert.Equal(t, at.Unix(), got.GetObservedAt().AsTime().Unix())
			// The row's columns and the hash follow the blob: a reappearance with the
			// ACTIVE content writes a revision.
			var status int32
			var rev int64
			var hash string
			require.NoError(t, s.db.QueryRow(`SELECT status, revision, content_hash FROM events WHERE id = $1`, id).Scan(&status, &rev, &hash))
			assert.EqualValues(t, gridv1.EventStatus_RESOLVED, status)
			assert.EqualValues(t, 2, rev)
			assert.Equal(t, ContentHash(got), hash)
		}
	}
	// The lite copy moved with the blob.
	var lite int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM event_lite WHERE event_id = ANY($1)`, ids).Scan(&lite))
	assert.Equal(t, n, lite)
	// 'revisions' moved by exactly the revisions written (the first id was already resolved).
	v1, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, n-1, v1-v0)

	active, err := s.ActiveEventsBySource(ctx, "chp")
	require.NoError(t, err)
	assert.Empty(t, active)

	// A second sweep is a no-op.
	require.NoError(t, s.TransitionEvents(ctx, ids, gridv1.EventStatus_RESOLVED, at.Add(time.Hour)))
	v2, err := s.DataVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, v1, v2)
}
