package sqliteimport

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// maxExamples bounds the differences a report lists per table and check.
const maxExamples = 10

// checker collects rehydration failures for one table.
type checker struct {
	table    string
	failures int64
	examples []string
	// contentHashDrift counts events whose stored content_hash differs from
	// store.ContentHash of the stored blob. Informational: the stored hash is
	// computed from the event AS POLLED (before the store backfills bbox and
	// centroid) and by whatever ContentHash rules were current then, so a
	// difference only means the next poll of that event may mint a revision.
	contentHashDrift int64
}

func (c *checker) failf(row []any, keyN int, format string, args ...any) {
	c.failures++
	if len(c.examples) < maxExamples {
		c.examples = append(c.examples, describeKey(row, keyN)+": "+fmt.Sprintf(format, args...))
	}
}

func unixOf(ts *timestamppb.Timestamp) any {
	if ts == nil {
		return nil
	}
	return ts.AsTime().Unix()
}

// bboxValues is the bbox as the four nullable column values (min_lat,
// max_lat, min_lng, max_lng), the same mapping as the store's bboxArgs.
func bboxValues(ev *gridv1.Event) []any {
	bb := ev.GetGeometry().GetBbox()
	if bb == nil {
		return []any{nil, nil, nil, nil}
	}
	return []any{bb.GetMinLat(), bb.GetMaxLat(), bb.GetMinLng(), bb.GetMaxLng()}
}

// eventToPG appends first_ingested_at (the srcCopy extra column) and the
// bbox columns derived from the blob.
func eventToPG(row []any) ([]any, error) {
	blob, _ := row[13].([]byte)
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		return nil, fmt.Errorf("event %v: blob does not unmarshal as grid.v1.Event: %w", row[0], err)
	}
	out := make([]any, 0, len(eventsCols)+len(eventsDerived))
	out = append(out, row[:len(eventsCols)]...)
	out = append(out, row[len(eventsCols)]) // first_ingested_at
	return append(out, bboxValues(ev)...), nil
}

// expect records a failure when the column value differs from the blob's.
func (c *checker) expect(row []any, keyN int, col string, got, want any) {
	if !equalCanon(got, want) {
		c.failf(row, keyN, "column %s = %s, blob says %s", col, describe(got), describe(want))
	}
}

// expectTime compares a timestamp column with the blob's timestamp when the
// blob has one. A blob without it gives the writer's fallback (the ingest
// time), which the column alone records.
func (c *checker) expectTime(row []any, keyN int, col string, got any, ts *timestamppb.Timestamp) {
	if ts != nil {
		c.expect(row, keyN, col, got, ts.AsTime().Unix())
	}
}

func checkEvent(c *checker, row []any) {
	blob, _ := row[13].([]byte)
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		c.failf(row, 1, "proto does not unmarshal as grid.v1.Event: %v", err)
		return
	}
	c.expect(row, 1, "id", row[0], ev.GetId())
	c.expect(row, 1, "layer", row[1], int64(ev.GetLayer()))
	c.expect(row, 1, "severity", row[2], int64(ev.GetSeverity()))
	c.expect(row, 1, "status", row[3], int64(ev.GetStatus()))
	c.expect(row, 1, "source_id", row[4], ev.GetProvenance().GetSourceId())
	c.expect(row, 1, "effective", row[5], unixOf(ev.GetEffective()))
	c.expect(row, 1, "expires", row[6], unixOf(ev.GetExpires()))
	c.expectTime(row, 1, "observed_at", row[7], ev.GetObservedAt())
	c.expectTime(row, 1, "ingested_at", row[8], ev.GetIngestedAt())
	c.expect(row, 1, "revision", row[10], int64(ev.GetRevision()))
	bbox := bboxValues(ev)
	for n, col := range eventsDerived[1:] {
		c.expect(row, 1, col, row[len(eventsCols)+1+n], bbox[n])
	}
	if hash, _ := row[12].(string); hash != store.ContentHash(ev) {
		c.contentHashDrift++
	}
}

func checkRevision(c *checker, row []any) {
	blob, _ := row[4].([]byte)
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		c.failf(row, 2, "proto does not unmarshal as grid.v1.Event: %v", err)
		return
	}
	c.expect(row, 2, "event_id", row[0], ev.GetId())
	c.expect(row, 2, "revision", row[1], int64(ev.GetRevision()))
	c.expectTime(row, 2, "observed_at", row[2], ev.GetObservedAt())
	c.expectTime(row, 2, "ingested_at", row[3], ev.GetIngestedAt())
}

func checkLite(c *checker, row []any) {
	blob, _ := row[1].([]byte)
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		c.failf(row, 1, "proto does not unmarshal as grid.v1.Event: %v", err)
		return
	}
	c.expect(row, 1, "event_id", row[0], ev.GetId())
	if g := ev.GetGeometry(); len(g.GetGeojson()) > 0 || g.GetObject() != nil {
		c.failf(row, 1, "lite copy still carries a GeoJSON shape")
	}
}

func checkPlace(c *checker, row []any) {
	blob, _ := row[5].([]byte)
	p := &gridv1.Place{}
	if err := proto.Unmarshal(blob, p); err != nil {
		c.failf(row, 1, "proto does not unmarshal as grid.v1.Place: %v", err)
		return
	}
	c.expect(row, 1, "id", row[0], p.GetId())
	c.expect(row, 1, "kind", row[1], int64(p.GetKind()))
	c.expect(row, 1, "name", row[2], p.GetName())
	c.expect(row, 1, "slug", row[3], p.GetSlug())
	var parent any
	if p.GetParentId() != "" {
		parent = p.GetParentId()
	}
	c.expect(row, 1, "parent_id", row[4], parent)
}
