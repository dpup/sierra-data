package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UpsertResult reports what UpsertEvent did.
type UpsertResult struct {
	Changed  bool   // a row was written (new event or new revision)
	Revision uint32 // current revision after the call
}

// ContentHash returns the SHA-256 hex of a deterministic marshal of ev with
// volatile fields zeroed: revision, ingested_at, observed_at,
// provenance.fetched_at, enhancement, summary, place_ids, and (for MESH
// events) mesh.telemetry. Upstream re-stamps without content change produce
// the same hash (no revision), and enhancement output never causes hash churn
// — the scheduler decides whether to spend enhancement budget via NeedsUpdate
// BEFORE enhancing. The MESH exclusion is what keeps the MeshCore advert
// firehose from minting a revision per packet: SNR/RSSI/hops/gateways change
// constantly but ride in telemetry, so only a node's stable identity, role,
// name, location, or status flips the hash. See internal/store/AGENTS.md.
func ContentHash(ev *gridv1.Event) string {
	c := proto.Clone(ev).(*gridv1.Event)
	c.Revision = 0
	c.IngestedAt = nil
	c.ObservedAt = nil
	if c.Provenance != nil {
		c.Provenance.FetchedAt = nil
	}
	c.Enhancement = nil
	c.Summary = ""
	c.PlaceIds = nil
	if n := c.GetMesh(); n != nil {
		n.Telemetry = nil
	}
	// The county burn line's message names the date ("Today, September 10th, is
	// a burn day"), so its text changes daily even when the ANSWER has not.
	// Hashed, that would mint a revision every day and bury the few real
	// transitions this layer exists to record. Each LINE carries its own
	// observation, so this zeroes all of them; the lines' burn_day values stay
	// hashed, because a line changing its answer IS the transition.
	if b := c.GetBurnStatus(); b != nil {
		for _, line := range b.GetBurnLines() {
			line.Observation = nil
		}
		// Moves every tick; hashed, it would mint a revision every tick.
		b.CalfireObservedAt = nil
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		// Marshal of a well-formed generated message cannot fail; a sentinel
		// keyed by id keeps the failure visible without panicking ingest.
		b = []byte("marshal-error:" + ev.GetId())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// maxHashBatch bounds one ContentHashes round trip (one array parameter), so
// a whole poll is one or two queries.
const maxHashBatch = 900

// ContentHashes returns the stored content hash for each of ids that exists, in
// ONE query per batch. Absent ids are simply missing from the map (the caller
// treats that as "new, must write").
//
// This exists because the per-event alternative is catastrophic on a network
// filesystem. The ingest scheduler pre-checks every polled event before deciding
// whether to open a write transaction; doing that one id at a time made the mesh
// tick issue ~399 separate `SELECT content_hash` statements, each a separate EFS
// round trip. Measured in production: 9.2 SECONDS per tick with zero events
// actually written, on alternating ticks (~23 ms per query — one round trip
// each). That 9-second window is what user reads were contending with.
// Batched, the same check is one round trip.
func (s *Store) ContentHashes(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += maxHashBatch {
		end := min(start+maxHashBatch, len(ids))
		chunk := ids[start:end]
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, content_hash FROM events WHERE id = ANY($1)`, chunk)
		if err != nil {
			return nil, fmt.Errorf("store: content hashes: %w", err)
		}
		for rows.Next() {
			var id, hash string
			if err := rows.Scan(&id, &hash); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan content hash: %w", err)
			}
			out[id] = hash
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: iterate content hashes: %w", err)
		}
	}
	return out, nil
}

// NeedsUpdate reports whether UpsertEvent(ev) would write: true when the
// event is unknown or its normalized content hash differs from the stored
// one. Single-event form — prefer ContentHashes when checking a whole poll,
// which is one round trip instead of one per event.
func (s *Store) NeedsUpdate(ctx context.Context, ev *gridv1.Event) (bool, error) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM events WHERE id = $1`, ev.GetId()).Scan(&stored)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: needs-update %s: %w", ev.GetId(), err)
	}
	return stored != ContentHash(ev), nil
}

// UpsertEvent inserts or revises an event, gated on ContentHash:
//
//   - no existing row: insert at revision 1;
//   - hash equal to stored: no revision and Changed=false, but the place
//     attachments are still refreshed — place_ids are zeroed out of the
//     hash, so a changed caller preset (e.g. a new zone->area mapping) or a
//     place seeded after the event first arrived must still attach;
//   - hash differs: revision = old+1, row updated.
//
// Every content write also inserts an event_revisions snapshot (bumping the
// 'revisions' version), recomputes event_places, writes the bbox columns and
// the lite copy, and stamps last_seen_at. first_ingested_at is set on insert
// only.
// ingested_at is stamped here; callers own observed_at (nil falls back to
// now for the NOT NULL index column only).
func (s *Store) UpsertEvent(ctx context.Context, ev *gridv1.Event) (UpsertResult, error) {
	if ev.GetId() == "" {
		return UpsertResult{}, fmt.Errorf("store: upsert event with empty id")
	}
	// Hash the event exactly as passed — before any store-side mutation — so
	// the stored hash always matches what the next poll's ContentHash yields.
	hash := ContentHash(ev)

	var res UpsertResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var oldRev uint32
		var oldHash string
		err := tx.QueryRow(`SELECT revision, content_hash FROM events WHERE id = $1`, ev.GetId()).
			Scan(&oldRev, &oldHash)
		switch {
		case err == sql.ErrNoRows:
			res = UpsertResult{Changed: true, Revision: 1}
		case err != nil:
			return fmt.Errorf("store: upsert lookup %s: %w", ev.GetId(), err)
		case oldHash == hash:
			res = UpsertResult{Changed: false, Revision: oldRev}
			// Content unchanged, but the place set may not be: recompute and
			// sync attachments without touching revision/hash/history.
			_, err := s.refreshEventPlaces(tx, ev)
			return err
		default:
			res = UpsertResult{Changed: true, Revision: oldRev + 1}
		}

		now := time.Now()
		c := proto.Clone(ev).(*gridv1.Event)
		c.Revision = res.Revision
		c.IngestedAt = timestamppb.New(now)
		ensureGeometryIndex(c)

		placeIDs, err := s.matchPlaces(tx, c)
		if err != nil {
			return err
		}
		// UNION caller-preset place_ids (e.g. NWS zone->area mapping) with
		// geometric matches — preset ids are never dropped.
		c.PlaceIds = unionSorted(ev.GetPlaceIds(), placeIDs)

		blob, err := proto.Marshal(c)
		if err != nil {
			return fmt.Errorf("store: marshal event %s: %w", c.GetId(), err)
		}

		observedAt := now.Unix() // NOT NULL index column fallback; blob keeps caller's value
		if c.GetObservedAt() != nil {
			observedAt = c.GetObservedAt().AsTime().Unix()
		}
		if _, err := tx.Exec(`
			INSERT INTO events (id, layer, severity, status, source_id, effective, expires,
			                    observed_at, ingested_at, first_ingested_at, revision, content_hash,
			                    proto, last_seen_at, min_lat, max_lat, min_lng, max_lng)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10, $11, $12, $9, $13, $14, $15, $16)
			ON CONFLICT (id) DO UPDATE SET
			  layer = excluded.layer, severity = excluded.severity, status = excluded.status,
			  source_id = excluded.source_id, effective = excluded.effective,
			  expires = excluded.expires, observed_at = excluded.observed_at,
			  ingested_at = excluded.ingested_at, revision = excluded.revision,
			  content_hash = excluded.content_hash, proto = excluded.proto,
			  last_seen_at = excluded.last_seen_at,
			  min_lat = excluded.min_lat, max_lat = excluded.max_lat,
			  min_lng = excluded.min_lng, max_lng = excluded.max_lng`,
			append([]any{c.GetId(), int32(c.GetLayer()), int32(c.GetSeverity()), int32(c.GetStatus()),
				c.GetProvenance().GetSourceId(), unixOrNil(c.GetEffective()), unixOrNil(c.GetExpires()),
				observedAt, now.Unix(), res.Revision, hash, blob}, bboxArgs(c)...)...,
		); err != nil {
			return fmt.Errorf("store: upsert event %s: %w", c.GetId(), err)
		}
		if err := insertRevision(tx, c.GetId(), res.Revision, observedAt, now.Unix(), blob); err != nil {
			return err
		}
		if err := putLite(tx, c); err != nil {
			return err
		}
		return replaceEventPlaces(tx, c.GetId(), c.PlaceIds)
	})
	if err != nil {
		return UpsertResult{}, err
	}
	return res, nil
}

