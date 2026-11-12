package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/places"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// Two processes serving one database (the writer and a reader, or two
// readers) must agree on every validator, and a reader must stop answering
// 304 for a list once the writer has changed it: validators come from the
// versions table and the build, never from per-process state.
func TestValidatorsAgreeAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	w, err := store.Open(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	cfg := testConfig()
	require.NoError(t, places.Seed(ctx, w, cfg))
	seedEvents(t, w)

	// The reader's backstop poll is short and LISTEN is off, so this pins the
	// worst case: staleness bounded by the poll.
	poll := 300 * time.Millisecond
	r, err := store.OpenReader(url, store.WithVersionPoll(poll), store.WithoutVersionListen())
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	gw := NewGridServer(NewService(w, &fakeWeather{resp: weatherResp()}, &fakeCensus{}, cfg, nil))
	gr := NewGridServer(NewService(r, &fakeWeather{resp: weatherResp()}, &fakeCensus{}, cfg, nil))

	listPlaces := func(g *GridServer) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := g.ListPlaces(ctx, &gridv1.ListPlacesRequest{})
			return err
		}
	}
	for name, call := range map[string]func(*GridServer) func(context.Context) error{
		"list":   listAll,
		"places": listPlaces,
		"event":  func(g *GridServer) func(context.Context) error { return getEvent(g, "usgs:q1") },
	} {
		assert.Equal(t, rpcTag(t, call(gw)), rpcTag(t, call(gr)), "%s: same data, same validator", name)
	}

	tag := rpcTag(t, listAll(gr))
	require.True(t, rpcNotModified(t, tag, listAll(gr)))
	_, err = w.UpsertEvent(ctx, &gridv1.Event{Id: "usgs:new", Layer: gridv1.Layer_EARTHQUAKE,
		Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE, Headline: "new",
		Provenance: &gridv1.Provenance{SourceId: "usgs"}})
	require.NoError(t, err)
	assert.False(t, rpcNotModified(t, tag, listAll(gw)), "the writer sees its own write at once")
	start := time.Now()
	require.Eventually(t, func() bool { return !rpcNotModified(t, tag, listAll(gr)) },
		3*time.Second, 10*time.Millisecond)
	assert.Less(t, time.Since(start), poll+200*time.Millisecond, "a reader's stale 304 ends within the poll")
	assert.Equal(t, rpcTag(t, listAll(gw)), rpcTag(t, listAll(gr)))
}
