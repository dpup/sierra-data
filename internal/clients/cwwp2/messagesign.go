package cwwp2

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"
)

// Display is how a changeable message sign is presenting its message.
// DisplayUnknown covers "Not Reported", a mode the parser does not recognize,
// and a mode the sign's own lines contradict — never a synonym for blank.
type Display int

const (
	DisplayUnknown Display = iota
	DisplayBlank           // "Blank": the sign is dark
	DisplayOnePage         // "1 Page (Normal)": phase 1 alone
	DisplayTwoPage         // "2 Pages (Extended)": phases 1 and 2 alternate
)

func (d Display) String() string {
	return [...]string{"UNKNOWN", "BLANK", "ONE_PAGE", "TWO_PAGE"}[d]
}

// notReported is the portal's placeholder for a field it has no value for. A
// sign it can't read carries it in every line, so it must never read as text.
const notReported = "Not Reported"

// MessageSign is one changeable message sign (CMS) and what it is showing.
type MessageSign struct {
	// ID is the portal index, verbatim. In District 10 it is unique and
	// matches the sign number ("V42" for "42 - EB 108 SOULSBYVILLE"), but it
	// is not an identity everywhere: D12 numbers every sign "1", D3 and D11
	// repeat some, D3 has "N/A". It never carries the district.
	ID         string
	RecordedAt time.Time // when this file's row was generated
	// Location.Name leads with the sign number ("42 - EB 108 SOULSBYVILLE").
	// Location.Route names ONE route where routes share the road: D10 files
	// "45 - EB 49 (MOCCASIN)" under SR-120 and "43 - EB 120 W/O YOSEMITE"
	// under SR-108. Match signs to roads by position, not by route.
	Location   Location
	InService  bool
	Display    Display
	RawDisplay string // verbatim display mode, kept so an Unknown is diagnosable
	// Phase1 and Phase2 are the sign's two pages of three lines, entity-decoded
	// ("DON&apos;T" → "DON'T") and trimmed, exactly as reported: including
	// text Display says is not showing, and the "Not Reported" placeholder.
	// Read the message through Pages or Text, which honor Display.
	Phase1, Phase2 [3]string
	// MessageSince is when the message last changed. Zero when the portal
	// doesn't report it, which D7 does even for signs that are showing text.
	MessageSince time.Time
}

// Pages returns what the sign is showing: nothing when it is blank or its
// message is unknown, phase 1 for a one-page message, both phases for two.
func (s MessageSign) Pages() [][3]string {
	switch s.Display {
	case DisplayOnePage:
		return [][3]string{s.Phase1}
	case DisplayTwoPage:
		return [][3]string{s.Phase1, s.Phase2}
	default:
		return nil
	}
}

// Text renders what the sign is showing on one line: each page's non-empty
// lines joined by spaces, pages by " / " ("GUSTY WIND WARNING / OVER PACHECO
// PASS"). Empty when nothing is showing; Display tells blank from unknown.
func (s MessageSign) Text() string {
	var pages []string
	for _, p := range s.Pages() {
		if t := joinLines(p); t != "" {
			pages = append(pages, t)
		}
	}
	return strings.Join(pages, " / ")
}

func joinLines(p [3]string) string {
	var lines []string
	for _, l := range p {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " ")
}

func hasText(p [3]string) bool { return joinLines(p) != "" }

// parseDisplay maps a raw display mode to a Display, then checks it against
// the lines: a blank sign carries no text, a one-page message text in phase 1
// alone, a two-page message text in both. Anything else — including a line
// holding the "Not Reported" placeholder — is DisplayUnknown. On 2026-10-01
// all 1,017 signs across the twelve districts passed these checks, except the
// 9 whose display mode was itself "Not Reported".
func parseDisplay(raw string, p1, p2 [3]string) Display {
	for _, l := range append(p1[:], p2[:]...) {
		if l == notReported {
			return DisplayUnknown
		}
	}
	raw = strings.TrimSpace(raw)
	switch {
	case strings.EqualFold(raw, "Blank") && !hasText(p1) && !hasText(p2):
		return DisplayBlank
	case strings.EqualFold(raw, "1 Page (Normal)") && hasText(p1) && !hasText(p2):
		return DisplayOnePage
	case strings.EqualFold(raw, "2 Pages (Extended)") && hasText(p1) && hasText(p2):
		return DisplayTwoPage
	default:
		return DisplayUnknown
	}
}

