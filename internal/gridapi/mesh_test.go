package gridapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/store"
)

// meshLinksAt issues GET /api/v1/mesh/links?window=… against the hand-built
// handler and decodes the response.
func meshLinksAt(t *testing.T, s *Service, window string) meshLinksResponse {
	t.Helper()
	u := "/api/v1/mesh/links"
	if window != "" {
		u += "?window=" + window
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	rec := httptest.NewRecorder()
	s.serveMeshLinks(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var out meshLinksResponse
	decode(t, rec, &out)
	return out
}

func TestServeMeshLinks(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	// Two receptions of node aa relayed via bb, ~2 days ago; compact into the
	// rollup so the endpoint reads real topology. base is the service clock "now".
	heard := base.Add(-48 * time.Hour)
	require.NoError(t, s.Store.InsertMeshObservations(ctx, []store.MeshObservation{
		{PubKey: "aa", HeardAt: heard, SNR: -6, PathNodes: []string{"bb"}},
		{PubKey: "aa", HeardAt: heard.Add(time.Minute), SNR: -4, PathNodes: []string{"bb"}},
	}))
	_, err := s.Store.CompactMeshObservations(ctx)
	require.NoError(t, err)

	// Default window (72h) covers the receptions → one weighted link.
	resp := meshLinksAt(t, s, "")
	assert.Equal(t, "72h", resp.Window)
	require.Len(t, resp.Links, 1)
	l := resp.Links[0]
	assert.Equal(t, "aa", l.A)
	assert.Equal(t, "bb", l.B)
	assert.Equal(t, 2, l.Observations)
	assert.Equal(t, 1, l.DaysActive)
	assert.InDelta(t, -4, l.BestSnr, 1e-9)
	assert.NotEmpty(t, l.LastSeen)

	// A window whose day-bucket floor is after the (2-day-old) rollup bucket
	// excludes it — still 200 + []. (Tier 1 is day-grained; sub-day precision
	// lives only in the raw tail.)
	resp = meshLinksAt(t, s, "1h")
	assert.Equal(t, "1h", resp.Window)
	assert.Empty(t, resp.Links)

	// A garbage window falls back to the default rather than erroring.
	assert.Equal(t, "72h", meshLinksAt(t, s, "not-a-duration").Window)
}
