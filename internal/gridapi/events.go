package gridapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/store"
)

// serveEvents handles GET /v1/events: the store-backed event list with
// filters place, layer (repeatable), status (repeatable, default
// ACTIVE+SCHEDULED via the store), severity_min, since (RFC 3339), and keyset
// pagination (page_size 1..200 default 50, opaque page_token). Order is
// severity DESC, observed_at DESC, id ASC (plan §2.3).
func (s *Service) serveEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	var eq store.EventQuery
	if p := q.Get("place"); p != "" {
		place, err := s.Store.GetPlace(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, fmt.Sprintf("unknown place: %q", p))
			return
		}
		if err != nil {
			internal(ctx, w, err)
			return
		}
		eq.PlaceID = place.GetId()
	}

	layers, err := parseLayers(q["layer"])
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	eq.Layers = layers

	statuses, err := parseStatuses(q["status"])
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	eq.Statuses = statuses

	if v := q.Get("severity_min"); v != "" {
		sev, err := parseSeverity(v)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		eq.MinSeverity = sev
	}
	if v := q.Get("since"); v != "" {
		t, err := parseRFC3339("since", v)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		eq.Since = t
	}
	eq.PageSize, err = parsePageSize(q.Get("page_size"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	eq.PageToken = q.Get("page_token")

	events, next, err := s.Store.QueryEvents(ctx, eq)
	if err != nil {
		if isBadToken(err) {
			badRequest(w, "invalid page_token")
			return
		}
		internal(ctx, w, err)
		return
	}
	stripEventsIO(events, wantEnhancementIO(r))
	writeMessage(w, r, &gridv1.EventList{Events: events, NextPageToken: next}, maxAgeEntities)
}

// serveEvent handles GET /v1/events/{id}: the current revision of one event.
func (s *Service) serveEvent(w http.ResponseWriter, r *http.Request, id string) {
	ev, err := s.Store.GetEvent(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, fmt.Sprintf("unknown event: %q", id))
		return
	}
	if err != nil {
		internal(r.Context(), w, err)
		return
	}
	stripEventsIO([]*gridv1.Event{ev}, wantEnhancementIO(r))
	writeMessage(w, r, ev, maxAgeEntities)
}

