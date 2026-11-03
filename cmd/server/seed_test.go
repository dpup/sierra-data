package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/store"
)

func TestSeedPathsDropsBrokerFallbackObserver(t *testing.T) {
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	got := seedPaths([]store.MeshObservation{
		{PubKey: "s1", HeardAt: at, Broker: "wss://b", Gateway: "abcd", Path: []string{"2c"}},
		{PubKey: "s2", HeardAt: at, Broker: "wss://b", Gateway: "wss://b", Path: []string{"fe"}},
	})
	assert.Equal(t, []meshcore.SeedPath{
		{Hops: []string{"2c"}, Observer: "abcd", Sender: "s1", At: at},
		{Hops: []string{"fe"}, Sender: "s2", At: at},
	}, got)
}
