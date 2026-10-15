package pushingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/config"
	"github.com/dpup/sierra-data/internal/store"
)

// burnRegistry builds a registry whose reporter is granted burn.line, backed by
// a real store (the stream stages rows, so a fake would test nothing).
func burnRegistry(t *testing.T, lineIDs ...string) (*Registry, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	if len(lineIDs) == 0 {
		lineIDs = []string{"calaveras-apcd", "tuolumne-apcd"}
	}
	r, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{{
		ID:          "burn-line",
		Name:        "County burn line reader",
		TokenSha256: TokenHash(testToken),
		Streams:     []string{BurnStream},
		StaleAfter:  30 * time.Hour,
		MinInterval: time.Nanosecond,
	}}}, WithBurnLines(st, lineIDs))
	require.NoError(t, err)
	return r, st
}

func burnBody(readings ...string) string {
	body := `{"schema_version":1,"generated_at":"2026-09-18T14:05:00Z","readings":[`
	for i, r := range readings {
		if i > 0 {
			body += ","
		}
		body += r
	}
	return body + `]}`
}

func reading(lineID, status, observed string) string {
	return fmt.Sprintf(
		`{"line_id":%q,"status":%q,"message":"msg","cleaned_transcription":"transcript","confidence":93,"observed_at":%q}`,
		lineID, status, observed)
}

var burnObserved = time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)

func TestBurnStream_StagesAReading(t *testing.T) {
	r, st := burnRegistry(t)
	w := post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "red", burnObserved)))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	var resp ingestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Accepted)
	assert.Empty(t, resp.Warnings)

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	require.Contains(t, rows, "calaveras-apcd")
	assert.Equal(t, "red", rows["calaveras-apcd"].Status)
	assert.Equal(t, int32(93), rows["calaveras-apcd"].Confidence)
}

// The tool batches every line into ONE report, because the rate limiter would
// 429 a second push. So a multi-reading report is the normal case, not an edge.
func TestBurnStream_BatchesSeveralLines(t *testing.T) {
	r, st := burnRegistry(t)
	w := post(r, testToken, BurnStream, burnBody(
		reading("calaveras-apcd", "green", burnObserved),
		reading("tuolumne-apcd", "red", burnObserved),
	))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "green", rows["calaveras-apcd"].Status)
	assert.Equal(t, "red", rows["tuolumne-apcd"].Status)
}

// A bad reading must not discard the good ones: the next attempt is TOMORROW.
func TestBurnStream_BadReadingIsAWarningNotAFailure(t *testing.T) {
	r, st := burnRegistry(t)
	w := post(r, testToken, BurnStream, burnBody(
		reading("calaveras-apcd", "green", burnObserved),
		reading("fresno-apcd", "red", burnObserved), // not a configured line
		reading("tuolumne-apcd", "chartreuse", burnObserved),
	))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	var resp ingestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Accepted)
	assert.Len(t, resp.Warnings, 2)

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Contains(t, rows, "calaveras-apcd")
	assert.NotContains(t, rows, "fresno-apcd", "an unconfigured line must never be staged")
	assert.NotContains(t, rows, "tuolumne-apcd", "an unknown status must never be staged")
}

