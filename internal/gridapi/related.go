package gridapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/dpup/prefab/plugins/etag"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/related"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// ListRelatedEvents returns the events related to one event (internal/related).
func (g *GridServer) ListRelatedEvents(ctx context.Context, req *gridv1.ListRelatedEventsRequest) (*gridv1.RelatedEventList, error) {
	if _, ok, err := g.svc.Store.EventVersion(ctx, req.GetId()); err != nil {
		return nil, internalErr(ctx, err)
	} else if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	// Any stored change can add or end a relation, so the tag follows the
	// whole store's data version (revisions plus in-place rewrites).
	dv, err := g.svc.Store.DataVersion(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	rv, err := g.svc.Store.RefreshVersion(ctx, relatedLayers)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	tag := fmt.Sprintf("%s:related:%s:%d.%s", g.placesVersion, req.GetId(), dv, strconv.FormatInt(rv, 10))
	if err := etag.Guard(ctx, etag.Weak(tag)); err != nil {
		return nil, err
	}
	subject, err := g.svc.Store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	eng := related.New(g.svc.Store)
	eng.Now = g.svc.Now
	rels, err := eng.Related(ctx, subject)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	out := &gridv1.RelatedEventList{EventId: subject.GetId()}
	for _, r := range rels {
		stripEventsIO([]*gridv1.Event{r.Event}, false)
		out.Related = append(out.Related, &gridv1.RelatedEvent{
			Event: r.Event, Rule: r.Rule, Reason: r.Reason,
			Confidence: confidenceProto(r.Confidence),
		})
	}
	return out, nil
}

func confidenceProto(c related.Confidence) gridv1.RelationConfidence {
	switch c {
	case related.High:
		return gridv1.RelationConfidence_RELATION_HIGH
	case related.Medium:
		return gridv1.RelationConfidence_RELATION_MEDIUM
	case related.Low:
		return gridv1.RelationConfidence_RELATION_LOW
	case related.Context:
		return gridv1.RelationConfidence_RELATION_CONTEXT
	}
	return gridv1.RelationConfidence_RELATION_CONFIDENCE_UNSPECIFIED
}

// relatedLayers are the layers the rules read; mesh refreshes do not move
// the validator.
var relatedLayers = []gridv1.Layer{gridv1.Layer_WILDFIRE, gridv1.Layer_EVACUATION, gridv1.Layer_POWER,
	gridv1.Layer_WEATHER_ALERT, gridv1.Layer_ROAD_INCIDENT}
