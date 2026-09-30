package cwwp2

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LaneClosure is one planned closure window. Caltrans files a multi-day job
// as one row PER WINDOW (C4QB log 4 is four rows, one per day), so the unit
// here is a window, not a project.
type LaneClosure struct {
	// ID is the portal index: closure id, log number and window start
	// ("C4QB-0004-2026-10-02-07:01:00"). Unique per window.
	ID         string
	ClosureID  string // route-level PROJECT id — not unique on its own
	LogNumber  string
	RecordedAt time.Time

	FlowDirection string // "East / West", "North", ...
	Begin, End    Location

	RequestedAt   time.Time
	Start         time.Time
	EndTime       time.Time // zero when EndIndefinite
	EndIndefinite bool

	Facility       string // "Conventional Hwy", "Freeway", ...
	ClosureType    string // "Full", "Lane", "One-Way Traffic", "Alternating Lanes", "Moving"
	WorkType       string // "Drainage Work", "Roadway Excavation", ...
	Duration       string // "Standard", "Long Term", ...
	EstimatedDelay int    // minutes; 0 when none/unknown
	LanesClosed    string // "All", "RShoulder", "1, 2", ...
	TotalLanes     int
	CHINReportable bool // Caltrans flags it for its Highway Information Network

	// The Caltrans radio codes, zero when not (yet) called.
	SetUpAt     time.Time // 10-97: crew on scene, closure in place
	PickedUpAt  time.Time // 10-98: closure removed
	CancelledAt time.Time // 10-22: window cancelled
	SetUp       bool
	PickedUp    bool
	Cancelled   bool
}

// Phase is where a closure window stands at a given instant.
type Phase int

const (
	// PhaseScheduled: not set up yet — the window is in the future, or it has
	// opened but no 10-97 has been called (crews are often late or no-show).
	PhaseScheduled Phase = iota
	// PhaseActive: 10-97 called and not yet picked up.
	PhaseActive
	// PhaseCompleted: 10-98 called, or the window ended.
	PhaseCompleted
	// PhaseCancelled: 10-22 called.
	PhaseCancelled
)

func (p Phase) String() string {
	return [...]string{"SCHEDULED", "ACTIVE", "COMPLETED", "CANCELLED"}[p]
}

// PhaseAt derives the window's lifecycle phase. The radio codes win over the
// clock: a closure set up and not picked up is ACTIVE even past its planned
// end (overruns happen), and a cancelled one is CANCELLED whatever the window
// says. Only without codes does the clock decide.
func (l LaneClosure) PhaseAt(now time.Time) Phase {
	switch {
	case l.Cancelled:
		return PhaseCancelled
	case l.PickedUp:
		return PhaseCompleted
	case l.SetUp:
		return PhaseActive
	case !l.EndIndefinite && !l.EndTime.IsZero() && !now.Before(l.EndTime):
		return PhaseCompleted
	default:
		return PhaseScheduled
	}
}

type lcsCode struct {
	Is   string
	Date string
	Time string
	Ep   string
}

// UnmarshalJSON decodes the portal's per-code shape, whose key names embed the
// code number ({"isCode1097": .., "code1097Timestamp": {"code1097Epoch": ..}}).
func (c *lcsCode) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		if strings.HasPrefix(k, "isCode") {
			_ = json.Unmarshal(v, &c.Is)
			continue
		}
		var ts map[string]string
		if json.Unmarshal(v, &ts) != nil {
			continue
		}
		for tk, tv := range ts {
			switch {
			case strings.HasSuffix(tk, "Date"):
				c.Date = tv
			case strings.HasSuffix(tk, "Time"):
				c.Time = tv
			case strings.HasSuffix(tk, "Epoch"):
				c.Ep = tv
			}
		}
	}
	return nil
}

func (c lcsCode) at() time.Time {
	if t := parseEpoch(c.Ep); !t.IsZero() {
		return t
	}
	return parseLocal(c.Date, c.Time)
}

type lcsFile struct {
	Data []struct {
		LCS struct {
			Index           string `json:"index"`
			RecordTimestamp struct {
				Date  string `json:"recordDate"`
				Time  string `json:"recordTime"`
				Epoch string `json:"recordEpoch"`
			} `json:"recordTimestamp"`
			Location struct {
				TravelFlowDirection string            `json:"travelFlowDirection"`
				Begin               map[string]string `json:"begin"`
				End                 map[string]string `json:"end"`
			} `json:"location"`
			Closure struct {
				ClosureID string `json:"closureID"`
				LogNumber string `json:"logNumber"`
				Timestamp struct {
					RequestDate   string `json:"closureRequestDate"`
					RequestTime   string `json:"closureRequestTime"`
					RequestEpoch  string `json:"closureRequestEpoch"`
					StartDate     string `json:"closureStartDate"`
					StartTime     string `json:"closureStartTime"`
					StartEpoch    string `json:"closureStartEpoch"`
					EndDate       string `json:"closureEndDate"`
					EndTime       string `json:"closureEndTime"`
					EndEpoch      string `json:"closureEndEpoch"`
					EndIndefinite string `json:"isClosureEndIndefinite"`
				} `json:"closureTimestamp"`
				Facility           string  `json:"facility"`
				TypeOfClosure      string  `json:"typeOfClosure"`
				TypeOfWork         string  `json:"typeOfWork"`
				DurationOfClosure  string  `json:"durationOfClosure"`
				EstimatedDelay     string  `json:"estimatedDelay"`
				LanesClosed        string  `json:"lanesClosed"`
				TotalExistingLanes string  `json:"totalExistingLanes"`
				IsCHINReportable   string  `json:"isCHINReportable"`
				Code1097           lcsCode `json:"code1097"`
				Code1098           lcsCode `json:"code1098"`
				Code1022           lcsCode `json:"code1022"`
			} `json:"closure"`
		} `json:"lcs"`
	} `json:"data"`
}

