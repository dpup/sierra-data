package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

// Disappearance policies (implementation plan decision 8): what the
// lifecycle sweep does when an event vanishes from its source's feed.
const (
	DisappearanceResolve = "resolve" // authoritative active-only feed => RESOLVED
	DisappearanceExpire  = "expire"  // missing AND past expires/grace => EXPIRED
)

// SourceSeed carries the config-owned fields of a source registry row.
// Runtime fields (last_attempt, last_success, last_error, status) are owned
// by RecordAttempt and never touched by seeding.
type SourceSeed struct {
	ID            string
	Name          string
	Attribution   string
	HomepageURL   string // upstream's human-facing page, surfaced as Source.homepage_url
	PollInterval  time.Duration
	StaleAfter    time.Duration // 0 => default 3x PollInterval
	ExpireAfter   time.Duration // 0 => never auto-expire
	Disappearance string        // DisappearanceResolve (default) | DisappearanceExpire
}

// SeedSources inserts or updates the config-owned fields of each source,
// preserving runtime health fields across restarts and config changes.
func (s *Store) SeedSources(ctx context.Context, seeds []SourceSeed) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, seed := range seeds {
			if seed.ID == "" {
				return fmt.Errorf("store: source seed with empty id")
			}
			disappearance := seed.Disappearance
			if disappearance == "" {
				disappearance = DisappearanceResolve
			}
			var staleAfter, expireAfter any
			if seed.StaleAfter > 0 {
				staleAfter = int64(seed.StaleAfter.Seconds())
			}
			if seed.ExpireAfter > 0 {
				expireAfter = int64(seed.ExpireAfter.Seconds())
			}
			if _, err := tx.Exec(`
				INSERT INTO sources (id, name, attribution, homepage_url, poll_interval_seconds,
				                     stale_after_seconds, expire_after_seconds, disappearance)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET
				  name = excluded.name, attribution = excluded.attribution,
				  homepage_url = excluded.homepage_url,
				  poll_interval_seconds = excluded.poll_interval_seconds,
				  stale_after_seconds = excluded.stale_after_seconds,
				  expire_after_seconds = excluded.expire_after_seconds,
				  disappearance = excluded.disappearance`,
				seed.ID, seed.Name, seed.Attribution, seed.HomepageURL,
				int64(seed.PollInterval.Seconds()),
				staleAfter, expireAfter, disappearance,
			); err != nil {
				return fmt.Errorf("store: seed source %s: %w", seed.ID, err)
			}
		}
		return nil
	})
}

// DeleteSource removes a source registry row (health + config). Used to retire a
// source no poller declares anymore, so /api/v1/sources doesn't list a defunct
// entry forever (SeedSources is upsert-only and never removes). Idempotent —
// deleting an absent id is a no-op. Events keep their own source_id in the proto
// blob and are unaffected (retire them separately via TransitionEvents).
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sources WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete source %s: %w", id, err)
		}
		return nil
	})
}

// RecordAttempt records the outcome of one poll of a source. last_attempt is
// always stamped. On success: last_success=now, last_error cleared,
// status=OK. On failure: last_error set, and status degrades to STALE while
// the last success is within stale_after (default 3x poll interval), then
// UNAVAILABLE — never-succeeded sources go straight to UNAVAILABLE.
func (s *Store) RecordAttempt(ctx context.Context, id string, attemptErr error) error {
	now := time.Now()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var pollInterval int64
		var staleAfter, lastSuccess sql.NullInt64
		err := tx.QueryRow(
			`SELECT poll_interval_seconds, stale_after_seconds, last_success_at FROM sources WHERE id = ?`, id,
		).Scan(&pollInterval, &staleAfter, &lastSuccess)
		if err == sql.ErrNoRows {
			return fmt.Errorf("store: record attempt for unseeded source %q", id)
		}
		if err != nil {
			return fmt.Errorf("store: record attempt %s: %w", id, err)
		}

		if attemptErr == nil {
			_, err := tx.Exec(`
				UPDATE sources SET last_attempt_at = ?, last_success_at = ?, last_error = '', status = ?
				WHERE id = ?`,
				now.Unix(), now.Unix(), int32(gridv1.SourceStatus_OK), id)
			if err != nil {
				return fmt.Errorf("store: record success %s: %w", id, err)
			}
			return nil
		}

		staleWindow := staleAfter.Int64
		if !staleAfter.Valid {
			staleWindow = 3 * pollInterval
		}
		status := gridv1.SourceStatus_UNAVAILABLE
		if lastSuccess.Valid && now.Unix()-lastSuccess.Int64 <= staleWindow {
			status = gridv1.SourceStatus_STALE
		}
		if _, err := tx.Exec(`
			UPDATE sources SET last_attempt_at = ?, last_error = ?, status = ? WHERE id = ?`,
			now.Unix(), attemptErr.Error(), int32(status), id); err != nil {
			return fmt.Errorf("store: record failure %s: %w", id, err)
		}
		return nil
	})
}

// ListSources returns the registry with health, ordered by id.
// stale_after_seconds is surfaced as the effective value (3x poll interval
// when unset); expire_after_seconds stays 0 for "never auto-expire".
func (s *Store) ListSources(ctx context.Context) ([]*gridv1.Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, attribution, homepage_url, poll_interval_seconds, stale_after_seconds,
		       expire_after_seconds, last_success_at, last_attempt_at, last_error, status
		FROM sources ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}
	defer rows.Close()

	var out []*gridv1.Source
	for rows.Next() {
		var src gridv1.Source
		var pollInterval int64
		var staleAfter, expireAfter, lastSuccess, lastAttempt sql.NullInt64
		var status int32
		if err := rows.Scan(&src.Id, &src.Name, &src.Attribution, &src.HomepageUrl, &pollInterval,
			&staleAfter, &expireAfter, &lastSuccess, &lastAttempt, &src.LastError, &status); err != nil {
			return nil, fmt.Errorf("store: scan source: %w", err)
		}
		src.PollIntervalSeconds = uint32(pollInterval)
		if staleAfter.Valid {
			src.StaleAfterSeconds = uint32(staleAfter.Int64)
		} else {
			src.StaleAfterSeconds = uint32(3 * pollInterval)
		}
		if expireAfter.Valid {
			src.ExpireAfterSeconds = uint32(expireAfter.Int64)
		}
		src.LastSuccessAt = tsFromNull(lastSuccess)
		src.LastAttemptAt = tsFromNull(lastAttempt)
		src.Status = gridv1.SourceStatus(status)
		out = append(out, &src)
	}
	return out, rows.Err()
}