// TransitionEvents moves each event to status `to`, bumping its revision and
// writing a revision snapshot — the all-clear IS history. Events already in
// that status (and unknown ids) are skipped, so lifecycle sweeps are
// idempotent. observedAt is when the transition was observed (e.g. the poll
// that noticed the disappearance).
func (s *Store) TransitionEvents(ctx context.Context, ids []string, to gridv1.EventStatus, observedAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	// A repeated id is already in the target status the second time round.
	seen := make(map[string]bool, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(uniq); start += transitionChunk {
			if err := transitionChunkTx(ctx, tx, uniq[start:min(start+transitionChunk, len(uniq))], to, observedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// transitionChunk bounds one batch: a perimeter blob can be large, and the
// batch holds the events, their revision snapshots and their lite copies at
// once.
const transitionChunk = 100

// transitionChunkTx transitions one chunk in a fixed number of round trips (one
// read, then one set-based write each for the events, their revision
// snapshots, their lite copies and the 'revisions' counter) instead of five
// per event. Events already in the target status, and unknown ids, are left
// out of every write. The rows written are what the per-event loop wrote,
// with one ingested_at stamp for the chunk.
func transitionChunkTx(ctx context.Context, tx *sql.Tx, ids []string, to gridv1.EventStatus, observedAt time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, proto FROM events WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("store: transition lookup: %w", err)
	}
	stored := make(map[string][]byte, len(ids))
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			rows.Close()
			return fmt.Errorf("store: transition scan: %w", err)
		}
		stored[id] = blob
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: transition lookup: %w", err)
	}

	now := time.Now()
	var (
		mid              []string
		rev              []int64
		hashes           []string
		blobs, liteBlobs [][]byte
		liteIDs          []string
	)
	for _, id := range ids { // the caller's order, not the map's
		blob, ok := stored[id]
		if !ok {
			continue
		}
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return fmt.Errorf("store: transition unmarshal %s: %w", id, err)
		}
		if ev.GetStatus() == to {
			continue
		}
		ev.Status = to
		ev.ObservedAt = timestamppb.New(observedAt)
		ev.IngestedAt = timestamppb.New(now)
		ev.Revision++
		newBlob, err := proto.Marshal(ev)
		if err != nil {
			return fmt.Errorf("store: transition marshal %s: %w", id, err)
		}
		lb, err := liteBlob(ev)
		if err != nil {
			return fmt.Errorf("store: marshal lite event %s: %w", id, err)
		}
		mid, rev = append(mid, id), append(rev, int64(ev.GetRevision()))
		// Status is hashed content: recompute so a later reappearance in
		// the feed (status differs) correctly writes a revision.
		hashes = append(hashes, ContentHash(ev))
		blobs, liteBlobs, liteIDs = append(blobs, newBlob), append(liteBlobs, lb), append(liteIDs, id)
	}
	if len(mid) == 0 {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE events e SET status = $1, observed_at = $2, ingested_at = $3,
		                    revision = v.revision, content_hash = v.content_hash, proto = v.proto
		FROM unnest($4::text[], $5::bigint[], $6::text[], $7::bytea[]) AS v(id, revision, content_hash, proto)
		WHERE e.id = v.id`,
		int32(to), observedAt.Unix(), now.Unix(), mid, rev, hashes, blobs); err != nil {
		return fmt.Errorf("store: transition update of %d events: %w", len(mid), err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_revisions (event_id, revision, observed_at, ingested_at, proto)
		SELECT id, revision, $1, $2, proto
		FROM unnest($3::text[], $4::bigint[], $5::bytea[]) AS v(id, revision, proto)`,
		observedAt.Unix(), now.Unix(), mid, rev, blobs); err != nil {
		return fmt.Errorf("store: transition revisions of %d events: %w", len(mid), err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_lite (event_id, proto)
		SELECT id, proto FROM unnest($1::text[], $2::bytea[]) AS v(id, proto)
		ON CONFLICT (event_id) DO UPDATE SET proto = excluded.proto`, liteIDs, liteBlobs); err != nil {
		return fmt.Errorf("store: transition lite copies of %d events: %w", len(mid), err)
	}
	return bumpVersion(tx, versionRevisions, int64(len(mid)))
}

// TouchSeen stamps last_seen_at = t for every id whose current stamp is older
// than staleBefore, in one UPDATE, and returns how many rows it rewrote. It
// writes no revisions and never touches the content hash — "the source still
// lists this event" is liveness metadata, not content. The scheduler calls it
// for every id a successful poll returned, including hash-equal no-op upserts,
// so the expire grace is anchored to the last successful appearance.
//
// The staleBefore cutoff is why this takes a cutoff at all, and it is a
// PERFORMANCE INVARIANT, not a convenience. Without it this rewrote
// last_seen_at for the ENTIRE polled set every tick — ~400 mesh rows every
// 60s, each row carrying the full proto blob, so the whole active working set
// was dirtied, journaled, and fsynced once a minute. Any commit makes every
// other connection discard its page cache wholesale, and on EFS the changed
// pages come back over the network — so the first reader after each tick paid
// hundreds of cold page reads (measured 1.7-3.5s on ~7% of requests, clustered
// on the tick cadence). last_seen_at feeds expire graces measured in HOURS
// (smallest configured: 2h), so per-minute precision bought nothing.
//
// With the cutoff, a row freshly stamped within the coalescing window is left
// alone — no dirty page — and a tick where nothing is stale commits nothing
// (an UPDATE matching zero rows journals nothing and bumps no change counter).
// The stamp may lag the last confirmed appearance by up to the caller's
// window; callers must keep that window far below the smallest expireAfter
// they configure, since the worst case is an event expiring earlier by the
// lag. Passing staleBefore = t reproduces the old always-rewrite behavior.
func (s *Store) TouchSeen(ctx context.Context, ids []string, t, staleBefore time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var touched int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE events SET last_seen_at = $1 WHERE id = ANY($2) AND last_seen_at < $3`,
			t.Unix(), ids, staleBefore.Unix())
		if err != nil {
			return fmt.Errorf("store: touch seen: %w", err)
		}
		if touched, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("store: touch seen rows affected: %w", err)
		}
		return nil
	})
	return touched, err
}

