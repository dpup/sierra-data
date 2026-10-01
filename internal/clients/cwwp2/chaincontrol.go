package cwwp2

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Level is a chain-control requirement level. LevelUnknown is a status the
// parser did not recognize — never a synonym for "none".
type Level int

const (
	LevelUnknown Level = iota
	LevelNone          // R-0: no chain controls in effect
	LevelR1
	LevelR2
	LevelR3
)

// String renders the level in the codebase's compact form ("R0".."R3"), or
// "UNKNOWN".
func (l Level) String() string {
	switch l {
	case LevelNone:
		return "R0"
	case LevelR1:
		return "R1"
	case LevelR2:
		return "R2"
	case LevelR3:
		return "R3"
	default:
		return "UNKNOWN"
	}
}

// Active reports whether the level is a chain requirement (R-1..R-3).
func (l Level) Active() bool { return l == LevelR1 || l == LevelR2 || l == LevelR3 }

var chainStatusRe = regexp.MustCompile(`(?i)^R-?([0-3])$`)

// ParseLevel maps a raw status ("R-2") to a Level; anything else is
// LevelUnknown.
func ParseLevel(status string) Level {
	m := chainStatusRe.FindStringSubmatch(strings.TrimSpace(status))
	if m == nil {
		return LevelUnknown
	}
	return [...]Level{LevelNone, LevelR1, LevelR2, LevelR3}[m[1][0]-'0']
}

// ChainControl is one checkpoint's current status.
type ChainControl struct {
	// ID is the portal's checkpoint index, e.g. "10-ALP-4-0.65-W-14W" —
	// district, county, route, postmile, direction and sign. Stable across
	// status changes.
	ID          string
	RecordedAt  time.Time // when this file's row was generated
	Location    Location
	InService   bool
	Level       Level
	RawStatus   string    // verbatim status, kept so an Unknown level is diagnosable
	Description string    // the requirement text, e.g. "No chain controls are in effect at this time."
	StatusSince time.Time // when the checkpoint entered its current status
}

type ccFile struct {
	Data []struct {
		CC struct {
			Index           string `json:"index"`
			RecordTimestamp struct {
				Date string `json:"recordDate"`
				Time string `json:"recordTime"`
			} `json:"recordTimestamp"`
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
			InService  string `json:"inService"`
			StatusData struct {
				StatusTimestamp struct {
					Date string `json:"statusDate"`
					Time string `json:"statusTime"`
				} `json:"statusTimestamp"`
				Status            string `json:"status"`
				StatusDescription string `json:"statusDescription"`
			} `json:"statusData"`
		} `json:"cc"`
	} `json:"data"`
}

// ParseChainControls decodes a ccStatusD{NN}.json body. A body with no rows is
// ErrEmptyFeed.
func ParseChainControls(body []byte) ([]ChainControl, error) {
	var f ccFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("cwwp2: decode chain controls: %w", err)
	}
	if len(f.Data) == 0 {
		return nil, ErrEmptyFeed
	}
	out := make([]ChainControl, 0, len(f.Data))
	for _, row := range f.Data {
		c := row.CC
		l := c.Location
		out = append(out, ChainControl{
			ID:          strings.TrimSpace(c.Index),
			RecordedAt:  parseLocal(c.RecordTimestamp.Date, c.RecordTimestamp.Time),
			Location:    newLocation(l.District, l.LocationName, l.NearbyPlace, l.Latitude, l.Longitude, l.Elevation, l.Direction, l.County, l.Route, l.PostmilePrefix, l.Postmile, ""),
			InService:   parseBool(c.InService),
			Level:       ParseLevel(c.StatusData.Status),
			RawStatus:   strings.TrimSpace(c.StatusData.Status),
			Description: strings.TrimSpace(c.StatusData.StatusDescription),
			StatusSince: parseLocal(c.StatusData.StatusTimestamp.Date, c.StatusData.StatusTimestamp.Time),
		})
	}
	return out, nil
}

// ChainControls fetches and parses one district's checkpoints. Beyond
// transport/decode errors it fails on an empty file (ErrEmptyFeed) and on a
// frozen one (ErrStaleFeed) — the two ways this feed could otherwise pass
// "nothing in effect" off as the truth.
func (c *Client) ChainControls(ctx context.Context, district int) ([]ChainControl, error) {
	body, err := c.get(ctx, c.FeedURL(district, "cc", "cc"))
	if err != nil {
		return nil, err
	}
	controls, err := ParseChainControls(body)
	if err != nil {
		return nil, fmt.Errorf("district %d: %w", district, err)
	}
	var newest time.Time
	for _, cc := range controls {
		if cc.RecordedAt.After(newest) {
			newest = cc.RecordedAt
		}
	}
	if err := c.checkFresh(newest, len(controls)); err != nil {
		return nil, fmt.Errorf("district %d chain controls: %w", district, err)
	}
	return controls, nil
}
