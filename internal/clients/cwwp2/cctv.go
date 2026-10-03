package cwwp2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Camera is one Caltrans CCTV camera from a district's camera list
// (`/data/d{N}/cctv/cctvStatusD{NN}.json`). The list is near-static: the file's
// Last-Modified moves when Caltrans edits the registry (weeks to months apart),
// not on a schedule. The images and streams it points at are live and are
// served by Caltrans — callers link to them, never proxy them.
//
// There is NO freshness signal to check here, unlike the chain-control feed.
// `recordTimestamp` is when the camera's record was last edited (2022–2026 in
// the 2026-10-01 captures), not when the file or the image was generated.
type Camera struct {
	// ID is "d{district}-{index}", e.g. "d10-172". The portal's `index` is only
	// unique within a district, so the district is folded in.
	ID       string
	District int
	// Name is the portal's locationName, trimmed, with District 10's leading
	// sequence number removed ("179 - EB 108 W/O Soulsbyville Rd" ->
	// "EB 108 W/O Soulsbyville Rd"). Other districts use other conventions
	// ("US-395 : Bridgeport", "Hwy 50 at Echo Summit") and pass through.
	Name     string
	Location Location
	// InService is the portal's own flag. Every out-of-service camera checked
	// on 2026-10-01 in D10 and D9 served a "Down for Construction" placeholder.
	// It is conservative, not exact: two D3 cameras flagged out of service were
	// serving live images.
	InService bool
	// ImageURL is the current snapshot JPEG. Empty when the portal's value is
	// not an absolute https URL.
	ImageURL string
	// ImageRefreshMinutes is how often Caltrans replaces the snapshot
	// (currentImageUpdateFrequency). 0 when the portal says "Not Reported" or
	// anything else that isn't a positive whole number.
	ImageRefreshMinutes int
	// StreamURL is the HLS playlist (.m3u8). Empty for an image-only camera
	// (every District 9 camera) or a value that is not an absolute https URL.
	StreamURL string
	// Description is Caltrans's note on the view ("Looking South"). Usually
	// empty.
	Description string
}

type cctvFile struct {
	Data []struct {
		CCTV struct {
			Index    string `json:"index"`
			Location struct {
				District       string `json:"district"`
				LocationName   string `json:"locationName"`
				NearbyPlace    string `json:"nearbyPlace"`
				Longitude      string `json:"longitude"`
				Latitude       string `json:"latitude"`
				Elevation      string `json:"elevation"`
				Direction      string `json:"direction"`
				County         string `json:"county"`
				Route          string `json:"route"`
				PostmilePrefix string `json:"postmilePrefix"`
				Postmile       string `json:"postmile"`
			} `json:"location"`
			InService string `json:"inService"`
			ImageData struct {
				ImageDescription  string `json:"imageDescription"`
				StreamingVideoURL string `json:"streamingVideoURL"`
				Static            struct {
					CurrentImageUpdateFrequency string `json:"currentImageUpdateFrequency"`
					CurrentImageURL             string `json:"currentImageURL"`
				} `json:"static"`
			} `json:"imageData"`
		} `json:"cctv"`
	} `json:"data"`
}

// d10SequencePrefix is District 10's "NNN - " camera-number prefix.
var d10SequencePrefix = regexp.MustCompile(`^\d+\s+-\s+`)

// ParseCameras decodes a cctvStatusD{NN}.json body for the given district. A
// body with no rows is ErrEmptyFeed: every district has cameras, so zero means
// the upstream broke. Rows are returned as published, out-of-service ones
// included; deciding what to show is the caller's job.
func ParseCameras(district int, body []byte) ([]Camera, error) {
	var f cctvFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("cwwp2: decode cameras: %w", err)
	}
	if len(f.Data) == 0 {
		return nil, ErrEmptyFeed
	}
	out := make([]Camera, 0, len(f.Data))
	for _, row := range f.Data {
		c := row.CCTV
		l := c.Location
		name := strings.TrimSpace(l.LocationName)
		if district == 10 {
			name = d10SequencePrefix.ReplaceAllString(name, "")
		}
		out = append(out, Camera{
			ID:                  fmt.Sprintf("d%d-%s", district, strings.TrimSpace(c.Index)),
			District:            district,
			Name:                name,
			Location:            newLocation(l.District, l.LocationName, l.NearbyPlace, l.Latitude, l.Longitude, l.Elevation, l.Direction, l.County, l.Route, l.PostmilePrefix, l.Postmile, ""),
			InService:           parseBool(c.InService),
			ImageURL:            httpsURL(c.ImageData.Static.CurrentImageURL),
			ImageRefreshMinutes: positiveInt(c.ImageData.Static.CurrentImageUpdateFrequency),
			StreamURL:           httpsURL(c.ImageData.StreamingVideoURL),
			Description:         strings.TrimSpace(c.ImageData.ImageDescription),
		})
	}
	return out, nil
}

// Cameras fetches and parses one district's camera list.
func (c *Client) Cameras(ctx context.Context, district int) ([]Camera, error) {
	body, err := c.get(ctx, c.FeedURL(district, "cctv", "cctv"))
	if err != nil {
		return nil, err
	}
	cams, err := ParseCameras(district, body)
	if err != nil {
		return nil, fmt.Errorf("district %d: %w", district, err)
	}
	return cams, nil
}

// httpsURL returns s when it is an absolute https URL with a host, else "".
// These URLs go straight into consumers' <img>/<video> elements, so a value
// that isn't one (a relative path, another scheme, hand-templated garbage) is
// dropped rather than passed through.
func httpsURL(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return s
}

// positiveInt parses a whole number > 0; anything else is 0.
func positiveInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
