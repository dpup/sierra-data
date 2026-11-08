package gridapi

import (
	"context"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Mesh activity is the one part of a MESH event served from outside its blob.
//
// telemetry.last_heard_at / last_relayed_at move every few seconds, and the
// blob is canonical: keeping them there would rewrite every node's proto on
// every tick. So the ingest tick writes them to the store's mesh_activity table
// and every read path that serves a MESH event stamps them on here. Paths that
// serve the CURRENT event call overlayMeshActivity; revision history does not,
// because a revision is a snapshot of what was stored then.
//
// A failed activity read degrades to no overlay (the fields stay null, which
// reads as "unknown") rather than failing the request: these are ambient
// display fields and must never take the event list down with them.

// meshActivityPrefix is the id prefix of a MESH node event (the meshcore source).
const meshActivityPrefix = "meshcore:"

// meshActivityKey is the key a MESH event's activity is filed under, or "".
func meshActivityKey(ev *gridv1.Event) string {
	if ev.GetLayer() != gridv1.Layer_MESH {
		return ""
	}
	return strings.ToLower(ev.GetMesh().GetPublicKey())
}

// overlayMeshActivity stamps each MESH event's activity clocks onto its
// telemetry. Mutates in place: the store returns freshly unmarshaled events
// per query, never shared ones.
func (s *Service) overlayMeshActivity(ctx context.Context, events []*gridv1.Event) {
	var keys []string
	for _, ev := range events {
		if k := meshActivityKey(ev); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	act, err := s.Store.MeshActivityFor(ctx, keys)
	if err != nil {
		logError(ctx, "gridapi: reading mesh activity", err)
		return
	}
	applyMeshActivity(events, act)
}

func applyMeshActivity(events []*gridv1.Event, act map[string]store.MeshActivity) {
	for _, ev := range events {
		a, ok := act[meshActivityKey(ev)]
		if !ok {
			continue
		}
		d := ev.GetMesh()
		if d.GetTelemetry() == nil {
			d.Telemetry = &gridv1.MeshTelemetry{}
		}
		if !a.LastHeardAt.IsZero() {
			d.Telemetry.LastHeardAt = timestamppb.New(a.LastHeardAt)
		}
		if !a.LastRelayedAt.IsZero() {
			d.Telemetry.LastRelayedAt = timestamppb.New(a.LastRelayedAt)
		}
	}
}

// layersIncludeMesh reports whether an event query can return MESH events (no
// layer filter means every layer). Validators for such a query must fold in
// the activity version: the overlay changes the body without a revision.
func layersIncludeMesh(layers []gridv1.Layer) bool {
	if len(layers) == 0 {
		return true
	}
	for _, l := range layers {
		if l == gridv1.Layer_MESH {
			return true
		}
	}
	return false
}

// meshActivityTag renders one node's activity as an ETag input.
func meshActivityTag(a store.MeshActivity) string {
	return strconv.FormatInt(unixOrZero(a.LastHeardAt), 10) + "." + strconv.FormatInt(unixOrZero(a.LastRelayedAt), 10)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