// StoredEvent pairs an event with store-side liveness metadata that is not
// part of the proto blob.
type StoredEvent struct {
	Event *gridv1.Event
	// LastSeenAt is when a successful poll last included this event (see
	// TouchSeen). Zero for rows that predate last-seen tracking; callers
	// fall back to observed/ingested times.
	LastSeenAt time.Time
}

// ActiveEventsBySource returns all ACTIVE and SCHEDULED events for a source,
// each with its last-seen time. The lifecycle sweep diffs this set against
// the latest poll to find disappeared events and anchors the expire grace to
// LastSeenAt.
func (s *Store) ActiveEventsBySource(ctx context.Context, sourceID string) ([]StoredEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT proto, last_seen_at FROM events
		WHERE source_id = $1 AND status IN ($2, $3)
		ORDER BY id`,
		sourceID, int32(gridv1.EventStatus_ACTIVE), int32(gridv1.EventStatus_SCHEDULED))
	if err != nil {
		return nil, fmt.Errorf("store: active by source %s: %w", sourceID, err)
	}
	defer rows.Close()

	var out []StoredEvent
	for rows.Next() {
		var blob []byte
		var lastSeen int64
		if err := rows.Scan(&blob, &lastSeen); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return nil, fmt.Errorf("store: unmarshal event: %w", err)
		}
		se := StoredEvent{Event: ev}
		if lastSeen > 0 {
			se.LastSeenAt = time.Unix(lastSeen, 0)
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// EventsBySourceAndStatus returns a source's events in the given statuses,
// ordered by id. The mesh normalizer reads its EXPIRED nodes this way: a node
// the mesh once announced stays a known identity (key, name, position) that
// relay evidence can bring back, after the sweep has expired it.
func (s *Store) EventsBySourceAndStatus(ctx context.Context, sourceID string, statuses ...gridv1.EventStatus) ([]*gridv1.Event, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT proto FROM events
		WHERE source_id = $1 AND status = ANY($2)
		ORDER BY id`, sourceID, enumInts(statuses))
	if err != nil {
		return nil, fmt.Errorf("store: events by source %s: %w", sourceID, err)
	}
	defer rows.Close()
	var out []*gridv1.Event
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		ev := &gridv1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			return nil, fmt.Errorf("store: unmarshal event: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetEvent returns the current revision of an event, or ErrNotFound.
func (s *Store) GetEvent(ctx context.Context, id string) (*gridv1.Event, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT proto FROM events WHERE id = $1`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get event %s: %w", id, err)
	}
	ev := &gridv1.Event{}
	if err := proto.Unmarshal(blob, ev); err != nil {
		return nil, fmt.Errorf("store: unmarshal event %s: %w", id, err)
	}
	return ev, nil
}

// EventStamp identifies one served state of an event: its revision, plus how
// many times the current revision's blob has been rewritten in place (the
// hash-equal path persists hash-excluded fields such as an AI enhancement,
// mesh telemetry or a burn observation without minting a revision). The pair
// changes whenever GetEvent's body would, so it is the event-detail validator.
type EventStamp struct {
	Revision  int64
	Refreshes int64
}

// EventVersion returns an event's current stamp without rehydrating its proto
// blob — a cheap index-only read. ok is false for an unknown id. Callers use it
// as an ETag validator to answer a conditional GET before the expensive load.
// Revision history only grows with Revision; Refreshes never touches it.
func (s *Store) EventVersion(ctx context.Context, id string) (stamp EventStamp, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT revision, refreshes FROM events WHERE id = $1`, id).
		Scan(&stamp.Revision, &stamp.Refreshes)
	if err == sql.ErrNoRows {
		return EventStamp{}, false, nil
	}
	if err != nil {
		return EventStamp{}, false, fmt.Errorf("store: event version %s: %w", id, err)
	}
	return stamp, true, nil
}