func TestBurnStream_RejectsUnusableReports(t *testing.T) {
	cases := map[string]string{
		"wrong schema":  `{"schema_version":99,"readings":[]}`,
		"no readings":   `{"schema_version":1,"readings":[]}`,
		"all unusable":  burnBody(reading("calaveras-apcd", "chartreuse", burnObserved)),
		"missing stamp": burnBody(`{"line_id":"calaveras-apcd","status":"red","confidence":50}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r, st := burnRegistry(t)
			w := post(r, testToken, BurnStream, body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

			rows, err := st.BurnReadings(context.Background())
			require.NoError(t, err)
			assert.Empty(t, rows, "a rejected report must never reach the store")
		})
	}
}

// A future stamp cannot be judged by an age-based gate and would read as
// permanently fresh, so it is clamped rather than trusted.
func TestBurnStream_FutureStampIsClamped(t *testing.T) {
	r, st := burnRegistry(t)
	future := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
	w := post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "red", future)))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.False(t, rows["calaveras-apcd"].ObservedAt.After(time.Now().UTC().Add(time.Minute)),
		"observed_at must not be left in the future")
}

// Auth and per-stream grants are the framework's, but a burn reporter must
// actually be subject to them.
func TestBurnStream_AuthAndGrants(t *testing.T) {
	r, st := burnRegistry(t)
	body := burnBody(reading("calaveras-apcd", "red", burnObserved))

	assert.Equal(t, http.StatusUnauthorized, post(r, "", BurnStream, body).Code)
	assert.Equal(t, http.StatusUnauthorized, post(r, "wrong-token", BurnStream, body).Code)
	// Granted burn.line only, so the mesh stream is forbidden, not unknown.
	assert.Equal(t, http.StatusForbidden, post(r, testToken, MeshStream, `{"schema_version":1}`).Code)

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Dispatched but unwired: KnownStreams promises the route exists, so the failure
// must name the real problem rather than 404.
func TestBurnStream_UnconfiguredDeploymentSaysSo(t *testing.T) {
	r := testRegistry(t, func(rep *config.Reporter) { rep.Streams = []string{BurnStream} })
	w := post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "red", burnObserved)))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "no burn lines configured")
}

// The staging row is last-write-wins on observed_at, so a retry cannot move a
// line backwards to an older reading.
func TestBurnStream_RetryDoesNotRegress(t *testing.T) {
	r, st := burnRegistry(t)
	newer := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	older := time.Now().UTC().Add(-20 * time.Hour).Format(time.RFC3339)

	require.Equal(t, http.StatusAccepted, post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "green", newer))).Code)
	require.Equal(t, http.StatusAccepted, post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "red", older))).Code)

	rows, err := st.BurnReadings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "green", rows["calaveras-apcd"].Status, "an older retry must not displace a newer reading")
}

// A restart must not forget that the burn line reported: it pushes once a day,
// so in-memory-only health read "no report received yet" (STALE) for up to a
// day after every deploy, while its reading sat current in the store.
func TestBurnStream_HealthSurvivesRestart(t *testing.T) {
	r, st := burnRegistry(t)
	w := post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "red", burnObserved)))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	before := r.Health(BurnStream)[0]
	require.Equal(t, ReporterOK, before.State)

	// A fresh registry over the same store is a restarted process.
	restarted, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{{
		ID: "burn-line", TokenSha256: TokenHash(testToken),
		Streams: []string{BurnStream}, StaleAfter: 30 * time.Hour,
	}}}, WithBurnLines(st, []string{"calaveras-apcd"}))
	require.NoError(t, err)
	assert.Equal(t, ReporterUnknown, restarted.Health(BurnStream)[0].State)

	require.NoError(t, restarted.RestoreBurnHealth(context.Background()))
	after := restarted.Health(BurnStream)[0]
	assert.Equal(t, ReporterOK, after.State)
	assert.Equal(t, before.LastAcceptedAt.Unix(), after.LastAcceptedAt.Unix())
}

// Rows staged before the reporter id was recorded are credited only when one
// reporter could have sent them; with two, nothing is guessed.
func TestRestoreBurnHealth_UnattributedRows(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	received := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	require.NoError(t, st.PutBurnReading(context.Background(), store.BurnReading{
		LineID: "calaveras-apcd", Status: "red", ObservedAt: received, ReceivedAt: received,
	}))

	rep := func(id, token string) config.Reporter {
		return config.Reporter{ID: id, TokenSha256: TokenHash(token),
			Streams: []string{BurnStream}, StaleAfter: 30 * time.Hour}
	}
	one, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{rep("burn-line", "a")}},
		WithBurnLines(st, []string{"calaveras-apcd"}))
	require.NoError(t, err)
	require.NoError(t, one.RestoreBurnHealth(context.Background()))
	assert.Equal(t, ReporterOK, one.Health(BurnStream)[0].State)
	assert.True(t, received.Equal(one.Health(BurnStream)[0].LastAcceptedAt))

	two, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{
		rep("burn-line", "a"), rep("backup", "b")}},
		WithBurnLines(st, []string{"calaveras-apcd"}))
	require.NoError(t, err)
	require.NoError(t, two.RestoreBurnHealth(context.Background()))
	for _, h := range two.Health(BurnStream) {
		assert.Equal(t, ReporterUnknown, h.State, h.ID)
	}
}

// An accepted report fires the hook (which wakes the burn poller); a rejected
// one does not, since nothing new was staged.
func TestBurnStream_AcceptedReportFiresHook(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "grid.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	calls := 0
	r, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{{
		ID: "burn-line", TokenSha256: TokenHash(testToken),
		Streams: []string{BurnStream}, MinInterval: time.Nanosecond,
	}}}, WithBurnLines(st, []string{"calaveras-apcd"}), WithBurnAccepted(func() { calls++ }))
	require.NoError(t, err)

	w := post(r, testToken, BurnStream, burnBody(reading("not-a-line", "red", burnObserved)))
	require.NotEqual(t, http.StatusAccepted, w.Code)
	assert.Equal(t, 0, calls)

	w = post(r, testToken, BurnStream, burnBody(reading("calaveras-apcd", "red", burnObserved)))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, 1, calls)
}