// endpointLocation reads a begin/end block, whose keys carry the block name as
// a prefix ("beginLatitude", "endLatitude").
func endpointLocation(prefix string, m map[string]string) Location {
	g := func(k string) string { return m[prefix+k] }
	return newLocation(g("District"), g("LocationName"), g("NearbyPlace"), g("Latitude"), g("Longitude"),
		g("Elevation"), g("Direction"), g("County"), g("Route"), g("PostmilePrefix"), g("Postmile"), g("FreeFormDescription"))
}

func pickTime(epoch, date, clock string) time.Time {
	if t := parseEpoch(epoch); !t.IsZero() {
		return t
	}
	return parseLocal(date, clock)
}

// ParseLaneClosures decodes an lcsStatusD{NN}.json body. Unlike chain
// controls, an empty file is not an error here: a district can have no
// planned closures.
func ParseLaneClosures(body []byte) ([]LaneClosure, error) {
	var f lcsFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("cwwp2: decode lane closures: %w", err)
	}
	out := make([]LaneClosure, 0, len(f.Data))
	for _, row := range f.Data {
		r := row.LCS
		c := r.Closure
		ts := c.Timestamp
		delay, _ := strconv.Atoi(strings.TrimSpace(c.EstimatedDelay))
		lanes, _ := strconv.Atoi(strings.TrimSpace(c.TotalExistingLanes))
		lc := LaneClosure{
			ID:             strings.TrimSpace(r.Index),
			ClosureID:      strings.TrimSpace(c.ClosureID),
			LogNumber:      strings.TrimSpace(c.LogNumber),
			RecordedAt:     pickTime(r.RecordTimestamp.Epoch, r.RecordTimestamp.Date, r.RecordTimestamp.Time),
			FlowDirection:  strings.TrimSpace(r.Location.TravelFlowDirection),
			Begin:          endpointLocation("begin", r.Location.Begin),
			End:            endpointLocation("end", r.Location.End),
			RequestedAt:    pickTime(ts.RequestEpoch, ts.RequestDate, ts.RequestTime),
			Start:          pickTime(ts.StartEpoch, ts.StartDate, ts.StartTime),
			EndIndefinite:  parseBool(ts.EndIndefinite),
			Facility:       strings.TrimSpace(c.Facility),
			ClosureType:    strings.TrimSpace(c.TypeOfClosure),
			WorkType:       strings.TrimSpace(c.TypeOfWork),
			Duration:       strings.TrimSpace(c.DurationOfClosure),
			EstimatedDelay: delay,
			LanesClosed:    strings.TrimSpace(c.LanesClosed),
			TotalLanes:     lanes,
			CHINReportable: parseBool(c.IsCHINReportable),
			SetUp:          parseBool(c.Code1097.Is),
			PickedUp:       parseBool(c.Code1098.Is),
			Cancelled:      parseBool(c.Code1022.Is),
		}
		if !lc.EndIndefinite {
			lc.EndTime = pickTime(ts.EndEpoch, ts.EndDate, ts.EndTime)
		}
		if lc.SetUp {
			lc.SetUpAt = c.Code1097.at()
		}
		if lc.PickedUp {
			lc.PickedUpAt = c.Code1098.at()
		}
		if lc.Cancelled {
			lc.CancelledAt = c.Code1022.at()
		}
		out = append(out, lc)
	}
	return out, nil
}

// LaneClosures fetches and parses one district's closure windows. A frozen
// file is ErrStaleFeed; an empty one is a legitimate empty result.
func (c *Client) LaneClosures(ctx context.Context, district int) ([]LaneClosure, error) {
	body, err := c.get(ctx, c.FeedURL(district, "lcs", "lcs"))
	if err != nil {
		return nil, err
	}
	closures, err := ParseLaneClosures(body)
	if err != nil {
		return nil, fmt.Errorf("district %d: %w", district, err)
	}
	var newest time.Time
	for _, lc := range closures {
		if lc.RecordedAt.After(newest) {
			newest = lc.RecordedAt
		}
	}
	if err := c.checkFresh(newest); err != nil {
		return nil, fmt.Errorf("district %d lane closures: %w", district, err)
	}
	return closures, nil
}