// --- internals ---

func insertRevision(tx *sql.Tx, id string, rev uint32, observedAt, ingestedAt int64, blob []byte) error {
	if _, err := tx.Exec(`
		INSERT INTO event_revisions (event_id, revision, observed_at, ingested_at, proto)
		VALUES ($1, $2, $3, $4, $5)`,
		id, rev, observedAt, ingestedAt, blob,
	); err != nil {
		return fmt.Errorf("store: insert revision %s@%d: %w", id, rev, err)
	}
	return bumpVersion(tx, versionRevisions, 1)
}

// refreshEventPlaces syncs an existing event's place attachments on the
// hash-equal upsert path. place_ids are zeroed out of the content hash, so
// hash-equal does NOT mean place-set-equal: the caller's preset ids may have
// changed (config edit) and places seeded after the event first arrived
// (boot-order, new polygons) must still match geometrically. When the
// recomputed set differs from the stored rows, event_places and the stored
// blob's place_ids are updated in place — no new revision, hash and
// revision untouched. It reports whether it rewrote the row; a rewrite bumps
// the row's refreshes counter and the layer's refresh version (in this
// transaction), the validators' view of the change.
func (s *Store) refreshEventPlaces(tx *sql.Tx, ev *gridv1.Event) (bool, error) {
	c := proto.Clone(ev).(*gridv1.Event)
	ensureGeometryIndex(c)
	matched, err := s.matchPlaces(tx, c)
	if err != nil {
		return false, err
	}
	want := unionSorted(ev.GetPlaceIds(), matched)

	rows, err := tx.Query(`SELECT place_id FROM event_places WHERE event_id = $1 ORDER BY place_id`, ev.GetId())
	if err != nil {
		return false, fmt.Errorf("store: load event_places %s: %w", ev.GetId(), err)
	}
	var have []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return false, fmt.Errorf("store: scan event_places %s: %w", ev.GetId(), err)
		}
		have = append(have, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: iterate event_places %s: %w", ev.GetId(), err)
	}
	placesChanged := !slices.Equal(want, have)

	// Load the stored blob to compare the hash-excluded fields (place_ids and the
	// AI enhancement/summary) against the incoming event.
	var blob []byte
	if err := tx.QueryRow(`SELECT proto FROM events WHERE id = $1`, ev.GetId()).Scan(&blob); err != nil {
		return false, fmt.Errorf("store: load event %s for refresh: %w", ev.GetId(), err)
	}
	stored := &gridv1.Event{}
	if err := proto.Unmarshal(blob, stored); err != nil {
		return false, fmt.Errorf("store: unmarshal event %s for refresh: %w", ev.GetId(), err)
	}

	// enhancement/summary are excluded from the content hash, so a re-run
	// enhancement (fresh enhanced_at/request/response, or a reworded summary)
	// with otherwise-identical content lands on this hash-equal path. Persist the
	// latest without a new revision. Only when the incoming actually carries a
	// fresh enhancement — never erase a stored enhancement on a plain no-op poll
	// (weather alerts don't re-enhance on hash-equal ticks, so ev.Enhancement is
	// nil then and must not overwrite the stored one).
	enhChanged := ev.GetEnhancement() != nil &&
		(!proto.Equal(stored.GetEnhancement(), ev.GetEnhancement()) || stored.GetSummary() != ev.GetSummary())

	// Mesh telemetry is the other hash-excluded field, and it is excluded for the
	// opposite reason from enhancement: not because it is expensive to recompute,
	// but because it changes on EVERY observation. Battery, airtime and packet
	// counters hashed would mint a revision per report and drown the node's real
	// history (renames, relocations, going unreachable) in noise.
	//
	// The consequence is that this path is the ONLY thing that can ever persist
	// them: a telemetry-only change is hash-equal, so it lands here. Without this
	// branch the stored sample would only ever be refreshed on the ticks where
	// something else about the node changed — for a fixed repeater, essentially
	// never — leaving a battery reading frozen at whatever was current the last
	// time anyone renamed it. Reaching the branch at all requires the scheduler
	// to take the write path, which it does for PollResult.ForceWrite ids.
	//
	// Only when the incoming actually carries telemetry: a poll that omits it
	// must not erase a stored sample (same rule as the enhancement above).
	telChanged := ev.GetMesh().GetTelemetry() != nil && stored.GetMesh() != nil &&
		!proto.Equal(stored.GetMesh().GetTelemetry(), ev.GetMesh().GetTelemetry())

	// Burn status's volatile block, excluded from the hash for the same reason as
	// mesh telemetry: each line's observation names the date ("Today, October
	// 10th, is not a burn day"), so it changes daily while the ANSWER does not.
	// A newer reading of the same answer is therefore hash-equal and lands here,
	// and this is the only thing that persists it. Without it the event kept its
	// FIRST observation forever (2026-10-09 → 10-12 in production while daily
	// readings landed), and consumers reading observation.observedAt as "as of"
	// declared fresh data stale. The event's own observed_at / fetched_at follow
	// the reading, since they are the "as of" the API documents.
	burnChanged := false
	if in, have := ev.GetBurnStatus(), stored.GetBurnStatus(); in != nil && have != nil {
		burnChanged = !proto.Equal(burnVolatile(in), burnVolatile(have)) ||
			!proto.Equal(ev.GetObservedAt(), stored.GetObservedAt()) ||
			!proto.Equal(ev.GetProvenance().GetFetchedAt(), stored.GetProvenance().GetFetchedAt())
	}

	if !placesChanged && !enhChanged && !telChanged && !burnChanged {
		return false, nil
	}
	if placesChanged {
		if err := replaceEventPlaces(tx, ev.GetId(), want); err != nil {
			return false, err
		}
	}
	// Keep the canonical blob consistent (reads rehydrate from it): place_ids in
	// lockstep with event_places, and the freshest enhancement/summary.
	stored.PlaceIds = want
	if enhChanged {
		stored.Enhancement = ev.GetEnhancement()
		stored.Summary = ev.GetSummary()
	}
	if telChanged {
		stored.GetMesh().Telemetry = ev.GetMesh().GetTelemetry()
	}
	if burnChanged {
		// The incoming burn detail differs from the stored one ONLY in the
		// hash-excluded fields (the hashes are equal), so taking it wholesale
		// persists the fresh observations without touching the answer.
		stored.Detail = &gridv1.Event_BurnStatus{BurnStatus: ev.GetBurnStatus()}
		stored.ObservedAt = ev.GetObservedAt()
		if stored.Provenance != nil && ev.GetProvenance() != nil {
			stored.Provenance.FetchedAt = ev.GetProvenance().GetFetchedAt()
		}
	}
	newBlob, err := proto.Marshal(stored)
	if err != nil {
		return false, fmt.Errorf("store: marshal event %s for refresh: %w", ev.GetId(), err)
	}
	// Columns are indexes re-derived from the blob on every write (see
	// internal/store/AGENTS.md): the burn refresh moves observed_at, so the
	// observed_at column moves with it. For every other refresh it is unchanged.
	if err := putLite(tx, stored); err != nil {
		return false, err
	}
	if stored.GetObservedAt() != nil {
		if _, err := tx.Exec(`UPDATE events SET proto = $1, observed_at = $2, refreshes = refreshes + 1 WHERE id = $3`,
			newBlob, stored.GetObservedAt().AsTime().Unix(), ev.GetId()); err != nil {
			return false, fmt.Errorf("store: update event %s for refresh: %w", ev.GetId(), err)
		}
	} else if _, err := tx.Exec(`UPDATE events SET proto = $1, refreshes = refreshes + 1 WHERE id = $2`, newBlob, ev.GetId()); err != nil {
		return false, fmt.Errorf("store: update event %s for refresh: %w", ev.GetId(), err)
	}
	// Same transaction as the rewrite: a reader can never pair the new
	// version with the old blob.
	return true, bumpVersion(tx, refreshKey(ev.GetLayer()), 1)
}

