package pushingest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/config"
)

const testToken = "s3cret-token-value"

// sampleReport is the document shape an operator monitor actually produces (a
// trimmed copy of a live report). Two nodes with contrasting states: one healthy
// and one the monitor has NEVER reached — every metric null, last_success null.
const sampleReport = `{
  "schema_version": 1,
  "generated_at": "2026-09-15T00:23:32+00:00",
  "repeaters": [
    {"id":"de0715314cfa9b5e","name":"SIERRA Arnold Summit",
     "last_attempt":1789431672,"last_success":1789431672,
     "battery_voltage":4.14,"battery_percent":97.0,"battery_percent_source":"estimated",
     "temperature_c":37.0,"humidity":null,"pressure":null,
     "uptime_s":2615083,"airtime_ms":49646,"rx_airtime_ms":211238,
     "noise_floor_dbm":-116,"last_rssi_dbm":-88,"last_snr_db":12.5,"tx_queue_len":0,
     "nb_sent":150305,"nb_recv":649331,"sent_flood":149984,"sent_direct":321,
     "recv_flood":642043,"recv_direct":6702,"direct_dups":53,"flood_dups":30331,
     "full_evts":0,"recv_errors":240197,"online":true},
    {"id":"6781a18b2b47cb4e","name":"SIERRA Lilac Park",
     "last_attempt":1789421128,"last_success":null,
     "battery_voltage":null,"battery_percent":null,"battery_percent_source":null,
     "temperature_c":null,"humidity":null,"pressure":null,"uptime_s":null,
     "airtime_ms":null,"rx_airtime_ms":null,"noise_floor_dbm":null,
     "last_rssi_dbm":null,"last_snr_db":null,"tx_queue_len":null,
     "nb_sent":null,"nb_recv":null,"sent_flood":null,"sent_direct":null,
     "recv_flood":null,"recv_direct":null,"direct_dups":null,"flood_dups":null,
     "full_evts":null,"recv_errors":null,"online":false}
  ]
}`

func testRegistry(t *testing.T, mutate ...func(*config.Reporter)) *Registry {
	t.Helper()
	rep := config.Reporter{
		ID:          "alan-pi",
		Name:        "Alan's repeater monitor",
		TokenSha256: TokenHash(testToken),
		Streams:     []string{MeshStream},
		StaleAfter:  30 * time.Minute,
		// Effectively no rate limit: tests that care about it set their own.
		MinInterval: time.Nanosecond,
	}
	for _, m := range mutate {
		m(&rep)
	}
	r, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{rep}})
	require.NoError(t, err)
	return r
}

func post(r *Registry, token, stream, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/"+stream, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeStream(w, req, stream)
	return w
}

func TestRegistryRejectsBadConfig(t *testing.T) {
	cases := map[string]config.Reporter{
		"empty id":     {TokenSha256: TokenHash("x"), Streams: []string{MeshStream}},
		"short hash":   {ID: "a", TokenSha256: "abc", Streams: []string{MeshStream}},
		"non-hex hash": {ID: "a", TokenSha256: strings.Repeat("z", 64), Streams: []string{MeshStream}},
		"no streams":   {ID: "a", TokenSha256: TokenHash("x")},
	}
	for name, rep := range cases {
		t.Run(name, func(t *testing.T) {
			// A typo'd credential must fail LOUD at startup. Skipping the reporter
			// would present as a monitor that silently never authenticates, which
			// is indistinguishable from a wrong token on the client side.
			_, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{rep}})
			require.Error(t, err)
		})
	}
}

func TestRegistryRejectsSharedToken(t *testing.T) {
	h := TokenHash(testToken)
	_, err := NewRegistry(config.IngestConfig{Reporters: []config.Reporter{
		{ID: "a", TokenSha256: h, Streams: []string{MeshStream}},
		{ID: "b", TokenSha256: h, Streams: []string{MeshStream}},
	}})
	require.Error(t, err, "two reporters sharing a token makes attribution meaningless")
}

