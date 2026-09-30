// Package cwwp2 reads Caltrans's CWWP2 data portal (cwwp2.dot.ca.gov), the
// structured per-district feeds that QuickMap's KML layers are built from.
//
// Two feeds are implemented:
//
//   - CHAIN CONTROLS (`/data/d{N}/cc/ccStatusD{NN}.json`): every chain-control
//     checkpoint in the district, WITH an explicit status — "R-0" (no controls)
//     through "R-3". quickmap's cc.kml lists only active controls, so an empty
//     cc.kml cannot be told apart from a broken one; here a quiet day is 149
//     checkpoints each saying R-0, and an empty or frozen file is an error.
//   - LANE CLOSURES (`/data/d{N}/lcs/lcsStatusD{NN}.json`): planned closures
//     including SCHEDULED ones (lcs2way.kml shows only what is currently set
//     up), with epoch start/end times and the Caltrans radio codes that mark
//     a closure's lifecycle (10-97 set up, 10-98 picked up, 10-22 cancelled).
//
// CAVEATS, all observed on the live portal (2026-09-30):
//
//   - The documentation page answers 403; there is no contract we can read.
//     Treat it with the same posture as the quickmap KML and PG&E feeds.
//   - Some districts answer 500 (D4, D5, D12 for chain controls).
//   - The JSON is hand-templated, not serialized: the road-weather (rwis) feed
//     drops the comma between repeated sensor entries and fails to parse, D11's
//     chain-control file is not valid UTF-8, and D7 carried a LONGITUDE in a
//     checkpoint's `status` field. Parse strictly, and surface an unrecognized
//     status rather than reading it as R-0.
//   - Date/time strings are Pacific local time with no zone; the `*Epoch`
//     fields, where present, are real Unix epochs.
package cwwp2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the portal's data root; feeds hang off /d{N}/{feed}/.
const DefaultBaseURL = "https://cwwp2.dot.ca.gov/data"

// DefaultStaleAfter is how old a file's newest record stamp may be before the
// feed is treated as frozen. The portal regenerates the files every few
// minutes (Last-Modified tracked the wall clock to the minute in captures).
const DefaultStaleAfter = time.Hour

// maxBody caps a response read. D10's lane-closure file is ~1.1 MB.
const maxBody = 16 << 20

// ErrEmptyFeed is returned when a feed parses but holds no rows. A district
// always has chain-control checkpoints, so zero means the upstream broke — it
// must never read as "no chain controls".
var ErrEmptyFeed = errors.New("cwwp2: feed contains no records")

// ErrStaleFeed is returned when a feed's newest record stamp is older than the
// client's StaleAfter — the file is being served but no longer regenerated.
var ErrStaleFeed = errors.New("cwwp2: feed is stale")

// HTTPDoer is the slice of *http.Client the client uses, for test injection.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client fetches CWWP2 feeds.
type Client struct {
	HTTPClient HTTPDoer
	BaseURL    string
	// StaleAfter bounds the age of a feed's newest record; <=0 disables the check.
	StaleAfter time.Duration
	// Now is the clock used for the staleness check (tests pin it).
	Now func() time.Time
}

// NewClient returns a client against the live portal.
func NewClient() *Client {
	return NewClientWithHTTPDoer(&http.Client{Timeout: 30 * time.Second})
}

// NewClientWithHTTPDoer returns a client using the given transport.
func NewClientWithHTTPDoer(d HTTPDoer) *Client {
	return &Client{HTTPClient: d, BaseURL: DefaultBaseURL, StaleAfter: DefaultStaleAfter, Now: time.Now}
}

// FeedURL builds a district feed URL. The file name zero-pads the district
// ("ccStatusD03.json") while the directory does not ("/d3/") — the unpadded
// file name answers 500.
func (c *Client) FeedURL(district int, feed, prefix string) string {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return fmt.Sprintf("%s/d%d/%s/%sStatusD%02d.json", base, district, feed, prefix, district)
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cwwp2: build request: %w", err)
	}
	doer := c.HTTPClient
	if doer == nil {
		doer = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cwwp2: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cwwp2: GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("cwwp2: read %s: %w", url, err)
	}
	return body, nil
}

// checkFresh fails a feed whose newest record stamp is older than StaleAfter.
func (c *Client) checkFresh(newest time.Time) error {
	if c.StaleAfter <= 0 || newest.IsZero() {
		return nil
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if age := now().Sub(newest); age > c.StaleAfter {
		return fmt.Errorf("%w: newest record %s is %s old (limit %s)",
			ErrStaleFeed, newest.Format(time.RFC3339), age.Round(time.Minute), c.StaleAfter)
	}
	return nil
}

// Location is the portal's shared location block.
type Location struct {
	District     string
	Name         string // locationName, verbatim (upper-case for chain controls)
	NearbyPlace  string
	County       string
	Route        string // "SR-4", "US-50", "I-80"
	Direction    string // "East", "West", "North", "South" (may be empty)
	Postmile     string // with its prefix, e.g. "R9.51"
	Latitude     float64
	Longitude    float64
	ElevationFt  float64 // feet (Bear Valley reads 7116)
	HasPosition  bool    // lat/lng both parsed and non-zero
	FreeFormDesc string  // lane closures only
}

// pacific is the zone the portal's date/time strings are written in.
var pacific = func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.UTC
}()

// parseLocal parses a "2006-01-02" + "15:04:05" pair as Pacific time; zero on
// blank or malformed input.
func parseLocal(date, clock string) time.Time {
	date, clock = strings.TrimSpace(date), strings.TrimSpace(clock)
	if date == "" {
		return time.Time{}
	}
	if clock == "" {
		clock = "00:00:00"
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, pacific)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseEpoch parses a Unix-seconds string; zero on blank or malformed input.
func parseEpoch(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parseBool(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

func newLocation(district, name, nearby, lat, lng, elev, dir, county, route, pmPrefix, pm, freeForm string) Location {
	l := Location{
		District:     strings.TrimSpace(district),
		Name:         strings.TrimSpace(name),
		NearbyPlace:  strings.TrimSpace(nearby),
		County:       strings.TrimSpace(county),
		Route:        strings.TrimSpace(route),
		Direction:    strings.TrimSpace(dir),
		Postmile:     strings.TrimSpace(pmPrefix) + strings.TrimSpace(pm),
		Latitude:     parseFloat(lat),
		Longitude:    parseFloat(lng),
		ElevationFt:  parseFloat(elev),
		FreeFormDesc: strings.TrimSpace(freeForm),
	}
	l.HasPosition = l.Latitude != 0 && l.Longitude != 0
	return l
}