// serveEventHistory handles GET /v1/events/{id}/history: the event's
// revisions, newest first, keyset-paginated. Unknown ids 404 (an empty
// history list would be indistinguishable from a real but revision-less
// event, which cannot exist).
func (s *Service) serveEventHistory(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if _, err := s.Store.GetEvent(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, fmt.Sprintf("unknown event: %q", id))
			return
		}
		internal(ctx, w, err)
		return
	}
	q := r.URL.Query()
	pageSize, err := parsePageSize(q.Get("page_size"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	revs, next, err := s.Store.EventHistory(ctx, id, pageSize, q.Get("page_token"))
	if err != nil {
		if isBadToken(err) {
			badRequest(w, "invalid page_token")
			return
		}
		internal(ctx, w, err)
		return
	}
	stripRevisionsIO(revs, wantEnhancementIO(r))
	writeMessage(w, r, &gridv1.EventRevisionList{Revisions: revs, NextPageToken: next}, maxAgeEntities)
}

// serveHistory handles GET /v1/history: revisions across all events filtered
// by place, layer (repeatable) and a half-open [from, to) window over
// observed_at.
func (s *Service) serveHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	var hq store.HistoryQuery
	if p := q.Get("place"); p != "" {
		place, err := s.Store.GetPlace(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			notFound(w, fmt.Sprintf("unknown place: %q", p))
			return
		}
		if err != nil {
			internal(ctx, w, err)
			return
		}
		hq.PlaceID = place.GetId()
	}
	layers, err := parseLayers(q["layer"])
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	hq.Layers = layers
	if v := q.Get("from"); v != "" {
		if hq.From, err = parseRFC3339("from", v); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if hq.To, err = parseRFC3339("to", v); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	if hq.PageSize, err = parsePageSize(q.Get("page_size")); err != nil {
		badRequest(w, err.Error())
		return
	}
	hq.PageToken = q.Get("page_token")

	revs, next, err := s.Store.QueryHistory(ctx, hq)
	if err != nil {
		if isBadToken(err) {
			badRequest(w, "invalid page_token")
			return
		}
		internal(ctx, w, err)
		return
	}
	stripRevisionsIO(revs, wantEnhancementIO(r))
	writeMessage(w, r, &gridv1.EventRevisionList{Revisions: revs, NextPageToken: next}, maxAgeEntities)
}

// wantEnhancementIO reports whether the request opted into the large model I/O
// fields (enhancement.request/response). Default false — they are omitted so
// list responses stay lean; opt in with ?enhancement_io=true (or 1). The
// lightweight provenance (model, enhanced_at, fields) is always kept.
func wantEnhancementIO(r *http.Request) bool {
	switch strings.ToLower(r.URL.Query().Get("enhancement_io")) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// stripEventsIO clears enhancement.request/response on each event unless kept.
// Mutates in place — safe because the store returns freshly unmarshaled events
// per query (never shared with the caching layer).
func stripEventsIO(events []*gridv1.Event, keep bool) {
	if keep {
		return
	}
	for _, ev := range events {
		if e := ev.GetEnhancement(); e != nil {
			e.Request = ""
			e.Response = ""
		}
	}
}

// stripRevisionsIO is stripEventsIO over a revision list.
func stripRevisionsIO(revs []*gridv1.EventRevision, keep bool) {
	if keep {
		return
	}
	for _, rev := range revs {
		if e := rev.GetEvent().GetEnhancement(); e != nil {
			e.Request = ""
			e.Response = ""
		}
	}
}

// --- query-param parsers (shared by events, history, places) ---

// parseLayers accepts repeated layer params; each value is an enum name
// matched case-insensitively, which also covers the shipped lowercase layer
// slugs ("wildfire", "road_incident") since those uppercase onto the enum
// names exactly. Comma-separated lists inside one param are accepted too.
// LAYER_UNSPECIFIED and unknown values are rejected.
func parseLayers(vals []string) ([]gridv1.Layer, error) {
	var out []gridv1.Layer
	for _, raw := range vals {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			n, ok := gridv1.Layer_value[strings.ToUpper(v)]
			if !ok || n == int32(gridv1.Layer_LAYER_UNSPECIFIED) {
				return nil, fmt.Errorf("unknown layer: %q", v)
			}
			out = append(out, gridv1.Layer(n))
		}
	}
	return out, nil
}

// parseStatuses accepts repeated status params (enum names,
// case-insensitive). Empty input returns nil, which the store defaults to
// ACTIVE+SCHEDULED — the "what's happening now" read.
func parseStatuses(vals []string) ([]gridv1.EventStatus, error) {
	var out []gridv1.EventStatus
	for _, raw := range vals {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			n, ok := gridv1.EventStatus_value[strings.ToUpper(v)]
			if !ok || n == int32(gridv1.EventStatus_EVENT_STATUS_UNSPECIFIED) {
				return nil, fmt.Errorf("unknown status: %q", v)
			}
			out = append(out, gridv1.EventStatus(n))
		}
	}
	return out, nil
}

// parseSeverity maps a severity name (case-insensitive) onto the enum.
// "INFO" is valid and means no minimum.
func parseSeverity(v string) (gridv1.Severity, error) {
	n, ok := gridv1.Severity_value[strings.ToUpper(strings.TrimSpace(v))]
	if !ok {
		return 0, fmt.Errorf("unknown severity_min: %q", v)
	}
	return gridv1.Severity(n), nil
}

// parseRFC3339 parses one timestamp param, naming it in the error.
func parseRFC3339(name, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: %q is not RFC 3339", name, v)
	}
	return t, nil
}

// parsePageSize enforces the documented 1..200 window; "" means the store
// default (50).
func parsePageSize(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 200 {
		return 0, fmt.Errorf("page_size must be an integer in 1..200, got %q", v)
	}
	return n, nil
}

// isBadToken distinguishes a client-supplied garbage page_token (400) from a
// genuine store failure (500). The store wraps token decode failures with
// this stable marker; the cursor type itself is unexported there, so a
// message probe is the seam we have.
func isBadToken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid page token")
}
