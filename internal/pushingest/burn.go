package pushingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/dpup/sierra-data/internal/store"
)

// BurnStream carries county burn-day readings: the answer a county air
// district's recorded phone line gives today.
//
// It is pushed rather than polled because the authority is a PHONE NUMBER, not a
// feed — cmd/burn-line dials it from CI, transcribes the recording and extracts
// a status. See internal/ingest/CLAUDE.md for the layer this feeds.
const BurnStream = "burn.line"

// burnSchemaVersion is the payload contract this handler understands.
const burnSchemaVersion = 1

// BurnStore is the write side the burn.line stream needs (satisfied by
// *store.Store).
type BurnStore interface {
	PutBurnReading(ctx context.Context, r store.BurnReading) error
}

// burnPayload is one report. Request bodies are snake_case here, matching
// mesh.repeater — the camelCase convention is for RESPONSES.
type burnPayload struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   string          `json:"generated_at"`
	Readings      []burnReadingIn `json:"readings"`
}

type burnReadingIn struct {
	LineID     string `json:"line_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Transcript string `json:"cleaned_transcription"`
	Confidence int32  `json:"confidence"`
	// ObservedAt is when the line was CALLED. Required: it is the freshness
	// signal the ingest gate keys off, and a reading that cannot say when it was
	// taken cannot be judged.
	ObservedAt string `json:"observed_at"`
}

// ingestBurn validates a report and STAGES each reading in the store.
//
// # Why this one writes the store when mesh.repeater does not
//
// A mesh monitor re-reports every few minutes, so an in-memory buffer losing a
// restart costs nothing — the next report refills it. A burn line is called
// ONCE A DAY. An in-memory buffer would leave burnDay UNKNOWN until the next
// morning after any deploy, on a layer whose whole job is to answer "can I burn
// today". So the reading is staged durably, keeping its own observed_at, and the
// freshness gate re-judges a rehydrated one rather than resurrecting it as
// current.
//
// The invariant this framework actually protects is unchanged: the scheduler is
// still the only thing that writes EVENTS. This writes a staging row, through
// the same store mutex as every other writer.
//
// Per-reading problems are WARNINGS, not errors: a bad line in a multi-line
// report must not discard the good ones, because the next attempt is tomorrow.
func (r *Registry) ingestBurn(ctx context.Context, rep *reporter, body []byte, maxItems int) (accepted int, warnings []string, err error) {
	if r.burn == nil || len(r.burnLines) == 0 {
		// Dispatched but not wired: this deployment configures no burn lines, so
		// there is nothing a reading could belong to. Say so — the reporter's
		// grant is fine and a 404 would send the operator hunting the wrong thing.
		return 0, nil, fmt.Errorf("this deployment has no burn lines configured (grid.burn.lines)")
	}

	payload, err := decodeJSON[burnPayload](body)
	if err != nil {
		return 0, nil, err
	}
	if payload.SchemaVersion != burnSchemaVersion {
		return 0, nil, fmt.Errorf("unsupported schema_version %d (want %d)",
			payload.SchemaVersion, burnSchemaVersion)
	}
	if len(payload.Readings) > maxItems {
		return 0, nil, fmt.Errorf("report carries %d readings, limit is %d",
			len(payload.Readings), maxItems)
	}
	if len(payload.Readings) == 0 {
		return 0, nil, fmt.Errorf("report carries no readings")
	}

	now := r.now()
	seen := make(map[string]bool, len(payload.Readings))

	for i, in := range payload.Readings {
		lineID := strings.ToLower(strings.TrimSpace(in.LineID))
		if lineID == "" {
			warnings = append(warnings, fmt.Sprintf("readings[%d]: line_id is required; skipped", i))
			continue
		}
		// A reading for a line this deployment does not configure has nowhere to
		// go — no poller will ever read it — so it is rejected rather than left
		// to accumulate in the staging table.
		if !r.burnLines[lineID] {
			warnings = append(warnings, fmt.Sprintf(
				"readings[%d]: %q is not a configured burn line; skipped", i, in.LineID))
			continue
		}
		if seen[lineID] {
			warnings = append(warnings, fmt.Sprintf("readings[%d]: duplicate line_id %q; keeping the first", i, lineID))
			continue
		}
		status := strings.ToLower(strings.TrimSpace(in.Status))
		switch status {
		case "green", "red", "orange":
		default:
			// Never coerced to a value: an unrecognized status downstream would
			// read as UNKNOWN and look like an outage rather than the contract
			// break it is.
			warnings = append(warnings, fmt.Sprintf(
				`readings[%d]: status %q is not one of "green", "red", "orange"; skipped`, i, in.Status))
			continue
		}
		if in.Confidence < 0 || in.Confidence > 100 {
			warnings = append(warnings, fmt.Sprintf(
				"readings[%d]: confidence %d is outside 0-100; skipped", i, in.Confidence))
			continue
		}
		observed := parseRFC3339(in.ObservedAt)
		if observed.IsZero() {
			warnings = append(warnings, fmt.Sprintf(
				"readings[%d]: observed_at %q is required and must be RFC 3339; skipped", i, in.ObservedAt))
			continue
		}
		// A future stamp cannot be judged by an age-based gate and would read as
		// permanently fresh. clampPast tolerates ordinary clock skew.
		observed = clampPast(observed, now)

		if err := r.burn.PutBurnReading(ctx, store.BurnReading{
			LineID:     lineID,
			Status:     status,
			Message:    strings.TrimSpace(in.Message),
			Transcript: strings.TrimSpace(in.Transcript),
			Confidence: in.Confidence,
			ObservedAt: observed,
			ReceivedAt: now,
		}); err != nil {
			// A store failure is OURS, not the reporter's: fail the request so it
			// retries, rather than 202-ing a reading we did not keep.
			return 0, warnings, fmt.Errorf("staging reading for %q: %w", lineID, err)
		}
		seen[lineID] = true
		accepted++
	}

	if accepted == 0 {
		return 0, warnings, fmt.Errorf("no usable readings in report")
	}

	r.mu.Lock()
	rep.lastAcceptedAt = now
	rep.lastError = ""
	rep.reports++
	r.mu.Unlock()

	return accepted, warnings, nil
}
