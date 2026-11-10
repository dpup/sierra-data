package main

// Store-backed wiring for background readers: road sampling and the
// wildfire poller's ended-incident lookup.

import (
	"strings"

	"context"
	"errors"
	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"time"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/ingest"
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Road samples follow the roads refresh (15 min); a year of them is ~35k rows
// per segment.
const (
	roadSampleInterval  = 15 * time.Minute
	roadSampleRetention = 400 * 24 * time.Hour
)

// roadSamplingConfig samples the roads service's CURRENT set, which ListRoads
// serves from its cache (it refreshes upstream only when the cache is empty;
// the periodic refresher keeps it warm). Each sample is stamped with that
// set's refresh time, so re-reading an unchanged set re-offers the same rows
// and the store ignores them.
func roadSamplingConfig(roads *services.RoadsService) ingest.RoadSampling {
	if roads == nil {
		return ingest.RoadSampling{}
	}
	return ingest.RoadSampling{
		Interval:  roadSampleInterval,
		Retention: roadSampleRetention,
		Sample: func(ctx context.Context) ([]store.RoadSample, error) {
			resp, err := roads.ListRoads(ctx, &api.ListRoadsRequest{})
			if err != nil {
				return nil, err
			}
			return roadSamples(resp)
		},
	}
}

func roadSamples(resp *api.ListRoadsResponse) ([]store.RoadSample, error) {
	if resp.GetLastUpdated() == nil {
		return nil, errors.New("roads: no refresh time")
	}
	at := resp.GetLastUpdated().AsTime()
	var out []store.RoadSample
	for _, r := range resp.GetRoads() {
		s := store.RoadSample{RoadID: r.GetId(), At: at, Status: int32(r.GetStatus()), Congestion: int32(r.GetCongestionLevel())}
		// A zero travel time is Google Routes not answering for this road,
		// not a zero-minute drive.
		if d := r.GetDurationMinutes(); d > 0 {
			s.DurationMinutes = &d
			delay := r.GetDelayMinutes()
			s.DelayMinutes = &delay
		}
		out = append(out, s)
	}
	return out, nil
}

// endedFires lists CAL FIRE incidents that ended since a time, for the
// wildfire poller's orphaned-perimeter check. Lite reads: no perimeters.
func endedFires(st *store.Store) ingest.EndedFires {
	return func(ctx context.Context, since time.Time) ([]*gridv1.Event, error) {
		var out []*gridv1.Event
		token := ""
		for {
			page, err := st.QueryEventsPage(ctx, store.EventQuery{
				Layers:   []gridv1.Layer{gridv1.Layer_WILDFIRE},
				Statuses: []gridv1.EventStatus{gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED},
				Since:    since, PageSize: store.MaxPageSize, PageToken: token, Lite: true,
			})
			if err != nil {
				return nil, err
			}
			for _, ev := range page.Items {
				if strings.HasPrefix(ev.GetId(), "calfire:") {
					out = append(out, ev)
				}
			}
			if token = page.Next(len(page.Items)); token == "" {
				return out, nil
			}
		}
	}
}
