package gridapi

import (
	"testing"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetStats(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return clock }
	g := NewGridServer(s)

	all, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Positive(t, all.GetEvents())
	assert.GreaterOrEqual(t, all.GetRevisions(), all.GetEvents())
	assert.Positive(t, all.GetSources())
	require.NotNil(t, all.GetSince())
	var sum int32
	for _, l := range all.GetLayers() {
		sum += l.GetEvents()
	}
	assert.Equal(t, all.GetEvents(), sum, "layers add up to the total")

	// Cached: a new event inside the TTL does not move the count.
	upsert(t, s.Store, &gridv1.Event{
		Id: "caloes:extra", Layer: gridv1.Layer_EVACUATION, Severity: gridv1.Severity_MINOR,
		Status: gridv1.EventStatus_ACTIVE, Headline: "Warning",
		Provenance: &gridv1.Provenance{SourceId: "caloes", SourceName: "Cal OES"},
	})
	again, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Equal(t, all.GetEvents(), again.GetEvents())
	clock = clock.Add(statsTTL)
	fresh, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.Equal(t, all.GetEvents()+1, fresh.GetEvents())

	_, err = g.GetStats(rpcBG(), &gridv1.GetStatsRequest{Place: "nowhere"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}
