// Package ingestapi serves the service's only WRITE surface: a small
// authenticated endpoint that accepts pushed burn-day readings.
//
// # Why this exists at all, on an otherwise read-only public API
//
// Every other source here is polled. The burn-day reading is different: it is
// produced by a job WE run (cmd/burn-line, on a daily schedule), so publishing
// it to a file for ourselves to poll would be indirection with no reader but
// us, and would delay a once-daily reading by up to a poll interval. It is
// pushed instead.
//
// Three boundaries keep that from eroding the read-only posture:
//
//  1. **It never writes events.** The handler validates a reading and lands it
//     in the burn_readings staging table; the ingest scheduler remains the sole
//     owner of event writes and picks the row up on its own tick. This is the
//     same push-source-wrapped-as-a-poller shape MeshCore uses.
//  2. **It is authenticated, and the rest of the API is not.** A single bearer
//     token, compared in constant time. It is the only route in the service
//     that requires a credential.
//  3. **It stays browser-unreachable cross-origin.** prefab's CORS config
//     grants GET only, so a POST preflight from any origin is denied — the same
//     property that already keeps /mcp unreachable from a page. Do not add POST
//     to corsAllowMethods.
package ingestapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/dpup/sierra-data/internal/store"
)

// maxBody caps a pushed document. A reading is a few hundred bytes; the
// transcript is the only unbounded field and a phone message is short.
const maxBody = 64 << 10 // 64 KiB

// BurnLineHandler accepts pushed burn-day readings.
type BurnLineHandler struct {
	store *store.Store
	token string
	// lines is the set of line ids this deployment will accept a reading for,
	// from grid.burn.lines. A push for anything else is rejected: the staging
	// table should never accumulate rows no poller will ever read.
	lines map[string]bool
	now   func() time.Time
}

// NewBurnLineHandler builds the handler. An empty token disables the endpoint
// outright (it 404s), so a deployment that has not configured a credential does
// not expose an unauthenticated write path by omission.
func NewBurnLineHandler(st *store.Store, token string, lineIDs []string) *BurnLineHandler {
	set := make(map[string]bool, len(lineIDs))
	for _, id := range lineIDs {
		set[strings.ToLower(id)] = true
	}
	return &BurnLineHandler{store: st, token: token, lines: set, now: time.Now}
}

// Enabled reports whether a credential is configured.
func (h *BurnLineHandler) Enabled() bool { return h.token != "" }

// pushRequest is the wire shape. It deliberately mirrors the burnday.ersn.net
// document so the two remain interchangeable during the cutover.
type pushRequest struct {
	// LineID names WHICH recorded line this reading came from. It is the unit of
	// identity, not the county: one line can speak for several counties.
	LineID               string `json:"lineId"`
	Status               string `json:"status"`
	Message              string `json:"message"`
	CleanedTranscription string `json:"cleanedTranscription"`
	Confidence           int32  `json:"confidence"`
	// ObservedAt is when the burn line was CALLED. Required: it is the freshness
	// signal the ingest gate keys off, and a reading that cannot say when it was
	// taken cannot be judged.
	ObservedAt string `json:"observedAt"`
}

func (h *BurnLineHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !h.Enabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.authorized(r) {
		// No detail: an unauthenticated caller learns nothing about the token.
		logging.Warnw(ctx, "Burn-line push rejected: bad credential", "remote", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return
	}
	var req pushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON")
		return
	}

	reading, err := h.validate(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	reading.ReceivedAt = h.now().UTC()

	if err := h.store.PutBurnReading(ctx, reading); err != nil {
		logging.Errorw(ctx, "Burn-line push: store write failed", "line", reading.LineID, "error", err)
		writeErr(w, http.StatusInternalServerError, "could not store reading")
		return
	}
	logging.Infow(ctx, "Burn-line push accepted",
		"line", reading.LineID, "status", reading.Status,
		"confidence", reading.Confidence, "observedAt", reading.ObservedAt)

	// 202, not 200: the reading is staged, not yet reflected in any event. The
	// scheduler applies it on its next tick.
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// authorized checks the bearer token in constant time.
func (h *BurnLineHandler) authorized(r *http.Request) bool {
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

// validate turns a wire document into a reading, rejecting anything it cannot
// vouch for. It is deliberately strict: this is the one place external input
// reaches the store, and a bad row here silently becomes a wrong public answer
// about whether it is legal to light a fire.
func (h *BurnLineHandler) validate(req pushRequest) (store.BurnReading, error) {
	lineID := strings.ToLower(strings.TrimSpace(req.LineID))
	if lineID == "" {
		return store.BurnReading{}, errors.New("lineId is required")
	}
	if !h.lines[lineID] {
		return store.BurnReading{}, errors.New("lineId is not a configured burn line")
	}
	status := strings.ToLower(strings.TrimSpace(req.Status))
	switch status {
	case "green", "red", "orange":
	default:
		// An unrecognized status must be REJECTED, not stored: stored, it would
		// map to UNKNOWN downstream and look like a source outage rather than a
		// contract break we need to fix.
		return store.BurnReading{}, errors.New(`status must be one of "green", "red", "orange"`)
	}
	if req.Confidence < 0 || req.Confidence > 100 {
		return store.BurnReading{}, errors.New("confidence must be between 0 and 100")
	}
	if strings.TrimSpace(req.ObservedAt) == "" {
		return store.BurnReading{}, errors.New("observedAt is required")
	}
	observed, err := time.Parse(time.RFC3339, req.ObservedAt)
	if err != nil {
		return store.BurnReading{}, errors.New("observedAt must be RFC 3339")
	}
	// A reading stamped in the future cannot be judged by an age-based gate and
	// would read as permanently fresh. Small clock skew is tolerated.
	if observed.After(h.now().UTC().Add(1 * time.Hour)) {
		return store.BurnReading{}, errors.New("observedAt is in the future")
	}
	return store.BurnReading{
		LineID:     lineID,
		Status:     status,
		Message:    strings.TrimSpace(req.Message),
		Transcript: strings.TrimSpace(req.CleanedTranscription),
		Confidence: req.Confidence,
		ObservedAt: observed.UTC(),
	}, nil
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
