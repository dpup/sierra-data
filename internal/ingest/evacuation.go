package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/dpup/prefab/logging"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"github.com/dpup/info.ersn.net/server/internal/clients/caloes"
	"github.com/dpup/info.ersn.net/server/internal/config"
	"github.com/dpup/info.ersn.net/server/internal/hazards"
)

// EvacuationNormalizer ingests Cal OES evacuation zones (id namespace
// "evac:") over the union bbox of the configured hazard areas. The Cal OES
// aggregation layer is active-events-only; explicitly-inactive statuses
// (lifted/all-clear) are dropped, and an unrecognized active status
// conservatively classifies as WARNING (life-safety bias, same as the
// shipped builder).
type EvacuationNormalizer struct {
	cfg    *config.Config
	caloes *caloes.Client
}

// NewEvacuationNormalizer wires the normalizer to a Cal OES client (tests
// inject one built with caloes.NewClientWithHTTPDoer).
func NewEvacuationNormalizer(cfg *config.Config, client *caloes.Client) *EvacuationNormalizer {
	return &EvacuationNormalizer{cfg: cfg, caloes: client}
}

// SourceIDs implements Normalizer.
func (n *EvacuationNormalizer) SourceIDs() []string { return []string{"caloes"} }

// Poll implements Normalizer. A Cal OES failure is a hard error — the caller
// must surface UNAVAILABLE/unknown, never an empty all-clear.
func (n *EvacuationNormalizer) Poll(ctx context.Context) (*PollResult, error) {
	minLat, minLng, maxLat, maxLng, ok := unionBounds(n.cfg.Hazards.Areas)
	if !ok {
		return &PollResult{}, nil
	}
	zones, err := n.caloes.GetActiveEvacuations(ctx, caloes.Bounds{
		MinLatitude:  minLat,
		MaxLatitude:  maxLat,
		MinLongitude: minLng,
		MaxLongitude: maxLng,
	})
	if err != nil {
		return nil, err
	}

	var events []*gridv1.Event
	for _, z := range zones {
		level := hazards.NormalizeEvacLevel(z.Status)
		if level == "" {
			continue // only surface active Order/Warning/Advisory/SIP zones
		}
		if !hazards.EvacStatusRecognized(z.Status) {
			// Conservatively classified as WARNING by the fail-loud default. Log
			// so the phrasing can be added to normalizeEvacLevel explicitly.
			logging.Warnw(ctx, "Unrecognized Cal OES evacuation STATUS; defaulted to WARNING",
				"status", z.Status, "zone", nonEmpty(z.ZoneID, z.ZoneName), "county", z.County)
		}

		ev := NewEvent(
			"evac:"+nonEmpty(z.ZoneID, z.ZoneName),
			gridv1.Layer_EVACUATION,
			SeverityFromLabel(hazards.SeverityFromEvacLevel(level)),
			gridv1.EventStatus_ACTIVE,
			fmt.Sprintf("Evacuation %s — %s", humanEvacLevel(level), nonEmpty(z.ZoneName, z.County)), // shipped headline format
		)
		ev.Category = strings.ToLower(level)
		// Life-safety: PublicInfo is directive text and is carried VERBATIM.
		// Enhancement may add context around it, never a paraphrase of it
		// (spec §3.1 policy 4 — we don't paraphrase orders).
		ev.Description = z.PublicInfo
		ev.AreaLabel = nonEmpty(z.ZoneName, z.County)
		ev.ObservedAt = tsProto(z.LastUpdated)
		ev.Provenance = NewProvenance("caloes", "Cal OES", "Cal OES — reference only", caloes.SourceURL)
		ev.Detail = &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{
			ZoneId:    z.ZoneID,
			Level:     level,
			EventType: z.EventType,
			County:    z.County,
		}}
		if geom, gerr := geometryFromTyped(z.GeometryType, z.GeometryCoords); gerr == nil {
			ev.Geometry = geom
		} else {
			// Never drop an active zone over bad geometry — emit it as a
			// geometry-less banner event (valid per the model).
			logging.Warnw(ctx, "Cal OES zone has unusable geometry; emitting without geometry",
				"zone", nonEmpty(z.ZoneID, z.ZoneName), "error", gerr)
		}
		events = append(events, ev)
	}
	return &PollResult{Events: events}, nil
}

// humanEvacLevel renders a coded level as the title-cased human phrase used
// in the shipped headline, e.g. "SHELTER_IN_PLACE" → "Shelter In Place".
func humanEvacLevel(level string) string {
	words := strings.Fields(strings.ToLower(strings.ReplaceAll(level, "_", " ")))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
