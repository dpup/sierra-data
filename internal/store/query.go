package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultPageSize is what a page_size of 0 (unset) gets; MaxPageSize is the
	// ceiling a larger request is clamped to.
	DefaultPageSize = 50
	MaxPageSize     = 200
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
	// Lite reads each event's geometry-free copy (event_lite): geometry
	// carries bbox and centroid but no GeoJSON. For lists that do not draw
	// shapes; see migrationV12 for why it matters.
	Lite bool
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
		return DefaultPageSize
	case n > MaxPageSize:
		return MaxPageSize
	default:
		return n
	}
}

// placeholders returns "?, ?, ..." for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// Page is one page of a keyset-paginated query, with a resume token per row,
// so a caller can serve fewer rows than it fetched (gridapi cuts a page at a
// byte budget) without breaking the walk.
type Page[T any] struct {
	Items []T
	// After[i] is the page token that resumes immediately after Items[i].
	After []string
	// More reports whether rows exist beyond the last item.
	More bool
}

// Next is the next-page token when the first n items are served: "" only when
// those n items end the result set. n is clamped to [1, len(Items)].
func (p Page[T]) Next(n int) string {
	if len(p.Items) == 0 {
		return ""
	}
	n = min(max(n, 1), len(p.Items))
	if n == len(p.Items) && !p.More {
		return ""
	}
	return p.After[n-1]
}

// QueryEvents returns one page of events ordered severity DESC,
// observed_at DESC, id ASC, plus the next page token ("" on the last page).
// Pagination is keyset-based, so pages stay stable and complete while new
// events arrive. All values are bound via placeholders.
func (s *Store) QueryEvents(ctx context.Context, q EventQuery) ([]*gridv1.Event, string, error) {
	p, err := s.QueryEventsPage(ctx, q)
	if err != nil {
		return nil, "", err
	}
	return p.Items, p.Next(len(p.Items)), nil
}

// QueryEventsPage is QueryEvents with a resume token for every row.
func (s *Store) QueryEventsPage(ctx context.Context, q EventQuery) (Page[*gridv1.Event], error) {
	pageSize := clampPageSize(q.PageSize)
	query, args, err := eventsPageSQL(q, pageSize)
	if err != nil {
		return Page[*gridv1.Event]{}, err
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page[*gridv1.Event]{}, fmt.Errorf("store: query events: %w", err)
	}
	defer rows.Close()

	var page Page[*gridv1.Event]
	for rows.Next() {
		var blob []byte
		var cur eventCursor
		if err := rows.Scan(&blob, &cur.S, &cur.O, &cur.ID); err != nil {
			return Page[*gridv1.Event]{}, fmt.Errorf("store: scan event: %w", err)
		}
		if len(page.Items) == pageSize {
			page.More = true
			break
		}
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return Page[*gridv1.Event]{}, fmt.Errorf("store: unmarshal event %s: %w", cur.ID, err)
		}
		page.Items = append(page.Items, ev)
		page.After = append(page.After, encodeToken(cur))
	}
	if err := rows.Err(); err != nil {
		return Page[*gridv1.Event]{}, fmt.Errorf("store: query events: %w", err)
	}
	return page, nil
}

// eventsPageSQL builds QueryEvents' statement, which returns pageSize+1 rows
// (the extra one probes for a next page).
//
// It is two-phase on purpose. The keyset order (severity, observed_at, id)
// comes from an index only when a single status is asked for; any status
// list, the default ACTIVE,SCHEDULED included, sorts. Sorting `SELECT
// e.proto, ...` copies the proto blob of EVERY matching row into the sorter
// to keep 201 of them, so the cost followed the size of the whole matching
// set, perimeters included, not the page: in production an all-statuses page
// took 25-40 s whether pageSize was 5 or 200 (2026-11-07). The inner query
// sorts only key columns, which sit ahead of the blob in the row and never
// reach its overflow pages; the outer join reads just the page's blobs, by
// primary key. Holding a read that long is what let a deploy's drain cancel
// it mid-query (the 500 that gridapi.internalErr now reports as a 503).
func eventsPageSQL(q EventQuery, pageSize int) (string, []any, error) {
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED}
	}

	var sb strings.Builder
	var args []any
	blobCol := "e.proto"
	if q.Lite {
		blobCol = "COALESCE(l.proto, e.proto)"
	}
	sb.WriteString(`SELECT ` + blobCol + `, k.severity, k.observed_at, k.id FROM (` +
		`SELECT e.id, e.severity, e.observed_at FROM events e`)
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
			return "", nil, err
		}
		sb.WriteString(` AND (e.severity < ? OR (e.severity = ? AND e.observed_at < ?)
			OR (e.severity = ? AND e.observed_at = ? AND e.id > ?))`)
		args = append(args, cur.S, cur.S, cur.O, cur.S, cur.O, cur.ID)
	}
	sb.WriteString(` ORDER BY e.severity DESC, e.observed_at DESC, e.id ASC LIMIT ?) k` +
		` JOIN events e ON e.id = k.id`)
	if q.Lite {
		// COALESCE short-circuits, so e.proto is read only for an event the
		// backfill has not reached yet.
		sb.WriteString(` LEFT JOIN event_lite l ON l.event_id = k.id`)
	}
	sb.WriteString(` ORDER BY k.severity DESC, k.observed_at DESC, k.id ASC`)
	args = append(args, pageSize+1)
	return sb.String(), args, nil
}

