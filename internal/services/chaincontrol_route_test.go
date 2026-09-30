package services

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/lib/geo"
	"github.com/dpup/sierra-data/internal/lib/routing"
)

// A CWWP2-shaped control ("Highway 4", checkpoint index as MessageID) matches
// the Hwy 4 route; an unrecognized status at the same spot never does — it is
// not a requirement, and finding it would flip the road to chains REQUIRED.
func TestFindChainControlForRoute_CWWP2Shapes(t *testing.T) {
	s := &RoadsService{geoUtils: geo.NewGeoUtils()}
	route := routing.Route{
		Name: "Hwy 4",
		Polyline: geo.Polyline{Points: []geo.Point{
			{Latitude: 38.2555, Longitude: -120.3505}, // Arnold
			{Latitude: 38.4605, Longitude: -120.0448}, // Bear Valley
		}},
	}
	at := &api.Coordinates{Latitude: 38.2560, Longitude: -120.3500}

	unreadable := caltrans.ChainControlData{Highway: "Highway 4", Coordinates: at, Unrecognized: true, RawStatus: "-120.35"}
	assert.Nil(t, s.findChainControlForRoute(context.Background(), route, []caltrans.ChainControlData{unreadable}))

	r2 := caltrans.ChainControlData{
		Highway: "Highway 4", Direction: "Eastbound", Level: "R2", LocationName: "ARNOLD",
		Coordinates: at, EffectiveTime: "2026-12-24T18:05:00-08:00", MessageID: "10-CAL-4-44.1-E-1E",
	}
	info := s.findChainControlForRoute(context.Background(), route, []caltrans.ChainControlData{unreadable, r2})
	require.NotNil(t, info)
	assert.Equal(t, api.ChainControlLevel_CHAIN_CONTROL_LEVEL_R2, info.Level)
	assert.Equal(t, "ARNOLD", info.LocationName)
	require.NotNil(t, info.EffectiveTime)
	assert.Equal(t, int64(1798164300), info.EffectiveTime.Seconds) // 18:05 PST = 02:05Z Dec 25
}