func TestPostAcceptsReport(t *testing.T) {
	r := testRegistry(t)
	w := post(r, testToken, MeshStream, sampleReport)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"accepted":2`)
	assert.Contains(t, w.Body.String(), `"reporterId":"alan-pi"`)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	snap := r.MeshReports()
	require.Len(t, snap.Reports, 2)
	assert.Equal(t, 1, snap.Live)
	assert.False(t, snap.SuppressSweep)

	byID := map[string]MeshNodeReport{}
	for _, rep := range snap.Reports {
		byID[rep.NodeID] = rep
	}

	arnold := byID["de0715314cfa9b5e"]
	require.NotNil(t, arnold.Telemetry)
	assert.Equal(t, "SIERRA Arnold Summit", arnold.Name)
	assert.Equal(t, "alan-pi", arnold.ReporterID)
	assert.Equal(t, "alan-pi", arnold.Telemetry.GetReporterId())
	assert.InDelta(t, 4.14, arnold.Telemetry.GetBatteryVolts().GetValue(), 0.001)
	assert.Equal(t, "estimated", arnold.Telemetry.GetBatteryPercentSource())
	assert.Equal(t, int64(150305), arnold.Telemetry.GetPacketsSent())
	assert.Nil(t, arnold.Telemetry.Humidity, "a sensor the node lacks stays absent, not zero")

	// The never-reached node carries NO sample. Publishing zeroed counters for it
	// would assert that it has sent and received nothing, which is a measurement
	// we never made.
	lilac := byID["6781a18b2b47cb4e"]
	assert.Nil(t, lilac.Telemetry, "never reached: no sample at all, not zeroed counters")
	assert.True(t, lilac.LastSuccess.IsZero())
	assert.False(t, lilac.LastAttempt.IsZero(), "the failed attempt is itself information")
}

func TestPostAuthFailures(t *testing.T) {
	r := testRegistry(t)

	t.Run("missing token", func(t *testing.T) {
		w := post(r, "", MeshStream, sampleReport)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.NotContains(t, w.Body.String(), "alan-pi", "a 401 must not confirm who exists")
	})
	t.Run("wrong token", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, post(r, "nope", MeshStream, sampleReport).Code)
	})
	t.Run("token is the stored hash", func(t *testing.T) {
		// Presenting the committed hash must NOT authenticate — otherwise the
		// public repo would contain working credentials.
		assert.Equal(t, http.StatusUnauthorized,
			post(r, TokenHash(testToken), MeshStream, sampleReport).Code)
	})
	t.Run("unknown stream", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, post(r, testToken, "weather.station", sampleReport).Code)
	})
	t.Run("unauthorized stream", func(t *testing.T) {
		r2 := testRegistry(t, func(rep *config.Reporter) { rep.Streams = []string{"other.stream"} })
		// 403 not 404: the credential is good, the grant is not — an operator
		// debugging a config typo needs to be able to tell those apart.
		assert.Equal(t, http.StatusForbidden, post(r2, testToken, MeshStream, sampleReport).Code)
	})
	t.Run("wrong method", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ingest/"+MeshStream, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		r.ServeStream(w, req, MeshStream)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})

	// None of the above may have left state behind.
	assert.Empty(t, r.MeshReports().Reports)
}

func TestPostRejectsMalformedBodies(t *testing.T) {
	r := testRegistry(t)
	cases := map[string]string{
		"not json":         `{`,
		"trailing content": sampleReport + `{"schema_version":1}`,
		"wrong version":    `{"schema_version":99,"repeaters":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := post(r, testToken, MeshStream, body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestPostEnforcesLimits(t *testing.T) {
	t.Run("body too large", func(t *testing.T) {
		r, err := NewRegistry(config.IngestConfig{
			MaxBodyBytes: 64,
			Reporters: []config.Reporter{{
				ID: "alan-pi", TokenSha256: TokenHash(testToken), Streams: []string{MeshStream}}},
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestEntityTooLarge, post(r, testToken, MeshStream, sampleReport).Code)
	})

	t.Run("too many items", func(t *testing.T) {
		r, err := NewRegistry(config.IngestConfig{
			MaxItems: 1,
			Reporters: []config.Reporter{{
				ID: "alan-pi", TokenSha256: TokenHash(testToken), Streams: []string{MeshStream}}},
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, post(r, testToken, MeshStream, sampleReport).Code)
	})

	t.Run("rate limit", func(t *testing.T) {
		r := testRegistry(t, func(rep *config.Reporter) { rep.MinInterval = time.Hour })
		require.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, sampleReport).Code)
		w := post(r, testToken, MeshStream, sampleReport)
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.NotEmpty(t, w.Header().Get("Retry-After"))
	})

	t.Run("rate limit is measured from the last ACCEPTED report", func(t *testing.T) {
		// A client being rejected must not be able to lock itself out: if the
		// window moved on every attempt, a monitor retrying after a 400 would
		// never get back in.
		r := testRegistry(t, func(rep *config.Reporter) { rep.MinInterval = time.Hour })
		require.Equal(t, http.StatusBadRequest, post(r, testToken, MeshStream, `{`).Code)
		assert.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, sampleReport).Code)
	})
}

func TestReportWarnsButKeepsTheRest(t *testing.T) {
	r := testRegistry(t)
	body := `{"schema_version":1,"generated_at":"2026-09-15T00:23:32+00:00","repeaters":[
	  {"id":"nothex!!","name":"Typo"},
	  {"id":"de0715314cfa9b5e","name":"Good","last_attempt":1789431672,"last_success":1789431672,"nb_sent":5},
	  {"id":"de0715314cfa9b5e","name":"Dup"}
	]}`
	w := post(r, testToken, MeshStream, body)
	require.Equal(t, http.StatusAccepted, w.Code)
	// One bad id must not cost the operator the other repeaters in the report.
	assert.Contains(t, w.Body.String(), `"accepted":1`)
	assert.Contains(t, w.Body.String(), "not 8-64 hex")
	assert.Contains(t, w.Body.String(), "duplicate id")

	snap := r.MeshReports()
	require.Len(t, snap.Reports, 1)
	assert.Equal(t, "Good", snap.Reports[0].Name, "the first of a duplicate pair wins")
}

func TestReportReplacesPreviousSet(t *testing.T) {
	r := testRegistry(t)
	require.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, sampleReport).Code)
	require.Len(t, r.MeshReports().Reports, 2)

	// A report is the reporter's COMPLETE current set — the same contract
	// PollResult.Events has with the disappearance sweep. Merging instead of
	// replacing would make a node immortal the moment a monitor stopped listing
	// it.
	smaller := `{"schema_version":1,"generated_at":"2026-09-15T00:30:00+00:00","repeaters":[
	  {"id":"de0715314cfa9b5e","name":"SIERRA Arnold Summit","last_attempt":1789431672,"last_success":1789431672,"nb_sent":1}]}`
	require.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, smaller).Code)
	snap := r.MeshReports()
	require.Len(t, snap.Reports, 1)
	assert.Equal(t, "de0715314cfa9b5e", snap.Reports[0].NodeID)
}

