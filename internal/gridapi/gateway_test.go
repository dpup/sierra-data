package gridapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// serveGateway runs g behind a real gRPC server and a gRPC-Gateway mux dialed
// over loopback, the way prefab serves /api/v1, and returns the HTTP base URL.
func serveGateway(t *testing.T, g *GridServer, dialOpts ...grpc.DialOption) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	gridv1.RegisterGridServiceServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := runtime.NewServeMux()
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOpts...)
	require.NoError(t, gridv1.RegisterGridServiceHandlerFromEndpoint(ctx, mux, lis.Addr().String(), opts))
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return hs.URL
}

func getJSON(t *testing.T, rawURL string, v any) int {
	t.Helper()
	resp, err := http.Get(rawURL)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode == http.StatusOK && v != nil {
		require.NoError(t, json.Unmarshal(body, v))
	}
	return resp.StatusCode
}

// The production 429 end to end: the gateway's loopback gRPC client refuses a
// message over its 4 MiB default receive limit. With the real page budget, a
// walk over ~7 MiB of perimeters gets every event in pages that each fit; an
// event that alone is over the old limit is served alone, which the raised
// receive limit (GatewayDialOptions) is for. Without those options the same
// lone event is the 429 production saw.
func TestGatewayServesEveryPageAndOversizedEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("builds ~12 MiB of geometry")
	}
	s := newTestService(t)
	g := NewGridServer(s) // the production PageByteBudget
	ctx := context.Background()

	// 12 resolved perimeters of ~600 KB each (the size of the largest PG&E
	// outage areas in production), plus one 5 MiB perimeter. They sit far
	// outside every place so the upsert's place matching stays cheap; the API
	// path under test never looks at where they are.
	want := []string{"calfire:megafire", "calfire:f1"}
	seed := func(id string, sev gridv1.Severity, vertices int, ago time.Duration) {
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: id, Layer: gridv1.Layer_WILDFIRE, Severity: sev, Status: gridv1.EventStatus_RESOLVED,
			Headline:   "Old fire " + id,
			Geometry:   polygonGeom(10.0, 10.0, vertices),
			ObservedAt: timestamppb.New(base.Add(-ago)),
			Provenance: &gridv1.Provenance{SourceId: "calfire"},
		})
		require.NoError(t, err)
	}
	seed("calfire:megafire", gridv1.Severity_EXTREME, 230000, 48*time.Hour)
	for i := range 12 {
		id := fmt.Sprintf("calfire:p%03d", i)
		seed(id, gridv1.Severity_SEVERE, 27000, time.Duration(i)*time.Minute)
		want = append(want, id)
	}

	baseURL := serveGateway(t, g, GatewayDialOptions()...)
	q := url.Values{"layer": {"WILDFIRE"}, "status": {"ACTIVE,SCHEDULED,RESOLVED,EXPIRED"}, "pageSize": {"200"}}
	var got []string
	pages := 0
	for {
		var page struct {
			Events []struct {
				ID string `json:"id"`
			} `json:"events"`
			NextPageToken string `json:"nextPageToken"`
		}
		code := getJSON(t, baseURL+"/api/v1/events?"+q.Encode(), &page)
		require.Equal(t, http.StatusOK, code, "page %d", pages)
		pages++
		for _, ev := range page.Events {
			got = append(got, ev.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}
	assert.Equal(t, want, got, "every event, in order, nothing repeated")
	assert.GreaterOrEqual(t, pages, 4, "megafire alone, then ~3 MiB pages of the rest")

	// The lone 5 MiB event, without and with the raised receive limit.
	plain := serveGateway(t, g)
	assert.Equal(t, http.StatusTooManyRequests, getJSON(t, plain+"/api/v1/events/calfire:megafire", nil),
		"prefab's default dial options: the 4 MiB gRPC client limit (RESOURCE_EXHAUSTED)")
	var ev map[string]any
	assert.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/events/calfire:megafire", &ev))
	assert.Equal(t, "calfire:megafire", ev["id"])
}