// signLine decodes one line of sign text. The JSON carries XML entities
// ("DON&apos;T SPEED"), and leading spaces that only position text on the
// face (" .US 50 22 MIN", D3).
func signLine(s string) string { return strings.TrimSpace(html.UnescapeString(s)) }

// parseMessageTime reads the message stamp. "Not Reported" fails to parse and
// is zero; so is the Unix-epoch placeholder D10 gives a sign that has never
// reported (V900, "1970-01-01 00:00:00").
func parseMessageTime(date, clock string) time.Time {
	t := parseLocal(date, clock)
	if t.Year() < 2000 {
		return time.Time{}
	}
	return t
}

type cmsFile struct {
	Data []struct {
		CMS struct {
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
			InService string `json:"inService"`
			Message   struct {
				Timestamp struct {
					Date string `json:"messageDate"`
					Time string `json:"messageTime"`
				} `json:"messageTimestamp"`
				Display string `json:"display"`
				Phase1  struct {
					Line1 string `json:"phase1Line1"`
					Line2 string `json:"phase1Line2"`
					Line3 string `json:"phase1Line3"`
				} `json:"phase1"`
				Phase2 struct {
					Line1 string `json:"phase2Line1"`
					Line2 string `json:"phase2Line2"`
					Line3 string `json:"phase2Line3"`
				} `json:"phase2"`
			} `json:"message"`
		} `json:"cms"`
	} `json:"data"`
}

// ParseMessageSigns decodes a cmsStatusD{NN}.json body. A body with no rows is
// ErrEmptyFeed: every district has signs, dark ones included.
func ParseMessageSigns(body []byte) ([]MessageSign, error) {
	var f cmsFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("cwwp2: decode message signs: %w", err)
	}
	if len(f.Data) == 0 {
		return nil, ErrEmptyFeed
	}
	out := make([]MessageSign, 0, len(f.Data))
	for _, row := range f.Data {
		c := row.CMS
		l := c.Location
		m := c.Message
		p1 := [3]string{signLine(m.Phase1.Line1), signLine(m.Phase1.Line2), signLine(m.Phase1.Line3)}
		p2 := [3]string{signLine(m.Phase2.Line1), signLine(m.Phase2.Line2), signLine(m.Phase2.Line3)}
		out = append(out, MessageSign{
			ID:           strings.TrimSpace(c.Index),
			RecordedAt:   parseLocal(c.RecordTimestamp.Date, c.RecordTimestamp.Time),
			Location:     newLocation(l.District, l.LocationName, l.NearbyPlace, l.Latitude, l.Longitude, l.Elevation, l.Direction, l.County, l.Route, l.PostmilePrefix, l.Postmile, ""),
			InService:    parseBool(c.InService),
			Display:      parseDisplay(m.Display, p1, p2),
			RawDisplay:   strings.TrimSpace(m.Display),
			Phase1:       p1,
			Phase2:       p2,
			MessageSince: parseMessageTime(m.Timestamp.Date, m.Timestamp.Time),
		})
	}
	return out, nil
}

// MessageSigns fetches and parses one district's signs. Beyond transport and
// decode errors it fails on an empty file (ErrEmptyFeed) and on a frozen one
// (ErrStaleFeed). On 2026-10-01 D7's file had been frozen for more than two
// days, still answering 200, while every other district's was current.
func (c *Client) MessageSigns(ctx context.Context, district int) ([]MessageSign, error) {
	body, err := c.get(ctx, c.FeedURL(district, "cms", "cms"))
	if err != nil {
		return nil, err
	}
	signs, err := ParseMessageSigns(body)
	if err != nil {
		return nil, fmt.Errorf("district %d: %w", district, err)
	}
	var newest time.Time
	for _, s := range signs {
		if s.RecordedAt.After(newest) {
			newest = s.RecordedAt
		}
	}
	if err := c.checkFresh(newest, len(signs)); err != nil {
		return nil, fmt.Errorf("district %d message signs: %w", district, err)
	}
	return signs, nil
}