// replaceEventPlaces sets an event's attachments: one DELETE and one
// multi-row INSERT (unnest), two round trips whatever the place count.
func replaceEventPlaces(tx *sql.Tx, id string, placeIDs []string) error {
	if _, err := tx.Exec(`DELETE FROM event_places WHERE event_id = $1`, id); err != nil {
		return fmt.Errorf("store: clear event_places %s: %w", id, err)
	}
	if len(placeIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO event_places (event_id, place_id)
		SELECT $1, p FROM unnest($2::text[]) AS p`, id, placeIDs); err != nil {
		return fmt.Errorf("store: insert event_places %s: %w", id, err)
	}
	return nil
}

// bboxArgs is the event's bbox as the four nullable column values
// (min_lat, max_lat, min_lng, max_lng), NULL without a bbox.
func bboxArgs(ev *gridv1.Event) []any {
	b := ev.GetGeometry().GetBbox()
	if b == nil {
		return []any{nil, nil, nil, nil}
	}
	return []any{b.GetMinLat(), b.GetMaxLat(), b.GetMinLng(), b.GetMaxLng()}
}

// enumInts converts proto enum values to the int32 slice an integer[]
// parameter (= ANY($n)) takes.
func enumInts[E ~int32](vs []E) []int32 {
	out := make([]int32, len(vs))
	for i, v := range vs {
		out[i] = int32(v)
	}
	return out
}

// ensureGeometryIndex backfills bbox and centroid from the raw GeoJSON when
// the normalizer left them unset. Geometry is hashed content, but the hash is
// computed from the event as passed, so backfilling here cannot cause churn.
func ensureGeometryIndex(ev *gridv1.Event) {
	g := ev.GetGeometry()
	if g == nil || len(g.GetGeojson()) == 0 {
		return
	}
	if g.GetBbox() != nil && g.GetCentroid() != nil {
		return
	}
	parsed, err := geojson.Parse(g.GetGeojson())
	if err != nil {
		return // columns are indexes only; bad geometry must not block ingest
	}
	if g.GetBbox() == nil {
		minLat, minLng, maxLat, maxLng := parsed.Bbox()
		g.Bbox = &gridv1.BoundingBox{MinLat: minLat, MinLng: minLng, MaxLat: maxLat, MaxLng: maxLng}
	}
	if g.GetCentroid() == nil {
		lat, lng := parsed.Centroid()
		g.Centroid = &gridv1.LatLng{Lat: lat, Lng: lng}
	}
}

// corridorBufferMeters is how close a point event must be to a corridor
// LineString to attach to it. Corridors are seeded as the straight
// origin->destination chord (not the road's true path), so the buffer has to
// absorb both the chord-vs-road deviation on these short mountain sections and
// the few-hundred-metre slop in CHP/Caltrans incident coordinates. Tune here.
// corridorBufferDeg is a loose degree-margin used only to widen the cheap bbox
// prefilter (in PlacesContaining) so a point near a zero-width corridor line
// still reaches the exact PointNearLine test. ~1° of latitude ≈ 111 km; the 1.3
// factor is deliberate slack (covers the ~cos(lat) shrink of a longitude degree
// at the service-area latitude and keeps the prefilter safely inclusive — the
// exact test is authoritative, so an over-wide prefilter only costs a few extra
// distance checks, never a missed attachment).
const (
	corridorBufferMeters = 1500
	corridorBufferDeg    = corridorBufferMeters / 111000.0 * 1.3
)

// matchPlaces computes the geometric event->place attachments.
//
// Rule (over-attach beats missing a perimeter that crosses a boundary):
//   - point events (or events whose GeoJSON won't parse but carry a
//     centroid): attach every place whose polygon contains the centroid;
//   - polygon/multipolygon events: attach a place when the bboxes intersect
//     AND (the event centroid is in the place, OR the place's bbox center is
//     in the event geometry, OR the place is polygonal and the two geometries
//     ACTUALLY intersect). The last rule is a real polygon-overlap test, not a
//     bbox one — county bboxes are large interlocking rectangles, so a bare
//     bbox-overlap rule attached a fire to every county whose bbox it clipped
//     (e.g. a Tuolumne fire falsely tagged Calaveras + Stanislaus);
//   - other event types (linestrings): bbox intersect AND centroid in place.
//
// One layer gets an extra, looser rule on top: a WILDFIRE event also attaches to
// an AREA or TOWN place it comes within wildfireBuffer metres of
// (WithWildfireProximity / grid.wildfire.placeBufferMeters). Fire is the hazard
// that MOVES toward you, so a fire 12 km outside the coverage polygon belongs on
// that area's map and summary — waiting for the perimeter to cross the line is
// waiting too long. It is scoped to those two kinds deliberately: counties tile
// the map, so a nearby fire already attaches to some county exactly and buffering
// them would smear one fire across four; corridors already have their own tuned
// 1.5 km buffer, and a 20 km fire buffer would pin every regional fire to every
// highway segment.
//
// Zone-carrying weather alerts are handled by the caller pre-setting
// ev.place_ids; UpsertEvent unions those with these matches.
func (s *Store) matchPlaces(tx *sql.Tx, ev *gridv1.Event) ([]string, error) {
	g := ev.GetGeometry()
	if g == nil {
		return nil, nil
	}
	var evGeom *geojson.Geom
	if len(g.GetGeojson()) > 0 {
		if parsed, err := geojson.Parse(g.GetGeojson()); err == nil {
			evGeom = parsed
		}
	}
	centroid := g.GetCentroid()
	if evGeom == nil && centroid == nil {
		return nil, nil
	}

	var evMinLat, evMinLng, evMaxLat, evMaxLng float64
	pointLike := evGeom == nil || evGeom.Type == "Point"
	if evGeom != nil {
		evMinLat, evMinLng, evMaxLat, evMaxLng = evGeom.Bbox()
		if centroid == nil {
			lat, lng := evGeom.Centroid()
			centroid = &gridv1.LatLng{Lat: lat, Lng: lng}
		}
	} else {
		evMinLat, evMinLng, evMaxLat, evMaxLng = centroid.GetLat(), centroid.GetLng(), centroid.GetLat(), centroid.GetLng()
	}
	evPolygonal := evGeom != nil && (evGeom.Type == "Polygon" || evGeom.Type == "MultiPolygon")

	places, err := s.loadPlaceGeoms(tx)
	if err != nil {
		return nil, err
	}

	// The extra wildfire proximity rule, resolved once: the buffer distance (0
	// disables it) and the geometry to measure from — the parsed GeoJSON, or a
	// synthetic point when only a centroid survived parsing.
	var nearBuffer float64
	var nearGeom *geojson.Geom
	if ev.GetLayer() == gridv1.Layer_WILDFIRE && s.wildfireBuffer > 0 {
		nearBuffer, nearGeom = s.wildfireBuffer, evGeom
		if nearGeom == nil {
			nearGeom = &geojson.Geom{
				Type:  "Point",
				Point: geojson.Position{centroid.GetLng(), centroid.GetLat()},
			}
		}
	}

	eg := eventGeo{
		geom: evGeom, centroid: centroid,
		minLat: evMinLat, minLng: evMinLng, maxLat: evMaxLat, maxLng: evMaxLng,
		pointLike: pointLike, polygonal: evPolygonal,
	}
	if ev.GetLayer() == gridv1.Layer_WEATHER_ALERT {
		// An alert's area is its NWS zones, drawn independently of the
		// Census counties: the two outlines of one border overlap in
		// slivers, which Intersects counts. Measured on the STO zones:
		// real county overlaps are 11-71% of the smaller shape, slivers
		// (Amador, Mariposa) at most 0.4%.
		eg.minOverlap = alertMinOverlap
	}

	var matched []string
	for _, pl := range places {
		near := nearBuffer > 0 && wildfireBufferedKind(pl.kind) &&
			geojson.WithinDistance(nearGeom, pl.geom, nearBuffer)
		if eg.matches(pl) || near {
			matched = append(matched, pl.id)
		}
	}
	return matched, nil
}

// eventGeo is an event's geometry, derived once by matchPlaces and tested
// against each place.
type eventGeo struct {
	geom                           *geojson.Geom // nil when the GeoJSON would not parse
	centroid                       *gridv1.LatLng
	minLat, minLng, maxLat, maxLng float64
	pointLike, polygonal           bool
	// minOverlap, when set, replaces the touch-counts Intersects test with
	// an overlap of at least this fraction of the smaller shape.
	minOverlap float64
}

// alertMinOverlap is the share of the smaller shape a weather alert's area
// and a place must overlap for the alert to attach by overlap alone.
const alertMinOverlap = 0.02

// matches applies the geometry attachment rules documented on matchPlaces. The
// wildfire proximity rule is separate and layered on by the caller.
func (e eventGeo) matches(pl parsedPlace) bool {
	if e.pointLike {
		// Cheap bbox reject before the exact test — the same prefilter the
		// request path already applies in PlacesContaining, widened by the
		// corridor buffer so a point near (but outside) a zero-width corridor
		// line still reaches the exact test. This runs for every event on every
		// tick, and the mesh layer (~93% of events, re-adverting constantly) is
		// overwhelmingly outside every place bbox, so without it each tick walks
		// every corridor polyline doing haversine per segment for points that
		// cannot possibly match. The exact test below stays authoritative.
		lat, lng := e.centroid.GetLat(), e.centroid.GetLng()
		if lat < pl.minLat-corridorBufferDeg || lat > pl.maxLat+corridorBufferDeg ||
			lng < pl.minLng-corridorBufferDeg || lng > pl.maxLng+corridorBufferDeg {
			return false
		}
		// Polygon places: exact point-in-polygon. Corridor (LineString) places:
		// within corridorBufferMeters of the road line. Same test resolve uses.
		return geojson.PointInOrNearGeometry(lat, lng, pl.geom, corridorBufferMeters)
	}
	if !geojson.BboxIntersects(e.minLat, e.minLng, e.maxLat, e.maxLng,
		pl.minLat, pl.minLng, pl.maxLat, pl.maxLng) {
		return false
	}
	if geojson.PointInGeometry(e.centroid.GetLat(), e.centroid.GetLng(), pl.geom) {
		return true
	}
	if !e.polygonal {
		return false
	}
	if geojson.PointInGeometry(pl.centLat, pl.centLng, e.geom) {
		return true
	}
	if !pl.polygonal {
		return false
	}
	if e.minOverlap > 0 {
		return geojson.OverlapFraction(e.geom, pl.geom) >= e.minOverlap
	}
	return geojson.Intersects(e.geom, pl.geom) // actual overlap, not just bbox
}

// wildfireBufferedKind reports whether a place kind participates in the wildfire
// proximity rule (see matchPlaces): the deployment's own coverage footprint and
// the communities inside it. Counties and corridors are deliberately excluded.
func wildfireBufferedKind(k gridv1.PlaceKind) bool {
	return k == gridv1.PlaceKind_AREA || k == gridv1.PlaceKind_TOWN
}

// loadPlaceGeoms returns the parsed place geometries, rebuilding the cache from
// the places table only when it has been invalidated (UpsertPlace) or never
// built. Called under the store mutex (matchPlaces runs inside inTx), so the
// cache needs no separate lock. This avoids re-SELECTing and re-parsing every
// place polygon on every per-event upsert — including hash-equal no-ops, which
// dominate a steady-state tick.
//
// The cache is valid only for the lease term that built it, and only inside a
// fenced write (txEpoch set by inTx): a reader has no business matching
// places, and a process that was a reader for a while may have missed another
// writer's place changes. That is a check, not a convention.
func (s *Store) loadPlaceGeoms(tx *sql.Tx) ([]parsedPlace, error) {
	if s.txEpoch == 0 {
		return nil, fmt.Errorf("store: place matching outside a writer transaction: %w", ErrNotWriter)
	}
	if s.placesGeoValid && s.placesGeoEpoch == s.txEpoch {
		return s.placesGeo, nil
	}
	rows, err := tx.Query(`SELECT id, proto FROM places`)
	if err != nil {
		return nil, fmt.Errorf("store: load places: %w", err)
	}
	defer rows.Close()

	var out []parsedPlace
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("store: scan place: %w", err)
		}
		place := &gridv1.Place{}
		if err := proto.Unmarshal(blob, place); err != nil {
			return nil, fmt.Errorf("store: unmarshal place %s: %w", id, err)
		}
		raw := place.GetGeometry().GetGeojson()
		if len(raw) == 0 {
			continue
		}
		g, err := geojson.Parse(raw)
		if err != nil {
			continue
		}
		minLat, minLng, maxLat, maxLng := g.Bbox()
		clat, clng := g.Centroid()
		out = append(out, parsedPlace{
			id: id, kind: place.GetKind(), geom: g,
			minLat: minLat, minLng: minLng, maxLat: maxLat, maxLng: maxLng,
			centLat: clat, centLng: clng,
			polygonal: g.Type == "Polygon" || g.Type == "MultiPolygon",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate places: %w", err)
	}
	s.placesGeo = out
	s.placesGeoValid = true
	s.placesGeoEpoch = s.txEpoch
	return out, nil
}

// unionSorted merges two id lists, deduped and sorted for deterministic blobs.
func unionSorted(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, list := range [][]string{a, b} {
		for _, id := range list {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// burnVolatile returns just the hash-excluded part of a burn status detail —
// CAL FIRE's observation stamp and each line's observation, in line order — so
// the hash-equal refresh can tell whether a newer reading arrived.
func burnVolatile(b *gridv1.BurnStatusDetail) *gridv1.BurnStatusDetail {
	v := &gridv1.BurnStatusDetail{CalfireObservedAt: b.GetCalfireObservedAt()}
	for _, l := range b.GetBurnLines() {
		v.BurnLines = append(v.BurnLines, &gridv1.BurnLineReading{Id: l.GetId(), Observation: l.GetObservation()})
	}
	return v
}
