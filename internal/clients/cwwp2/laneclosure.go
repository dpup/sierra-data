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
	// ("C4QB-0004-2026-10-02-07:01:00"). Unique per window within a district.
	ID         string
	District   int    // the district file the row came from (set by Client.LaneClosures)
	ClosureID  string // route-level PROJECT id — not unique on its own
	LogNumber  string
	RecordedAt time.Time // the FILE's generation stamp: every row carries the same one

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

	// Unrecognized is non-empty (the reason) when the row cannot be trusted to
	// say where or when the window is, or whether it was set up, picked up or
	// cancelled. Don't phase such a row. The JSON is hand-templated (see the
	// package doc), and a code block whose key or value drifts would otherwise
	// decode as "not called": a set-up closure would read SCHEDULED, and the
	// clock would then call it COMPLETED at its planned end even if it overran.
	// The caller decides whether the row is in its scope and how loudly to fail.
	Unrecognized string
}

// Phase is where a closure window stands at a given instant.
type Phase int

const (
	// PhaseScheduled: not set up yet — the window is in the future, or it has
	// opened but no 10-97 has been called (crews are often late or no-show).
	PhaseScheduled Phase = iota
	// PhaseActive: 10-97 called and not yet picked up, and not overrun past
	// its planned end by more than the overrun grace.
	PhaseActive
	// PhaseCompleted: 10-98 called, or the window ended (planned end with no
	// set-up call, or planned end + overrun grace with one).
	PhaseCompleted
	// PhaseCancelled: 10-22 called.
	PhaseCancelled
)

func (p Phase) String() string {
	return [...]string{"SCHEDULED", "ACTIVE", "COMPLETED", "CANCELLED"}[p]
}

// DefaultOverrunGrace is how long past its planned end a set-up closure with
// no 10-98 stays ACTIVE when roads.caltransFeeds.cwwp2.laneClosureOverrunGrace
// is unset. Windows are mostly day shifts of 8-10h, so a crew working late or
// through the night fits inside 12h, while a closure still "set up" by the next
// morning's shift is far more likely a pickup nobody radioed than a lane still
// coned off. Days-long jobs are filed as indefinite or as one row per day, and
// those rows are unaffected.
const DefaultOverrunGrace = 12 * time.Hour

// PhaseAt derives the window's lifecycle phase. The radio codes win over the
// clock: a cancelled window is CANCELLED whatever the window says, a picked-up
// one COMPLETED, and a set-up one ACTIVE even past its planned end, because
// crews overrun. Without codes the clock decides.
//
// The one exception bounds the overrun. Crews sometimes never radio the 10-98,
// and the row can stay in the file for days, so a set-up window with a planned
// end (not indefinite) is presumed COMPLETED once now reaches that end plus
// overrunGrace (<= 0 uses DefaultOverrunGrace). That is an inference from the
// clock, the same kind the no-show case (planned end, never set up) already
// makes; an ACTIVE closure days after its plan says something we have no
// evidence for. Indefinite windows have no planned end and stay ACTIVE until
// a code ends them.
func (l LaneClosure) PhaseAt(now time.Time, overrunGrace time.Duration) Phase {
	if overrunGrace <= 0 {
		overrunGrace = DefaultOverrunGrace
	}
	planned := !l.EndIndefinite && !l.EndTime.IsZero()
	switch {
	case l.Cancelled:
		return PhaseCancelled
	case l.PickedUp:
		return PhaseCompleted
	case l.SetUp && planned && !now.Before(l.EndTime.Add(overrunGrace)):
		return PhaseCompleted
	case l.SetUp:
		return PhaseActive
	case planned && !now.Before(l.EndTime):
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

// called reads the code's "is" flag strictly: "true" or "false" and nothing
// else. ok is false for a blank or missing flag, which is what a renamed key
// decodes to.
func (c lcsCode) called() (called, ok bool) {
	switch strings.ToLower(strings.TrimSpace(c.Is)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
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

// unrecognizedReason names the first thing that makes a row unusable, or "".
// Every row in the 2026-10-01 captures of D3, D6, D9 and D10 (2,593 rows)
// passes, so this fires only on drift.
func unrecognizedReason(lc LaneClosure, codesOK [3]bool) string {
	for i, code := range []string{"10-97", "10-98", "10-22"} {
		if !codesOK[i] {
			return "unreadable " + code + " flag"
		}
	}
	switch {
	case lc.ID == "":
		return "no index"
	case lc.Start.IsZero():
		return "no start time"
	case !lc.EndIndefinite && lc.EndTime.IsZero():
		return "no end time"
	case !lc.Begin.HasPosition && !lc.End.HasPosition:
		return "no position"
	}
	return ""
}

// ParseLaneClosures decodes an lcsStatusD{NN}.json body. An empty body parses
// to no rows; Client.LaneClosures is what refuses it (see there).
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
		}
		var codesOK [3]bool
		lc.SetUp, codesOK[0] = c.Code1097.called()
		lc.PickedUp, codesOK[1] = c.Code1098.called()
		lc.Cancelled, codesOK[2] = c.Code1022.called()
		if !lc.EndIndefinite {
			lc.EndTime = pickTime(ts.EndEpoch, ts.EndDate, ts.EndTime)
		}
		lc.Unrecognized = unrecognizedReason(lc, codesOK)
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
// file is ErrStaleFeed, and an EMPTY one is ErrEmptyFeed.
//
// The empty case was once accepted as "a district can have no planned
// closures". It can't in practice: D3 lists ~1,100 windows and D10 ~630, D9
// (the quietest district checked) 81. An empty file also has no record stamp,
// so the freshness check can't run. If it were accepted, the grid's
// disappearance sweep would read it as every closure ending at once.
func (c *Client) LaneClosures(ctx context.Context, district int) ([]LaneClosure, error) {
	body, err := c.get(ctx, c.FeedURL(district, "lcs", "lcs"))
	if err != nil {
		return nil, err
	}
	closures, err := ParseLaneClosures(body)
	if err != nil {
		return nil, fmt.Errorf("district %d: %w", district, err)
	}
	if len(closures) == 0 {
		return nil, fmt.Errorf("district %d lane closures: %w", district, ErrEmptyFeed)
	}
	var newest time.Time
	for i := range closures {
		closures[i].District = district
		if closures[i].RecordedAt.After(newest) {
			newest = closures[i].RecordedAt
		}
	}
	if err := c.checkFresh(newest, len(closures)); err != nil {
		return nil, fmt.Errorf("district %d lane closures: %w", district, err)
	}
	return closures, nil
}
