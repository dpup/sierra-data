package gridapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/store"
)

// page_size follows the proto comment exactly: 0 = the default, above the
// ceiling is clamped (a full page, not an error), and a negative size, which
// has no meaning and used to be read as 0, is a 400.
func TestPageSizeContract(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	ctx := context.Background()
	const n = store.MaxPageSize + 5
	for i := range n {
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: fmt.Sprintf("usgs:q%03d", i), Layer: gridv1.Layer_EARTHQUAKE,
			Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE,
			Headline:   "M2.0",
			Geometry:   pointGeom(38.2, -120.4),
			ObservedAt: timestamppb.New(base),
			Provenance: &gridv1.Provenance{SourceId: "usgs"},
		})
		require.NoError(t, err)
	}

	list := func(size int32) *gridv1.EventList {
		t.Helper()
		out, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{PageSize: size})
		require.NoError(t, err, "page_size=%d", size)
		return out
	}
	assert.Len(t, list(0).GetEvents(), store.DefaultPageSize, "0 is the default")
	assert.Len(t, list(7).GetEvents(), 7)
	big := list(10000)
	assert.Len(t, big.GetEvents(), store.MaxPageSize, "above the ceiling is clamped, not refused")
	assert.NotEmpty(t, big.GetNextPageToken(), "and the rest is a page away")

	for name, call := range map[string]func() error{
		"ListEvents": func() error {
			_, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{PageSize: -1})
			return err
		},
		"GetEventHistory": func() error {
			_, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "usgs:q000", PageSize: -1})
			return err
		},
		"ListHistory": func() error {
			_, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{PageSize: -1})
			return err
		},
	} {
		err := call()
		require.Error(t, err, name)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
		assert.Contains(t, status.Convert(err).Message(), "page_size", name)
	}
}
