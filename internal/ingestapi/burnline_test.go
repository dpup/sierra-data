package ingestapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/store"
)

const testToken = "s3cret-token"

var pushNow = time.Date(2026, 9, 10, 20, 0, 0, 0, time.UTC)

func newHandler(t *testing.T) (*BurnLineHandler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	h := NewBurnLineHandler(st, testToken, []string{"calaveras-apcd", "tuolumne-apcd"})
	h.now = func() time.Time { return pushNow }
	return h, st
}

func post(h *BurnLineHandler, token string, body any) *httptest.ResponseRecorder {
	var payload string
	switch v := body.(type) {
	case string:
		payload = v
	default:
		b, _ := json.Marshal(v)
		payload = string(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/ingest/burn-line", strings.NewReader(payload))
	req = req.WithContext(logging.With(req.Context(), logging.NewDevLogger()))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validBody() map[string]any {
	return map[string]any{
		"lineId":               "calaveras-apcd",
		"status":               "red",
		"message":              "Burning is prohibited today in Calaveras County.",
		"cleanedTranscription": "Thank you for calling...",
		"confidence":           95,
		"observedAt":           pushNow.Add(-2 * time.Hour).Format(time.RFC3339),
	}
}

func TestPush_AcceptsAValidReading(t *testing.T) {
	h, st := newHandler(t)
	rec := post(h, testToken, validBody())
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	r, ok := rows["calaveras-apcd"]
	require.True(t, ok)
	assert.Equal(t, "red", r.Status)
	assert.Equal(t, int32(95), r.Confidence)
	assert.Equal(t, pushNow.Add(-2*time.Hour).UTC(), r.ObservedAt)
}

// --- auth ---------------------------------------------------------------------

func TestPush_RequiresTheToken(t *testing.T) {
	h, st := newHandler(t)
	for name, tok := range map[string]string{
		"no token":    "",
		"wrong token": "not-the-token",
		"prefix only": "s3cret",
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(h, tok, validBody())
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected push must never reach the store")
}

// An unconfigured credential must DISABLE the endpoint, not leave it open.
func TestPush_NoTokenConfiguredMeansDisabled(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	h := NewBurnLineHandler(st, "", []string{"calaveras-apcd"})

	assert.False(t, h.Enabled())
	rec := post(h, "", validBody())
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// And an attacker guessing a token gets the same 404.
	assert.Equal(t, http.StatusNotFound, post(h, "anything", validBody()).Code)
}

func TestPush_RejectsNonPost(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/ingest/burn-line", nil)
	req = req.WithContext(logging.With(req.Context(), logging.NewDevLogger()))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// --- validation ---------------------------------------------------------------

// This is the one place external input reaches the store, and a bad row becomes
// a wrong public answer about whether it is legal to light a fire.
func TestPush_Validation(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"missing lineId":      func(m map[string]any) { delete(m, "lineId") },
		"unconfigured lineId": func(m map[string]any) { m["lineId"] = "fresno-apcd" },
		"unknown status":      func(m map[string]any) { m["status"] = "chartreuse" },
		"missing status":      func(m map[string]any) { delete(m, "status") },
		"missing observedAt":  func(m map[string]any) { delete(m, "observedAt") },
		"bad observedAt":      func(m map[string]any) { m["observedAt"] = "last tuesday" },
		"future observedAt":   func(m map[string]any) { m["observedAt"] = pushNow.Add(48 * time.Hour).Format(time.RFC3339) },
		"confidence > 100":    func(m map[string]any) { m["confidence"] = 101 },
		"confidence < 0":      func(m map[string]any) { m["confidence"] = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h, st := newHandler(t)
			body := validBody()
			mutate(body)
			rec := post(h, testToken, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

			rows, err := st.BurnReadings(context.Background())
			require.NoError(t, err)
			assert.Empty(t, rows, "an invalid reading must never reach the store")
		})
	}
}

func TestPush_RejectsMalformedJSON(t *testing.T) {
	h, _ := newHandler(t)
	assert.Equal(t, http.StatusBadRequest, post(h, testToken, "{not json").Code)
}

// --- idempotency / ordering ---------------------------------------------------

// A retry or duplicate delivery must never move a line BACKWARDS to an older
// reading — that would hand the freshness gate a stale stamp and blank a facet
// that was fine.
func TestPush_OlderReadingDoesNotOverwriteNewer(t *testing.T) {
	h, st := newHandler(t)

	newer := validBody()
	newer["observedAt"] = pushNow.Add(-1 * time.Hour).Format(time.RFC3339)
	newer["status"] = "green"
	require.Equal(t, http.StatusAccepted, post(h, testToken, newer).Code)

	older := validBody()
	older["observedAt"] = pushNow.Add(-20 * time.Hour).Format(time.RFC3339)
	older["status"] = "red"
	// Accepted at the HTTP layer (it is a well-formed reading)...
	require.Equal(t, http.StatusAccepted, post(h, testToken, older).Code)

	// ...but it must not have displaced the newer one.
	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "green", rows["calaveras-apcd"].Status)
	assert.Equal(t, pushNow.Add(-1*time.Hour).UTC(), rows["calaveras-apcd"].ObservedAt)
}

func TestPush_NewerReadingReplaces(t *testing.T) {
	h, st := newHandler(t)
	first := validBody()
	first["observedAt"] = pushNow.Add(-20 * time.Hour).Format(time.RFC3339)
	first["status"] = "red"
	require.Equal(t, http.StatusAccepted, post(h, testToken, first).Code)

	second := validBody()
	second["observedAt"] = pushNow.Add(-1 * time.Hour).Format(time.RFC3339)
	second["status"] = "green"
	require.Equal(t, http.StatusAccepted, post(h, testToken, second).Code)

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "green", rows["calaveras-apcd"].Status)
}
