package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// RoadSample is one monitored road segment at one roads refresh.
type RoadSample struct {
	RoadID     string
	At         time.Time
	Status     int32 // api.RoadStatus
	Congestion int32 // api.CongestionLevel
	// Minutes; nil when the refresh had no travel time (Google Routes down).
	DurationMinutes *int32
	DelayMinutes    *int32
}

// InsertRoadSamples appends samples. INSERT OR IGNORE: the sampler re-offers
// the same refresh until the roads service refreshes again, and a sample is
// immutable once written.
func (s *Store) InsertRoadSamples(ctx context.Context, samples []RoadSample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO road_samples
			(road_id, at, status, congestion, duration_m, delay_m) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("store: prepare road sample insert: %w", err)
		}
		defer stmt.Close()
		for _, r := range samples {
			if r.RoadID == "" || r.At.IsZero() {
				continue
			}
			if _, err := stmt.ExecContext(ctx, r.RoadID, r.At.Unix(), r.Status, r.Congestion, r.DurationMinutes, r.DelayMinutes); err != nil {
				return fmt.Errorf("store: insert road sample %s: %w", r.RoadID, err)
			}
		}
		return nil
	})
}

// RoadSamples returns one road's samples in [from, to), oldest first.
func (s *Store) RoadSamples(ctx context.Context, roadID string, from, to time.Time) ([]RoadSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, status, congestion, duration_m, delay_m FROM road_samples
		WHERE road_id = ? AND at >= ? AND at < ? ORDER BY at`, roadID, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: road samples %s: %w", roadID, err)
	}
	defer rows.Close()
	var out []RoadSample
	for rows.Next() {
		var at int64
		var dur, delay sql.NullInt32
		r := RoadSample{RoadID: roadID}
		if err := rows.Scan(&at, &r.Status, &r.Congestion, &dur, &delay); err != nil {
			return nil, fmt.Errorf("store: scan road sample: %w", err)
		}
		r.At = time.Unix(at, 0).UTC()
		if dur.Valid {
			v := dur.Int32
			r.DurationMinutes = &v
		}
		if delay.Valid {
			v := delay.Int32
			r.DelayMinutes = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoadStatusSince is when road's latest sample's status began: the first
// sample of the current unbroken run. ok is false with no samples. A run
// that reaches back to the first sample we hold is reported from that
// sample, and atStart says so ("since at least").
func (s *Store) RoadStatusSince(ctx context.Context, roadID string) (since time.Time, status int32, atStart, ok bool, err error) {
	var latest int64
	err = s.db.QueryRowContext(ctx, `SELECT at, status FROM road_samples WHERE road_id = ? ORDER BY at DESC LIMIT 1`, roadID).Scan(&latest, &status)
	if err == sql.ErrNoRows {
		return time.Time{}, 0, false, false, nil
	}
	if err != nil {
		return time.Time{}, 0, false, false, fmt.Errorf("store: road status %s: %w", roadID, err)
	}
	// Two ranges on idx_road_samples_status rather than "status != ?",
	// which no index can serve: each reads only the other statuses' rows.
	var lastOther sql.NullInt64
	if err = s.db.QueryRowContext(ctx, `SELECT MAX(m) FROM (
		SELECT MAX(at) AS m FROM road_samples WHERE road_id = ?1 AND status < ?2
		UNION ALL
		SELECT MAX(at) FROM road_samples WHERE road_id = ?1 AND status > ?2)`, roadID, status).Scan(&lastOther); err != nil {
		return time.Time{}, 0, false, false, fmt.Errorf("store: road status %s: %w", roadID, err)
	}
	var first int64
	if err = s.db.QueryRowContext(ctx, `SELECT MIN(at) FROM road_samples WHERE road_id = ? AND at > ?`,
		roadID, lastOther.Int64).Scan(&first); err != nil {
		return time.Time{}, 0, false, false, fmt.Errorf("store: road status %s: %w", roadID, err)
	}
	return time.Unix(first, 0).UTC(), status, !lastOther.Valid, true, nil
}

// TypicalRoadMinutes is the median travel time over the samples in
// [from, to) whose status is openStatus and hour of the week (in loc)
// matches at's, falling back to every open sample when that hour has fewer
// than minSamples. ok is false with too few samples either way. The hour of
// week matters: Friday-evening traffic is not Tuesday-morning traffic.
func (s *Store) TypicalRoadMinutes(ctx context.Context, roadID string, openStatus int32, at, from, to time.Time, loc *time.Location, minSamples int) (minutes int32, basis string, ok bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, duration_m FROM road_samples
		WHERE road_id = ? AND status = ? AND duration_m IS NOT NULL AND at >= ? AND at < ?`,
		roadID, openStatus, from.Unix(), to.Unix())
	if err != nil {
		return 0, "", false, fmt.Errorf("store: typical road minutes %s: %w", roadID, err)
	}
	defer rows.Close()
	want := hourOfWeek(at.In(loc))
	var same, all []int32
	for rows.Next() {
		var ts int64
		var d int32
		if err := rows.Scan(&ts, &d); err != nil {
			return 0, "", false, err
		}
		all = append(all, d)
		if hourOfWeek(time.Unix(ts, 0).In(loc)) == want {
			same = append(same, d)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", false, err
	}
	switch {
	case len(same) >= minSamples:
		return median(same), "HOUR_OF_WEEK", true, nil
	case len(all) >= minSamples:
		return median(all), "ALL_HOURS", true, nil
	}
	return 0, "", false, nil
}

// PruneRoadSamples deletes samples older than cutoff.
func (s *Store) PruneRoadSamples(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM road_samples WHERE at < ?`, cutoff.Unix())
		if err != nil {
			return fmt.Errorf("store: prune road samples: %w", err)
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

func hourOfWeek(t time.Time) int { return int(t.Weekday())*24 + t.Hour() }

func median(v []int32) int32 {
	c := append([]int32(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2] + 1) / 2
}
