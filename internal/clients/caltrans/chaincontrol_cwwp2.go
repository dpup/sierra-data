package caltrans

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// Attribution hosts for ChainControlData.Source.
const (
	SourceCWWP2    = "cwwp2.dot.ca.gov"
	SourceQuickMap = "quickmap.dot.ca.gov"
)

// ChainControlSource is the slice of *cwwp2.Client the parser uses.
type ChainControlSource interface {
	ChainControls(ctx context.Context, district int) ([]cwwp2.ChainControl, error)
}

// PartialError reports that chain-control data was returned but is known to
// be incomplete. Callers that can serve partial data (with a degraded status)
// should keep the controls returned alongside it; a plain error means none.
type PartialError struct{ Err error }

func (e *PartialError) Error() string { return "partial chain-control data: " + e.Err.Error() }
func (e *PartialError) Unwrap() error { return e.Err }

// UseCWWP2ChainControls switches chain-control LEVELS to the CWWP2 per-
// checkpoint feed for the given districts.
//
// Why: cc.kml lists only active controls, so an empty cc.kml — the normal
// state from spring to fall — is indistinguishable from a broken one. CWWP2
// lists every checkpoint with an explicit R-0, and the client fails an empty
// or frozen file, so "no chain controls" becomes a confirmed state.
//
// cc.kml is still read, but only for the entries CWWP2 has not been seen to
// carry: road closures ("Eastbound Highway 4 Road Closed" — the seasonal pass
// closures) and truck-only levels (MAX / MIN / TS screening). Every CWWP2
// status observed so far (all districts, 2026-09-30) was R-0; whether winter
// closures appear there is unverified, so dropping cc.kml would be a guess.
func (p *FeedParser) UseCWWP2ChainControls(src ChainControlSource, districts []int) {
	p.chainSource = src
	p.chainDistricts = append([]int(nil), districts...)
}

// chainControlsCWWP2 is ParseChainControlsDetailed with a CWWP2 source.
func (p *FeedParser) chainControlsCWWP2(ctx context.Context) ([]ChainControlData, error) {
	var out []ChainControlData
	for _, d := range p.chainDistricts {
		checkpoints, err := p.chainSource.ChainControls(ctx, d)
		if err != nil {
			// All or nothing per call: a missing district would read as
			// "no controls" for its whole footprint.
			return nil, fmt.Errorf("cwwp2 chain controls: %w", err)
		}
		for _, cp := range checkpoints {
			// Out-of-service checkpoints are skipped only when they report
			// nothing: a requirement still on a sign we can't poll is shown.
			if cp.Level == cwwp2.LevelNone || (!cp.InService && !cp.Level.Active()) {
				continue
			}
			out = append(out, chainControlFromCWWP2(cp))
		}
	}

	incidents, err := p.ParseChainControls(ctx)
	if err != nil {
		return out, &PartialError{Err: fmt.Errorf("cc.kml (road closures, truck levels): %w", err)}
	}
	var supplement []CaltransIncident
	for _, in := range incidents {
		if isKMLSupplement(in) {
			supplement = append(supplement, in)
		}
	}
	for _, c := range p.parseChainControlDetails(supplement) {
		c.Source = SourceQuickMap
		out = append(out, c)
	}
	return out, nil
}

var truckLevelRe = regexp.MustCompile(`(?i)\blevel\s+(MAX|MIN|TS)\b|truck chain requirements|screening for chains`)

// isKMLSupplement picks the cc.kml entries CWWP2 does not cover, by POSITIVE
// match only. Selecting "whatever has no parseable R-level" instead would be
// format-fragile: the CHP and lane-closure KMLs moved to the iw-* layout in
// 2026 with blank <name>s, and if cc.kml follows, no R-level would parse and
// every chain control would leak through as a level-less duplicate of its
// CWWP2 checkpoint. Anything unclassified is dropped — R-levels are CWWP2's.
//
//   - road closures: styleUrl "#full-closure" (the style id is the same in
//     the 2025 legacy capture and the 2026 file).
//   - truck-only levels: "level MAX/MIN/TS" in the name, or the truck
//     requirement / chain-screening text in the description.
func isKMLSupplement(in CaltransIncident) bool {
	if strings.EqualFold(strings.TrimSpace(in.StyleUrl), "#full-closure") {
		return true
	}
	return truckLevelRe.MatchString(in.Name) || truckLevelRe.MatchString(in.DescriptionText)
}

// chainControlFromCWWP2 maps a checkpoint onto the shape the cc.kml parser
// produces, so consumers stay source-agnostic.
func chainControlFromCWWP2(cp cwwp2.ChainControl) ChainControlData {
	c := ChainControlData{
		Highway:      highwayLabel(cp.Location.Route),
		Direction:    directionLabel(cp.Location.Direction),
		LocationName: cp.Location.Name,
		Description:  cp.Description,
		MessageID:    cp.ID,
		District:     cp.Location.District,
		Source:       SourceCWWP2,
		RawStatus:    cp.RawStatus,
	}
	if cp.Location.HasPosition {
		c.Coordinates = &api.Coordinates{Latitude: cp.Location.Latitude, Longitude: cp.Location.Longitude}
	}
	if !cp.StatusSince.IsZero() {
		c.EffectiveTime = cp.StatusSince.Format(rfc3339)
	}
	if !cp.RecordedAt.IsZero() {
		c.LastUpdated = cp.RecordedAt.Format(rfc3339)
	}
	if cp.Level.Active() {
		c.Level = cp.Level.String()
	} else {
		c.Unrecognized = true
		c.Description = fmt.Sprintf("Caltrans reported an unrecognized chain-control status (%q)", cp.RawStatus)
	}
	return c
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

var routeRe = regexp.MustCompile(`^(SR|US|I)-?\s*(\d+)`)

// highwayLabel renders a CWWP2 route in cc.kml's style, which the roads
// service's highway matcher understands: "SR-4" -> "Highway 4",
// "US-50" -> "US 50", "I-80" -> "I-80".
func highwayLabel(route string) string {
	m := routeRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(route)))
	if m == nil {
		return strings.TrimSpace(route)
	}
	switch m[1] {
	case "SR":
		return "Highway " + m[2]
	case "US":
		return "US " + m[2]
	default:
		return "I-" + m[2]
	}
}

// directionLabel renders "East" as cc.kml's "Eastbound".
func directionLabel(dir string) string {
	switch strings.ToLower(strings.TrimSpace(dir)) {
	case "north":
		return "Northbound"
	case "south":
		return "Southbound"
	case "east":
		return "Eastbound"
	case "west":
		return "Westbound"
	default:
		return strings.TrimSpace(dir)
	}
}
