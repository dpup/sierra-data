package hazards

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/dpup/sierra-data/internal/clients/caltrans"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/config"
)

// MessageSignAPI is the slice of *cwwp2.Client the message_sign layer uses.
type MessageSignAPI interface {
	MessageSigns(ctx context.Context, district int) ([]cwwp2.MessageSign, error)
}

// UseMessageSigns sets the source behind the message_sign layer: the CWWP2
// changeable message signs of the given districts. Without it the layer is
// UNAVAILABLE. It has no other source, so "no signs" is never a default.
func (s *Service) UseMessageSigns(src MessageSignAPI, districts []int) {
	s.signs = src
	s.signDistricts = append([]int(nil), districts...)
}

// messageSigns projects every in-area changeable message sign with what it is
// showing.
//
// The layer is CONTEXT, never a hazard. Every feature is INFO, and /summary
// doesn't read the layer. On 2026-10-01, 82 of District 10's 107 signs showed
// one statewide safety campaign, and the campaign rotates (the day before it
// was a work-zone one), so no fixed list can filter it. A map that shows the
// boilerplate shows what drivers see. Deciding which messages matter waits on
// a storm capture (tests/testdata/cwwp2/README.md).
//
// Fail-loud:
//   - Any district's fetch failing fails the layer. A missing district would
//     read as "no signs" across its footprint.
//   - A sign with no position degrades the layer to STALE: it can't be ruled
//     in or out of the area.
//   - A sign whose message can't be read is NOT a degradation. It is listed
//     with category "unknown", which says exactly that.
func (s *Service) messageSigns(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	if s.signs == nil || len(s.signDistricts) == 0 {
		return nil, errors.New("message signs: no CWWP2 districts configured (roads.caltransFeeds.cwwp2.messageSignDistricts)")
	}
	var out []Feature
	var unplaced []string
	for _, d := range s.signDistricts {
		signs, err := s.signs.MessageSigns(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("cwwp2 message signs: %w", err)
		}
		seen := make(map[string]int, len(signs))
		for _, sg := range signs {
			seen[sg.ID]++
		}
		for _, sg := range signs {
			if !sg.Location.HasPosition {
				unplaced = append(unplaced, strings.TrimSpace(fmt.Sprintf("district %d sign %q %s", d, sg.ID, sg.Location.Name)))
				continue
			}
			if !area.Bounds.Contains(sg.Location.Latitude, sg.Location.Longitude) {
				continue
			}
			out = append(out, messageSignFeature(d, sg, seen[sg.ID] == 1))
		}
	}
	if len(unplaced) > 0 {
		return out, partialData(fmt.Errorf("message signs without a position: %s", strings.Join(unplaced, "; ")))
	}
	return out, nil
}

// signNumberRe matches the sign number District 10 leads every name with
// ("42 - EB 108 SOULSBYVILLE"). The number is already the sign's id.
var signNumberRe = regexp.MustCompile(`^\d+\s*-\s*`)

// messageSignFeature builds one sign's feature. uniqueID reports whether the
// portal index is unique in its district's file. Where it isn't (D12 numbers
// every sign "1"), the id falls back to the sign's position.
func messageSignFeature(district int, sg cwwp2.MessageSign, uniqueID bool) Feature {
	lat, lng := sg.Location.Latitude, sg.Location.Longitude
	key := sg.ID
	if !uniqueID || key == "" || strings.EqualFold(key, "N/A") {
		key = fmt.Sprintf("%.5f,%.5f", lat, lng)
	}

	category, headline := "message", sg.Text()
	switch {
	case sg.Display == cwwp2.DisplayBlank:
		category, headline = "blank", "Sign is blank"
	case sg.Display == cwwp2.DisplayUnknown && !sg.InService:
		category, headline = "unknown", "Sign out of service"
	case sg.Display == cwwp2.DisplayUnknown:
		category, headline = "unknown", "Sign message unknown"
	}

	p := Properties{
		ID:        fmt.Sprintf("cms:%d:%s", district, key),
		Layer:     strings.ToUpper(LayerMessageSign),
		Kind:      "Message sign",
		Category:  category,
		Headline:  headline,
		AreaLabel: signNumberRe.ReplaceAllString(sg.Location.Name, ""),
		// When the sign last changed message. Zero (omitted) where the portal
		// doesn't report it.
		Effective: rfc3339Time(sg.MessageSince),
		Source:    Source{ID: "caltrans", Name: "Caltrans", Attribution: caltrans.SourceCWWP2},
		MessageSign: &MessageSignProps{
			SignID:    sg.ID,
			District:  district,
			Route:     sg.Location.Route,
			Direction: sg.Location.Direction,
			InService: sg.InService,
			Pages:     sg.Pages(),
		},
	}
	p.setSeverity(SevInfo)
	return Feature{Type: "Feature", Geometry: PointGeom(lat, lng), Properties: p}
}
