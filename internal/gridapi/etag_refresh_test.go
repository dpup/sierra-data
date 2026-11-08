package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/dpup/prefab/plugins/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// rpcTag runs an RPC and returns the ETag its guard set.
func rpcTag(t *testing.T, call func(ctx context.Context) error) string {
	t.Helper()
	ctx, fs := rpcCtx()
	require.NoError(t, call(ctx))
	tags := fs.hdr.Get("grpc-metadata-etag")
	require.Len(t, tags, 1)
	return tags[0]
}

// rpcNotModified reports whether the RPC answers If-None-Match: tag with a 304.
func rpcNotModified(t *testing.T, tag string, call func(ctx context.Context) error) bool {
	t.Helper()
	ctx, _ := rpcCtx()
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("if-none-match", tag))
	err := call(ctx)
	if err == nil {
		return false
	}
	require.ErrorIs(t, err, etag.ErrNotModified)
	return true
}

func listLayer(g *GridServer, layer string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{Layer: []string{layer}})
		return err
	}
}

func listAll(g *GridServer) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := g.ListEvents(ctx, &gridv1.ListEventsRequest{})
		return err
	}
}

func getEvent(g *GridServer, id string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := g.GetEvent(ctx, &gridv1.GetEventRequest{Id: id})
		return err
	}
}

// The store persists hash-excluded fields (AI enhancement, mesh telemetry,
// burn observations, place_ids) by rewriting the event's blob WITHOUT a new
// revision. The served body changes, so the list and detail validators must
// change too — keyed on revision/DataVersion alone they answered a
// conditional GET with a 304 for the old body.
func TestValidatorsFollowInPlaceRewrites(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		layer string
		seed  func(t *testing.T, s *Service) (first, refreshed *gridv1.Event)
	}{
		{
			name:  "enhancement",
			layer: "road_incident",
			seed: func(t *testing.T, s *Service) (*gridv1.Event, *gridv1.Event) {
				ev := &gridv1.Event{
					Id: "chp:live", Layer: gridv1.Layer_ROAD_INCIDENT,
					Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE,
					Headline:    "Vehicle off roadway",
					Geometry:    pointGeom(38.05, -120.55),
					ObservedAt:  timestamppb.New(base),
					Provenance:  &gridv1.Provenance{SourceId: "chp"},
					Summary:     "A car is off the road.",
					Enhancement: &gridv1.Enhancement{Model: "m", Fields: []string{"summary"}},
				}
				next := proto.Clone(ev).(*gridv1.Event)
				next.Summary = "A car is off the road; one lane blocked."
				next.Enhancement.EnhancedAt = timestamppb.New(base.Add(time.Minute))
				return ev, next
			},
		},
		{
			name:  "mesh telemetry",
			layer: "mesh",
			seed: func(t *testing.T, s *Service) (*gridv1.Event, *gridv1.Event) {
				seedMeshNode(t, s, "a2d649457de4481a")
				ev, err := s.Store.GetEvent(ctx, "meshcore:a2d649457de4481a")
				require.NoError(t, err)
				next := proto.Clone(ev).(*gridv1.Event)
				next.GetMesh().Telemetry = &gridv1.MeshTelemetry{
					Gateways: []string{"gw"},
					Admin:    &gridv1.MeshAdminTelemetry{BatteryVolts: wrapperspb.Double(3.91)},
				}
				return nil, next // already stored
			},
		},
		{
			name:  "burn observation",
			layer: "burn_status",
			seed: func(t *testing.T, s *Service) (*gridv1.Event, *gridv1.Event) {
				require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{
					{ID: "burnline", Name: "County burn line", PollInterval: time.Hour},
				}))
				mk := func(msg string, at time.Time) *gridv1.Event {
					return &gridv1.Event{
						Id: "burn:calaveras-county", Layer: gridv1.Layer_BURN_STATUS,
						Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE,
						Headline:   "Calaveras County — no-burn day",
						PlaceIds:   []string{"area:calaveras"},
						Provenance: &gridv1.Provenance{SourceId: "burnline", FetchedAt: timestamppb.New(at)},
						ObservedAt: timestamppb.New(at),
						Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{
							BurnDay:    gridv1.BurnDay_BURN_DAY_NO,
							Permission: gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED,
							BurnLines: []*gridv1.BurnLineReading{{
								Id: "calaveras-apcd", BurnDay: gridv1.BurnDay_BURN_DAY_NO,
								Observation: &gridv1.BurnObservation{Message: msg, ObservedAt: timestamppb.New(at)},
							}},
						}},
					}
				}
				return mk("Today, October 9th, is not a burn day", base),
					mk("Today, October 10th, is not a burn day", base.Add(24*time.Hour))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t)
			g := NewGridServer(s)
			first, refreshed := tc.seed(t, s)
			if first != nil {
				_, err := s.Store.UpsertEvent(ctx, first)
				require.NoError(t, err)
			}
			id := refreshed.GetId()
			list, all, detail := listLayer(g, tc.layer), listAll(g), getEvent(g, id)
			quakes, quake := listLayer(g, "earthquake"), getEvent(g, "usgs:q1")

			listTag, allTag, detailTag := rpcTag(t, list), rpcTag(t, all), rpcTag(t, detail)
			quakesTag, quakeTag := rpcTag(t, quakes), rpcTag(t, quake)
			require.True(t, rpcNotModified(t, listTag, list))
			require.True(t, rpcNotModified(t, detailTag, detail))

			// An idle hash-equal re-upsert rewrites nothing: every 304 holds.
			_, err := s.Store.UpsertEvent(ctx, mustGet(t, s, id))
			require.NoError(t, err)
			require.True(t, rpcNotModified(t, listTag, list), "a no-op poll must keep the list 304")
			require.True(t, rpcNotModified(t, detailTag, detail), "a no-op poll must keep the detail 304")

			res, err := s.Store.UpsertEvent(ctx, refreshed)
			require.NoError(t, err)
			require.False(t, res.Changed, "the refresh is hash-equal: no new revision")

			assert.False(t, rpcNotModified(t, detailTag, detail), "event detail revalidates after an in-place rewrite")
			assert.False(t, rpcNotModified(t, listTag, list), "a list of the layer revalidates after an in-place rewrite")
			assert.False(t, rpcNotModified(t, allTag, all), "an unfiltered list revalidates after an in-place rewrite")
			assert.True(t, rpcNotModified(t, quakesTag, quakes), "a list of another layer keeps its 304")
			assert.True(t, rpcNotModified(t, quakeTag, quake), "another event's detail keeps its 304")
		})
	}
}

func mustGet(t *testing.T, s *Service, id string) *gridv1.Event {
	t.Helper()
	ev, err := s.Store.GetEvent(context.Background(), id)
	require.NoError(t, err)
	return ev
}