// EventHistory returns an event's revisions, newest first, with keyset
// pagination on revision number.
func (s *Store) EventHistory(ctx context.Context, id string, pageSize int, token string) ([]*gridv1.EventRevision, string, error) {
	p, err := s.EventHistoryPage(ctx, id, pageSize, token)
	if err != nil {
		return nil, "", err
	}
	return p.Items, p.Next(len(p.Items)), nil
}

// EventHistoryPage is EventHistory with a resume token for every row.
func (s *Store) EventHistoryPage(ctx context.Context, id string, pageSize int, token string) (Page[*gridv1.EventRevision], error) {
	pageSize = clampPageSize(pageSize)
	var sb strings.Builder
	args := []any{id}
	sb.WriteString(`SELECT revision, observed_at, ingested_at, proto FROM event_revisions WHERE event_id = ?`)
	if token != "" {
		var cur historyCursor
		if err := decodeToken(token, &cur); err != nil {
			return Page[*gridv1.EventRevision]{}, err
		}
		sb.WriteString(` AND revision < ?`)
		args = append(args, cur.R)
	}
	sb.WriteString(` ORDER BY revision DESC LIMIT ?`)
	args = append(args, pageSize+1)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return Page[*gridv1.EventRevision]{}, fmt.Errorf("store: event history %s: %w", id, err)
	}
	defer rows.Close()

	var page Page[*gridv1.EventRevision]
	for rows.Next() {
		if len(page.Items) == pageSize {
			page.More = true
			break
		}
		rev, err := scanRevision(rows.Scan)
		if err != nil {
			return Page[*gridv1.EventRevision]{}, err
		}
		page.Items = append(page.Items, rev)
		page.After = append(page.After, encodeToken(historyCursor{R: rev.GetRevision()}))
	}
	if err := rows.Err(); err != nil {
		return Page[*gridv1.EventRevision]{}, fmt.Errorf("store: event history %s: %w", id, err)
	}
	return page, nil
}

// QueryHistory returns one page of revisions across events, ordered
// observed_at DESC, event_id ASC, revision DESC. Layer and place filters
// join the current events row — event_places reflects current attachments,
// not per-revision snapshots (documented tradeoff: places are recomputed at
// ingest, and revision-time re-matching would need per-revision geometry).
func (s *Store) QueryHistory(ctx context.Context, q HistoryQuery) ([]*gridv1.EventRevision, string, error) {
	p, err := s.QueryHistoryPage(ctx, q)
	if err != nil {
		return nil, "", err
	}
	return p.Items, p.Next(len(p.Items)), nil
}

// QueryHistoryPage is QueryHistory with a resume token for every row.
func (s *Store) QueryHistoryPage(ctx context.Context, q HistoryQuery) (Page[*gridv1.EventRevision], error) {
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
			return Page[*gridv1.EventRevision]{}, err
		}
		sb.WriteString(` AND (r.observed_at < ? OR (r.observed_at = ? AND r.event_id > ?)
			OR (r.observed_at = ? AND r.event_id = ? AND r.revision < ?))`)
		args = append(args, cur.O, cur.O, cur.ID, cur.O, cur.ID, cur.R)
	}
	sb.WriteString(` ORDER BY r.observed_at DESC, r.event_id ASC, r.revision DESC LIMIT ?`)
	args = append(args, pageSize+1)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return Page[*gridv1.EventRevision]{}, fmt.Errorf("store: query history: %w", err)
	}
	defer rows.Close()

	var page Page[*gridv1.EventRevision]
	for rows.Next() {
		if len(page.Items) == pageSize {
			page.More = true
			break
		}
		var eventID string
		rev, err := scanRevision(func(dest ...any) error {
			return rows.Scan(append(dest, &eventID)...)
		})
		if err != nil {
			return Page[*gridv1.EventRevision]{}, err
		}
		page.Items = append(page.Items, rev)
		page.After = append(page.After, encodeToken(historyCursor{
			O: rev.GetObservedAt().AsTime().Unix(), ID: eventID, R: rev.GetRevision(),
		}))
	}
	if err := rows.Err(); err != nil {
		return Page[*gridv1.EventRevision]{}, fmt.Errorf("store: query history: %w", err)
	}
	return page, nil
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

// EventRevisionAt returns one revision of an event, or ErrNotFound.
func (s *Store) EventRevisionAt(ctx context.Context, id string, revision uint32) (*gridv1.EventRevision, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT revision, observed_at, ingested_at, proto FROM event_revisions WHERE event_id = ? AND revision = ?`, id, revision)
	rev, err := scanRevision(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rev, err
}

// FirstIngested returns when each event's revision 1 was stored, for the ids
// that have one. It reads only the revisions' leading columns, never a blob.
func (s *Store) FirstIngested(ctx context.Context, ids []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(ids))
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT event_id, ingested_at FROM event_revisions
			WHERE revision = 1 AND event_id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("store: first ingested: %w", err)
		}
		for rows.Next() {
			var id string
			var at int64
			if err := rows.Scan(&id, &at); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = time.Unix(at, 0).UTC()
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
