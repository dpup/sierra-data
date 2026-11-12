package gridapi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpup/prefab"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// The default (no geometryFormat) JSON of every Geometry-carrying RPC must stay
// byte-for-byte what it was before the OBJECT form existed: external
// consumers (ersn.net and others) decode the base64 `geojson` string and
// nothing else. testdata/geometry_default.golden.json was captured from the
// code BEFORE the change; regenerate it only for a deliberate, changelogged
// response change (UPDATE_GOLDEN=1 go test ./internal/gridapi -run TestDefaultGeometryJSONIsUnchanged).
func TestDefaultGeometryJSONIsUnchanged(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	ctx := context.Background()

	// A geometry with no GeoJSON (a shadow mesh repeater: a centroid only), the
	// case most likely to change shape if the field gained presence.
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{{ID: "meshcore", Name: "MeshCore", PollInterval: time.Minute}}))
	_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
		Id: "meshcore:aa00", Layer: gridv1.Layer_MESH, Severity: gridv1.Severity_INFO,
		Status: gridv1.EventStatus_ACTIVE, Headline: "Shadow node",
		Geometry:   &gridv1.Geometry{Centroid: &gridv1.LatLng{Lat: 38.2, Lng: -120.4}},
		ObservedAt: timestamppb.New(base),
		Provenance: &gridv1.Provenance{SourceId: "meshcore"},
	})
	require.NoError(t, err)

	all := []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}
	list, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Status: all})
	require.NoError(t, err)
	one, err := g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "usgs:q1"})
	require.NoError(t, err)
	hist, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "usgs:q1"})
	require.NoError(t, err)
	arch, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{})
	require.NoError(t, err)
	town, err := g.GetPlace(rpcBG(), &gridv1.GetPlaceRequest{Place: "arnold"})
	require.NoError(t, err)
	resolved, err := g.ResolvePlace(rpcBG(), &gridv1.ResolvePlaceRequest{Lat: "38.25", Lng: "-120.35"})
	require.NoError(t, err)

	// protojson deliberately varies its whitespace from build to build (so no
	// one depends on its exact bytes), so each body is re-indented with
	// encoding/json before comparing: the golden pins keys, order and values.
	var out []byte
	for _, m := range []proto.Message{list, one, hist, arch, town, resolved} {
		scrubIngestTimes(m)
		b, err := prefab.JSONMarshalOptions.Marshal(m)
		require.NoError(t, err)
		var norm bytes.Buffer
		require.NoError(t, json.Indent(&norm, compactJSON(t, b), "", "  "))
		out = append(append(out, norm.Bytes()...), '\n')
	}

	path := filepath.Join("testdata", "geometry_default.golden.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, out, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(out))
}

// scrubIngestTimes zeroes the wall-clock stamps the store writes at upsert
// (ingestedAt), the only part of these bodies that differs run to run.
func scrubIngestTimes(m proto.Message) {
	scrubEvent := func(ev *gridv1.Event) {
		if ev != nil {
			ev.IngestedAt = nil
		}
	}
	switch v := m.(type) {
	case *gridv1.EventList:
		for _, ev := range v.GetEvents() {
			scrubEvent(ev)
		}
	case *gridv1.Event:
		scrubEvent(v)
	case *gridv1.EventRevisionList:
		for _, r := range v.GetRevisions() {
			r.IngestedAt = nil
			scrubEvent(r.GetEvent())
		}
	}
}

func compactJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var c bytes.Buffer
	require.NoError(t, json.Compact(&c, b))
	return c.Bytes()
}
