package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// seedRelatedFire adds a fire beside the presence fixtures' evacuation zone
// (evac:CAL-E-1 at 38.1,-120.4), named in its notes.
func seedRelatedFire(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{{ID: "calfire", Name: "CAL FIRE", PollInterval: time.Minute}}))
	upsert(t, s.Store, &gridv1.Event{
		Id: "calfire:presence", Layer: gridv1.Layer_WILDFIRE, Category: "wildfire",
		Severity: gridv1.Severity_SEVERE, Status: gridv1.EventStatus_ACTIVE,
		Headline:  "Presence Fire — 40 ac, 0% contained",
		Geometry:  pointGeom(38.1, -120.41),
		Effective: timestamppb.New(base.Add(-time.Hour)), ObservedAt: timestamppb.New(base.Add(-time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "calfire", SourceName: "CAL FIRE"},
		Detail:     &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{Acres: 40}},
	})
}

func TestListRelatedEvents(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	seedRelatedFire(t, s)
	g := NewGridServer(s)

	res, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "calfire:presence"})
	require.NoError(t, err)
	var evac *gridv1.RelatedEvent
	for _, r := range res.GetRelated() {
		if r.GetEvent().GetId() == "evac:CAL-E-1" {
			evac = r
		}
	}
	require.NotNil(t, evac, "the zone 0.9 km away, issued an hour after the fire started")
	assert.Equal(t, "fire-evacuation-near", evac.GetRule())
	assert.Equal(t, gridv1.RelationConfidence_RELATION_MEDIUM, evac.GetConfidence())
	assert.NotEmpty(t, evac.GetReason())
	assert.Empty(t, evac.GetEvent().GetGeometry().GetGeojson(), "related events carry no shapes")

	back, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "evac:CAL-E-1"})
	require.NoError(t, err)
	require.NotEmpty(t, back.GetRelated())
	assert.Equal(t, "calfire:presence", back.GetRelated()[0].GetEvent().GetId())

	_, err = g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "nope:1"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}
