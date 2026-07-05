package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gridv1 "github.com/dpup/info.ersn.net/server/api/grid/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// EventQuery filters QueryEvents. Zero values mean "no filter", except
// Statuses, which defaults to ACTIVE+SCHEDULED (the "what's happening now"
// read) when empty.
type EventQuery struct {
	PlaceID     string
	Layers      []gridv1.Layer
	Statuses    []gridv1.EventStatus
	MinSeverity gridv1.Severity
	Since       time.Time // observed_at >= Since
	PageSize    int       // default 50, max 200
	PageToken   string
}

// HistoryQuery filters QueryHistory over event_revisions. The time window is
// half-open: observed_at >= From AND observed_at < To (zero bound = open).
type HistoryQuery struct {
	PlaceID   string
	Layers    []gridv1.Layer
	From, To  time.Time
	PageSize  int
	PageToken string
}

// eventCursor is the keyset for QueryEvents' ordering
// (severity DESC, observed_at DESC, id ASC).
type eventCursor struct {
	S  int32  `json:"s"`
	O  int64  `json:"o"`
	ID string `json:"id"`
}

// historyCursor is the keyset for QueryHistory's ordering
// (observed_at DESC, event_id ASC, revision DESC). EventHistory reuses R.
type historyCursor struct {
	O  int64  `json:"o,omitempty"`
	ID string `json:"id,omitempty"`
	R  uint32 `json:"r"`
}

