package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	_ "time/tzdata" // the Pacific clock must not depend on the runner's zoneinfo
)

// pacific is the districts' clock: "today" and the morning cutoff are theirs,
// and computing them here keeps the schedule DST-proof (the cron is UTC).
var pacific = mustLoadLocation("America/Los_Angeles")

// dayStart is when today's reading may first be taken: today's date in Pacific
// at the given "HH:MM". A reading observed before it is yesterday's answer (or
// a pre-dawn read of a recording the district has not updated yet).
func dayStart(now time.Time, hhmm string) (time.Time, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad -not-before %q (want HH:MM): %w", hhmm, err)
	}
	p := now.In(pacific)
	return time.Date(p.Year(), p.Month(), p.Day(), t.Hour(), t.Minute(), 0, 0, pacific), nil
}

// lineObservations fetches each burn line's last observedAt from the public
// events API on the same host as the ingest endpoint.
//
// This is what makes a scheduled run idempotent: GitHub fires the cron hourly
// (its schedules land hours late, so one slot cannot hit the morning), and each
// run dials only the lines that have no reading since this morning. The server
// is the record of what was read, so there is no CI-side state to drift.
func lineObservations(ctx context.Context, endpoint string) (map[string]time.Time, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = "/api/v1/events"
	u.RawQuery = url.Values{"layer": {"burn_status"}, "page_size": {"100"}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}

	var body struct {
		Events []struct {
			BurnStatus struct {
				BurnLines []struct {
					ID          string `json:"id"`
					Observation *struct {
						ObservedAt time.Time `json:"observedAt"`
					} `json:"observation"`
				} `json:"burnLines"`
			} `json:"burnStatus"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode %s: %w", u, err)
	}

	// A line speaking for several counties appears on each county's event with
	// the same observation; keep the latest in case they ever disagree.
	seen := map[string]time.Time{}
	for _, ev := range body.Events {
		for _, l := range ev.BurnStatus.BurnLines {
			if l.Observation == nil || l.Observation.ObservedAt.IsZero() {
				continue
			}
			id := strings.ToLower(l.ID)
			if l.Observation.ObservedAt.After(seen[id]) {
				seen[id] = l.Observation.ObservedAt
			}
		}
	}
	return seen, nil
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}
