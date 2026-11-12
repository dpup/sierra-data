package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
)

const reporterToken = "reporter-token-for-tests"

func withReporter(app *gridApp) {
	sum := sha256.Sum256([]byte(reporterToken))
	app.cfg.Grid.Ingest.Reporters = []config.Reporter{{ID: "alanpi",
		TokenSha256: hex.EncodeToString(sum[:]), Streams: []string{pushingest.MeshStream}}}
}

// push sends a preflight push (auth and grant checked, nothing recorded)
// and returns the response.
func push(t *testing.T, port int, token string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/ingest/%s?preflight=true", port, pushingest.MeshStream), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp
}

func TestIngestPathMatchesPushingest(t *testing.T) {
	assert.Equal(t, pushingest.IngestPath, ingestPath)
}

// D9: a push that lands on a reader is forwarded to the writer (same bearer,
// its answer relayed); with no live writer it is a 503 with Retry-After; a
// takeover moves where pushes land without the pusher noticing.
func TestPushIngestIsForwardedToTheWriter(t *testing.T) {
	blockOutbound(t)
	dbURL := seededGridDB(t)
	a, portA := preparedGrid(t, dbURL)
	withReporter(a)
	ha := runGrid(t, a, false)
	require.Eventually(t, func() bool { return push(t, portA, reporterToken).StatusCode == http.StatusNoContent },
		20*time.Second, 20*time.Millisecond, "a's writer term serves pushes")

	b, portB := preparedGrid(t, dbURL)
	withReporter(b)
	hb := runGrid(t, b, false)
	require.Eventually(t, func() bool { return hb.state() == "ACTIVE" }, 5*time.Second, 10*time.Millisecond)
	require.False(t, b.DatabaseHeld(), "a holds the lease; b campaigns")

	assert.Equal(t, http.StatusNoContent, push(t, portA, reporterToken).StatusCode, "the writer serves it")
	assert.Equal(t, http.StatusNoContent, push(t, portB, reporterToken).StatusCode, "a reader forwards it")
	bad := push(t, portB, "wrong")
	assert.Equal(t, http.StatusUnauthorized, bad.StatusCode, "the writer's answer is relayed")
	assert.NotEmpty(t, bad.Header.Get("WWW-Authenticate"))

	// a yields: b takes over and serves pushes itself; a now forwards to b.
	assert.Equal(t, http.StatusOK, ha.post("/yield"))
	require.Eventually(t, func() bool { return push(t, portB, reporterToken).StatusCode == http.StatusNoContent },
		5*time.Second, 10*time.Millisecond, "b took over and serves pushes")
	assert.Equal(t, http.StatusNoContent, push(t, portA, reporterToken).StatusCode)

	// A forwarded request that reaches a non-writer is not forwarded again.
	loop := push(t, portA, reporterToken, forwardedHeader, "elsewhere")
	assert.Equal(t, http.StatusServiceUnavailable, loop.StatusCode)

	// No writer at all: 503 with Retry-After, from both.
	assert.Equal(t, http.StatusOK, hb.post("/yield"))
	for _, port := range []int{portA, portB} {
		resp := push(t, port, reporterToken)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "2", resp.Header.Get("Retry-After"))
	}
}