func encodeToken(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeToken(token string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("store: invalid page token: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("store: invalid page token: %w", err)
	}
	return nil
}

func clampPageSize(n int) int {
	switch {
	case n <= 0:
		return defaultPageSize
	case n > maxPageSize:
		return maxPageSize
	default:
		return n
	}
}

// placeholders returns "?, ?, ..." for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// QueryEvents returns one page of events ordered severity DESC,
// observed_at DESC, id ASC, plus the next page token ("" on the last page).
// Pagination is keyset-based, so pages stay stable and complete while new
// events arrive. All values are bound via placeholders.
func (s *Store) QueryEvents(ctx context.Context, q EventQuery) ([]*gridv1.Event, string, error) {
	pageSize := clampPageSize(q.PageSize)
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED}
	}

	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT e.proto, e.severity, e.observed_at, e.id FROM events e`)
	if q.PlaceID != "" {
		sb.WriteString(` JOIN event_places ep ON ep.event_id = e.id AND ep.place_id = ?`)
		args = append(args, q.PlaceID)
	}
	sb.WriteString(` WHERE e.status IN (` + placeholders(len(statuses)) + `)`)
	for _, st := range statuses {
		args = append(args, int32(st))
	}
	if len(q.Layers) > 0 {
		sb.WriteString(` AND e.layer IN (` + placeholders(len(q.Layers)) + `)`)
		for _, l := range q.Layers {
			args = append(args, int32(l))
		}
	}
	if q.MinSeverity > gridv1.Severity_INFO {
		sb.WriteString(` AND e.severity >= ?`)
		args = append(args, int32(q.MinSeverity))
	}
	if !q.Since.IsZero() {
		sb.WriteString(` AND e.observed_at >= ?`)
		args = append(args, q.Since.Unix())
	}
	if q.PageToken != "" {
		var cur eventCursor
		if err := decodeToken(q.PageToken, &cur); err != nil {
			return nil, "", err
		}
		sb.WriteString(` AND (e.severity < ? OR (e.severity = ? AND e.observed_at < ?)
			OR (e.severity = ? AND e.observed_at = ? AND e.id > ?))`)
		args = append(args, cur.S, cur.S, cur.O, cur.S, cur.O, cur.ID)
	}
	sb.WriteString(` ORDER BY e.severity DESC, e.observed_at DESC, e.id ASC LIMIT ?`)
	args = append(args, pageSize+1) // +1 probes for a next page

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: query events: %w", err)
	}
	defer rows.Close()

	var events []*gridv1.Event
	var last eventCursor
	for rows.Next() {
		var blob []byte
		var cur eventCursor
		if err := rows.Scan(&blob, &cur.S, &cur.O, &cur.ID); err != nil {
			return nil, "", fmt.Errorf("store: scan event: %w", err)
		}
		if len(events) == pageSize {
			return events, encodeToken(last), rows.Err()
		}
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return nil, "", fmt.Errorf("store: unmarshal event %s: %w", cur.ID, err)
		}
		events = append(events, ev)
		last = cur
	}
	return events, "", rows.Err()
}

// EventHistory returns an event's revisions, newest first, with keyset
// pagination on revision number.
func (s *Store) EventHistory(ctx context.Context, id string, pageSize int, token string) ([]*gridv1.EventRevision, string, error) {
	pageSize = clampPageSize(pageSize)
	var sb strings.Builder
	args := []any{id}
	sb.WriteString(`SELECT revision, observed_at, ingested_at, proto FROM event_revisions WHERE event_id = ?`)
	if token != "" {
		var cur historyCursor
		if err := decodeToken(token, &cur); err != nil {
			return nil, "", err
		}
		sb.WriteString(` AND revision < ?`)
		args = append(args, cur.R)
	}
	sb.WriteString(` ORDER BY revision DESC LIMIT ?`)
	args = append(args, pageSize+1)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: event history %s: %w", id, err)
	}
	defer rows.Close()

	var revs []*gridv1.EventRevision
	for rows.Next() {
		rev, err := scanRevision(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		if len(revs) == pageSize {
			return revs, encodeToken(historyCursor{R: revs[pageSize-1].GetRevision()}), rows.Err()
		}
		revs = append(revs, rev)
	}
	return revs, "", rows.Err()
}

// QueryHistory returns one page of revisions across events, ordered
// observed_at DESC, event_id ASC, revision DESC. Layer and place filters
// join the current events row — event_places reflects current attachments,
// not per-revision snapshots (documented tradeoff: places are recomputed at
// ingest, and revision-time re-matching would need per-revision geometry).
func (s *Store) QueryHistory(ctx context.Context, q HistoryQuery) ([]*gridv1.EventRevision, string, error) {
	pageSize := clampPageSize(q.PageSize)

	var sb strings.Builder
	var args []any
	sb.WriteString(`SELECT r.revision, r.observed_at, r.ingested_at, r.proto, r.event_id
		FROM event_revisions r JOIN events e ON e.id = r.event_id`)
	if q.PlaceID != "" {
		sb.WriteString(` JOIN event_places ep ON ep.event_id = r.event_id AND ep.place_id = ?`)
		args = append(args, q.PlaceID)
	}
	sb.WriteString(` WHERE 1=1`)
	if len(q.Layers) > 0 {
		sb.WriteString(` AND e.layer IN (` + placeholders(len(q.Layers)) + `)`)
		for _, l := range q.Layers {
			args = append(args, int32(l))
		}
	}
	if !q.From.IsZero() {
		sb.WriteString(` AND r.observed_at >= ?`)
		args = append(args, q.From.Unix())
	}
	if !q.To.IsZero() {
		sb.WriteString(` AND r.observed_at < ?`)
		args = append(args, q.To.Unix())
	}
	if q.PageToken != "" {
		var cur historyCursor
		if err := decodeToken(q.PageToken, &cur); err != nil {
			return nil, "", err
		}
		sb.WriteString(` AND (r.observed_at < ? OR (r.observed_at = ? AND r.event_id > ?)
			OR (r.observed_at = ? AND r.event_id = ? AND r.revision < ?))`)
		args = append(args, cur.O, cur.O, cur.ID, cur.O, cur.ID, cur.R)
	}
	sb.WriteString(` ORDER BY r.observed_at DESC, r.event_id ASC, r.revision DESC LIMIT ?`)
	args = append(args, pageSize+1)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: query history: %w", err)
	}
	defer rows.Close()

	var revs []*gridv1.EventRevision
	var last historyCursor
	for rows.Next() {
		var eventID string
		rev, err := scanRevision(func(dest ...any) error {
			return rows.Scan(append(dest, &eventID)...)
		})
		if err != nil {
			return nil, "", err
		}
		if len(revs) == pageSize {
			return revs, encodeToken(last), rows.Err()
		}
		revs = append(revs, rev)
		last = historyCursor{O: rev.GetObservedAt().AsTime().Unix(), ID: eventID, R: rev.GetRevision()}
	}
	return revs, "", rows.Err()
}

// scanRevision builds an EventRevision from (revision, observed_at,
// ingested_at, proto) columns via the given scan function.
func scanRevision(scan func(...any) error) (*gridv1.EventRevision, error) {
	var revision uint32
	var observedAt, ingestedAt int64
	var blob []byte
	if err := scan(&revision, &observedAt, &ingestedAt, &blob); err != nil {
		return nil, fmt.Errorf("store: scan revision: %w", err)
	}
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		return nil, fmt.Errorf("store: unmarshal revision: %w", err)
	}
	return &gridv1.EventRevision{
		Revision:   revision,
		ObservedAt: timestamppb.New(time.Unix(observedAt, 0)),
		IngestedAt: timestamppb.New(time.Unix(ingestedAt, 0)),
		Event:      ev,
	}, nil
}