func TestFutureStampsAreClamped(t *testing.T) {
	r := testRegistry(t)
	future := time.Now().Add(72 * time.Hour).Unix()
	body := fmt.Sprintf(`{"schema_version":1,"generated_at":"2099-01-01T00:00:00+00:00","repeaters":[
	  {"id":"de0715314cfa9b5e","name":"Skewed","last_attempt":%d,"last_success":%d,"nb_sent":1}]}`,
		future, future)
	require.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, body).Code)

	rep := r.MeshReports().Reports[0]
	now := time.Now()
	// A monitor's clock is not ours, and mesh deployments have a documented
	// history of skew. A future stamp that survived here would propagate into
	// last_seen_at and keep a node alive past any grace.
	assert.False(t, rep.LastSuccess.After(now))
	assert.False(t, rep.ReportedAt.After(now))
}

func TestReporterStateTransitions(t *testing.T) {
	r := testRegistry(t)
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	assert.Equal(t, ReporterUnknown, r.Health(MeshStream)[0].State,
		"a monitor that was never set up is not a healthy feed")
	assert.Zero(t, r.MeshReports().Live)

	require.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, sampleReport).Code)
	assert.Equal(t, ReporterOK, r.Health(MeshStream)[0].State)

	// STALE: silent, but plausibly coming back. Contribute nothing (replaying a
	// silent monitor's last set would fabricate liveness) but suppress the sweep
	// (the nodes are missing for OUR reason).
	r.now = func() time.Time { return base.Add(31 * time.Minute) }
	assert.Equal(t, ReporterStale, r.Health(MeshStream)[0].State)
	snap := r.MeshReports()
	assert.Empty(t, snap.Reports)
	assert.True(t, snap.SuppressSweep)
	assert.Zero(t, snap.Live)

	// DEAD: silent long enough that we must admit we no longer know. Suppression
	// ends so the layer's lifecycle is not frozen forever by a monitor that is
	// never coming back.
	r.now = func() time.Time { return base.Add(30*time.Minute*reporterDeadMultiple + time.Minute) }
	assert.Equal(t, ReporterDead, r.Health(MeshStream)[0].State)
	snap = r.MeshReports()
	assert.Empty(t, snap.Reports)
	assert.False(t, snap.SuppressSweep)
}

func TestMalformedReportsAgeTheReporter(t *testing.T) {
	r := testRegistry(t)
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	require.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, sampleReport).Code)

	r.now = func() time.Time { return base.Add(31 * time.Minute) }
	require.Equal(t, http.StatusBadRequest, post(r, testToken, MeshStream, `{`).Code)

	// A monitor posting garbage is not a healthy monitor. Traffic arriving must
	// not paint the source green.
	h := r.Health(MeshStream)[0]
	assert.Equal(t, ReporterStale, h.State)
	assert.NotEmpty(t, h.LastError)
}

func TestServeHTTPTakesStreamFromPath(t *testing.T) {
	r := testRegistry(t)
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+MeshStream, strings.NewReader(sampleReport))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

func TestValidNodeID(t *testing.T) {
	assert.True(t, validNodeID("de0715314cfa9b5e"))
	assert.True(t, validNodeID(strings.Repeat("ab", 32)))
	assert.False(t, validNodeID("de07"), "too short to resolve with any confidence")
	assert.False(t, validNodeID("de0715314cfa9b5"), "odd length is not hex bytes")
	assert.False(t, validNodeID(strings.Repeat("ab", 33)), "longer than a public key")
	assert.False(t, validNodeID("zz0715314cfa9b5e"))
}

// Every advertised stream must actually dispatch. `make ingest-token` validates
// a reporter's grants against KnownStreams, so a name listed there but missing
// from dispatch would be accepted into config and then 404 on every post.
func TestKnownStreamsAllDispatch(t *testing.T) {
	r := testRegistry(t)
	require.NotEmpty(t, KnownStreams())
	for _, s := range KnownStreams() {
		_, ok := r.dispatch(s)
		assert.True(t, ok, "stream %q is advertised but does not dispatch", s)
	}
}
