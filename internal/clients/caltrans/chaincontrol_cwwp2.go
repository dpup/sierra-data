package caltrans

import (
	"context"
	"errors"
	"fmt"
	"math"
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

// UseCWWP2ChainControls adds the CWWP2 per-checkpoint feed for the given
// districts as a chain-control source alongside cc.kml.
//
// Why: cc.kml lists only active controls, so an empty cc.kml — the normal
// state from spring to fall — is indistinguishable from a broken one. CWWP2
// lists every checkpoint with an explicit R-0, and the client fails an empty
// or frozen file, so "no chain controls" becomes a confirmed state.
//
// The two are MERGED, not ranked. Both describe the same checkpoint registry
// (every point in the 2025-12-24 cc.kml capture sits 0 m from a CWWP2
// checkpoint of the same name, the pass-closure gates included), but cc.kml
// carries Highway-Information-style "District / Message ID" identifiers, so
// its STATUS may come from a different Caltrans system, and the two have never
// been observed side by side in a storm. Dropping one source's controls in
// favour of the other's silence would turn a disagreement into "no chains".
// See mergeKML for the rules.
func (p *FeedParser) UseCWWP2ChainControls(src ChainControlSource, districts []int) {
	p.chainSource = src
	p.chainDistricts = append([]int(nil), districts...)
}

// chainControlsCWWP2 is ParseChainControlsDetailed with a CWWP2 source.
func (p *FeedParser) chainControlsCWWP2(ctx context.Context) ([]ChainControlData, error) {
	var cw, quiet []ChainControlData
	var cwErr error
	for _, d := range p.chainDistricts {
		checkpoints, err := p.chainSource.ChainControls(ctx, d)
		if err != nil {
			// All or nothing: a missing district would read as "no controls"
			// for its whole footprint.
			cw, cwErr = nil, fmt.Errorf("cwwp2 chain controls: %w", err)
			break
		}
		for _, cp := range checkpoints {
			// Out-of-service checkpoints are skipped only when they report
			// nothing: a requirement still on a sign we can't poll is shown.
			if cp.Level == cwwp2.LevelNone || (!cp.InService && !cp.Level.Active()) {
				if cp.Level == cwwp2.LevelNone {
					quiet = append(quiet, chainControlFromCWWP2(cp)) // position only, for mergeKML
				}
				continue
			}
			cw = append(cw, chainControlFromCWWP2(cp))
		}
	}

	incidents, kmlErr := p.ParseChainControls(ctx)
	switch {
	case cwErr != nil && kmlErr != nil:
		return nil, errors.Join(cwErr, fmt.Errorf("cc.kml: %w", kmlErr))
	case cwErr != nil:
		// CWWP2 down: cc.kml alone is what the service served before CWWP2,
		// so serve it — degraded. But an EMPTY cc.kml is exactly the answer
		// that cannot be trusted on its own, so with nothing to show this
		// stays an error: an error never becomes a 0.
		kml := p.parseChainControlDetails(incidents)
		if len(kml) == 0 {
			return nil, cwErr
		}
		for i := range kml {
			kml[i].Source = SourceQuickMap
		}
		return kml, &PartialError{Err: cwErr}
	case kmlErr != nil:
		return cw, &PartialError{Err: fmt.Errorf("cc.kml (road closures, truck levels, cross-check): %w", kmlErr)}
	}
	return p.mergeKML(cw, quiet, incidents), nil
}

// sameCheckpointMeters is how close a cc.kml point must be to a CWWP2
// checkpoint to be the same checkpoint. Measured: cc.kml points sit 0 m from
// their checkpoint, while the closest distinct checkpoints (TAMARACK East /
// West) are ~250 m apart — direction is matched too.
const sameCheckpointMeters = 200

// mergeKML combines active CWWP2 checkpoints with cc.kml's entries:
//
//   - Road closures and truck-only levels (isKMLSupplement) are always kept:
//     no CWWP2 status for them has been observed.
//   - Any other cc.kml entry at an ACTIVE CWWP2 checkpoint (same spot, same
//     direction) is that checkpoint reported twice, and is dropped.
//   - Any other cc.kml entry is KEPT. Outside the configured districts CWWP2
//     says nothing; inside them the sources disagree, and the safe side of a
//     disagreement is showing the control. An entry sitting on a CWWP2
//     checkpoint that reports R-0 is flagged Uncorroborated (positional, so it
//     works without cc.kml's District field).
//   - An Unrecognized CWWP2 checkpoint with a cc.kml entry at the same spot is
//     dropped — cc.kml says what is there (the pass-closure gates are likely
//     to carry a non-R status all winter, and that must not degrade the layer
//     for five months).
//
// Matching is positional, not by parsing cc.kml's text, so it holds whatever
// layout cc.kml's markup moves to.
func (p *FeedParser) mergeKML(cw, quiet []ChainControlData, incidents []CaltransIncident) []ChainControlData {
	kml := p.parseChainControlDetails(incidents) // one entry per incident, in order

	var out, kept []ChainControlData
	for i, k := range kml {
		k.Source = SourceQuickMap
		if !isKMLSupplement(incidents[i]) {
			if atActiveCheckpoint(k, cw) {
				continue // the same control CWWP2 already reports
			}
			k.Uncorroborated = sameCheckpointAny(k, quiet)
		}
		kept = append(kept, k)
	}
	for _, c := range cw {
		if c.Unrecognized && sameCheckpointAny(c, kept) {
			continue
		}
		out = append(out, c)
	}
	return append(out, kept...)
}

func atActiveCheckpoint(k ChainControlData, cw []ChainControlData) bool {
	for _, c := range cw {
		if !c.Unrecognized && sameCheckpoint(k, c) {
			return true
		}
	}
	return false
}

func sameCheckpointAny(c ChainControlData, kml []ChainControlData) bool {
	for _, k := range kml {
		if sameCheckpoint(k, c) {
			return true
		}
	}
	return false
}

// sameCheckpoint: within sameCheckpointMeters, and the same direction when both
// name one (a cc.kml entry whose direction didn't parse matches either).
func sameCheckpoint(a, b ChainControlData) bool {
	if a.Coordinates == nil || b.Coordinates == nil {
		return false
	}
	if a.Direction != "" && b.Direction != "" && !strings.EqualFold(a.Direction, b.Direction) {
		return false
	}
	return haversineMeters(a.Coordinates.Latitude, a.Coordinates.Longitude, b.Coordinates.Latitude, b.Coordinates.Longitude) <= sameCheckpointMeters
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := p2-p1, (lon2-lon1)*math.Pi/180
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}

var truckLevelRe = regexp.MustCompile(`(?i)\blevel\s+(MAX|MIN|TS)\b|truck chain requirements|screening for chains`)

// isKMLSupplement identifies the cc.kml entries kept whatever CWWP2 says, by
// POSITIVE match only — the 2026 iw-* layout (blank <name>s) defeats any rule
// keyed on parsing a level out of the text:
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
