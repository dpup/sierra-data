package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- expectedInstanceId -------------------------------------------------------

func (h *harness) postFor(path, opID, instanceID string) (int, opResponse) {
	h.t.Helper()
	b, _ := json.Marshal(opRequest{OperationID: opID, ExpectedInstanceID: instanceID})
	req, _ := http.NewRequest(http.MethodPost, h.base+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	var out opResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMismatchedInstanceIsRefusedInStandby(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	me := h.status().InstanceID

	code, out := h.postFor("/activate", "a1", "someone-else")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "instance_mismatch", out.Error.Code)
	assert.Equal(t, me, out.InstanceID, "the refusal says who actually answered")
	assert.Equal(t, StateStandby, h.status().State)
	assert.Nil(t, h.status().Operation, "no operation was even recorded")
	a, _ := svc.counts()
	assert.Zero(t, a)

	code, out = h.postFor("/quiesce", "q1", "someone-else")
	assert.Equal(t, http.StatusConflict, code, "not even the standby fence")
	assert.Equal(t, StateStandby, h.status().State)

	code, out = h.postFor("/activate", "a1", me)
	require.Equal(t, http.StatusAccepted, code)
	assert.Equal(t, me, out.InstanceID)
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
}

func TestMismatchedInstanceIsRefusedEvenForAReplay(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	me := h.status().InstanceID
	h.postFor("/activate", "a1", me)
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })

	code, _ := h.postFor("/activate", "a1", me)
	assert.Equal(t, http.StatusOK, code, "the matching replay is still idempotent")
	code, out := h.postFor("/activate", "a1", "previous-incarnation")
	assert.Equal(t, http.StatusConflict, code, "a replay addressed elsewhere is refused, not answered 200")
	assert.Equal(t, "instance_mismatch", out.Error.Code)

	code, _ = h.postFor("/quiesce", "q1", "previous-incarnation")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, StateActive, h.status().State, "a mismatched quiesce must not release anything")
	_, q := svc.counts()
	assert.Zero(t, q)

	code, _ = h.postFor("/quiesce", "q1", me)
	require.Equal(t, http.StatusAccepted, code)
	h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiesced })
}

// A container restart: same address, new process, new instanceId. A delayed
// activation meant for the old process must not activate the new one.
func TestRestartedProcessIgnoresCommandsForItsPredecessor(t *testing.T) {
	old := start(t, &fakeService{}, true)
	oldID := old.status().InstanceID
	old.terminate()

	svc := &fakeService{}
	next := start(t, svc, true)
	require.NotEqual(t, oldID, next.status().InstanceID, "every process gets its own instanceId")
	code, out := next.postFor("/activate", "deploy-7-activate", oldID)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "instance_mismatch", out.Error.Code)
	assert.Equal(t, StateStandby, next.status().State)
	a, _ := svc.counts()
	assert.Zero(t, a)
}

func TestOmittedInstanceIDStillWorks(t *testing.T) {
	h := start(t, &fakeService{}, true)
	code, _ := h.postFor("/activate", "a1", "")
	assert.Equal(t, http.StatusAccepted, code, "hand-run commands may omit it")
}

// --- ECS identity ---------------------------------------------------------------

// fakeECS serves the two v4 metadata documents. container returns the
// container document for the n-th container request (to model a restart).
type fakeECS struct {
	calls     atomic.Int32
	fail      atomic.Bool
	container func(n int) map[string]any
}

func (f *fakeECS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() {
			http.Error(w, "agent unavailable", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/v4/abc":
			_ = json.NewEncoder(w).Encode(f.container(int(f.calls.Add(1))))
		case "/v4/abc/task":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Cluster": "project-hub", "TaskARN": "arn:aws:ecs:us-east-1:1:task/project-hub/t1",
				"Family": "sierra-data", "Revision": "42", "AvailabilityZone": "us-east-1b",
				"LaunchType": "FARGATE",
			})
		default:
			http.NotFound(w, r)
		}
	})
}

// containerDoc is a v4 container document. restarts < 0 omits RestartCount.
func containerDoc(startedAt time.Time, restarts int) map[string]any {
	doc := map[string]any{
		"DockerId": "cafe01", "Name": "relay", "DockerName": "ecs-sierra-relay",
		"ContainerARN": "arn:aws:ecs:us-east-1:1:container/project-hub/t1/c1",
		"KnownStatus":  "RUNNING", "StartedAt": startedAt.Format(time.RFC3339Nano),
	}
	if restarts >= 0 {
		doc["RestartCount"] = restarts
	}
	return doc
}

func startECS(t *testing.T, uri string) (*Controller, *harness) {
	t.Helper()
	t.Setenv(ecsMetadataEnv, "")
	c, err := New(context.Background(), &fakeService{}, Options{
		Service: "test", Addr: "127.0.0.1:0", TokenSha256: tokenHash(testToken), Standby: true,
		ECSMetadataURI: uri,
	})
	require.NoError(t, err)
	c.ecsRefreshEvery = 10 * time.Millisecond
	require.NoError(t, c.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, c: c, base: "http://" + c.ControlAddr(), cancel: cancel, done: make(chan struct{})}
	go func() { h.exitCode = c.Run(ctx); close(h.done) }()
	t.Cleanup(func() { cancel(); <-h.done })
	return c, h
}

func rawStatus(t *testing.T, h *harness) string {
	t.Helper()
	resp, err := http.Get(h.base + "/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func TestStatusReportsECSIdentity(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	f := &fakeECS{container: func(int) map[string]any { return containerDoc(started, 2) }}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")

	d := h.waitFor(func(d StatusDoc) bool { return d.ECS.Available })
	assert.Equal(t, "arn:aws:ecs:us-east-1:1:task/project-hub/t1", d.ECS.TaskARN)
	assert.Equal(t, "project-hub", d.ECS.Cluster)
	assert.Equal(t, "sierra-data", d.ECS.Family)
	assert.Equal(t, "42", d.ECS.Revision)
	assert.Equal(t, "us-east-1b", d.ECS.AvailabilityZone)
	assert.Equal(t, "relay", d.ECS.ContainerName)
	assert.Equal(t, "cafe01", d.ECS.DockerID)
	require.NotNil(t, d.ECS.RestartCount)
	assert.Equal(t, 2, *d.ECS.RestartCount)
	require.NotNil(t, d.ECS.FetchedAt)
	assert.Empty(t, d.ECS.Reason)
	assert.Empty(t, d.ECS.LastError)
}

func TestUnreportedRestartCountIsNullNotZero(t *testing.T) {
	f := &fakeECS{container: func(int) map[string]any { return containerDoc(time.Now(), -1) }}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")
	h.waitFor(func(d StatusDoc) bool { return d.ECS.Available })
	assert.Contains(t, rawStatus(t, h), `"restartCount":null`, "unknown must read as unknown, never as 0 restarts")
}

// The regression from project-hub docs/sierra-control-runtime-check.md: on a
// Fargate in-place restart, ECS kept the ORIGINAL container start time (102s
// before the new process) and only RestartCount moved. The first version
// accepted the predecessor's reading as "bound" from start-time proximity and
// stopped reading after one call. Now nothing is inferred from the timestamp,
// and reading never stops: a restart count that moves after the first read
// must reach /status.
func TestRecentPredecessorMetadataIsNotTrustedAndKeepsRefreshing(t *testing.T) {
	originalStart := time.Now().Add(-90 * time.Second) // what Fargate kept reporting
	f := &fakeECS{container: func(n int) map[string]any {
		if n <= 3 {
			return containerDoc(originalStart, 0) // ECS has not caught up with the restart
		}
		return containerDoc(originalStart, 1)
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")

	d := h.waitFor(func(d StatusDoc) bool { return d.ECS.RestartCount != nil && *d.ECS.RestartCount == 1 })
	assert.GreaterOrEqual(t, int(f.calls.Load()), 4, "reading continued past the first reading")
	assert.Equal(t, originalStart.UTC().Truncate(time.Microsecond), d.ECS.ContainerStartedAt.UTC().Truncate(time.Microsecond),
		"the start time is reported as ECS gives it, not interpreted")
	raw := rawStatus(t, h)
	assert.NotContains(t, raw, "boundToProcess", "no field may claim the metadata identifies this process")
	assert.NotContains(t, raw, "bound")

	before := f.calls.Load()
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, f.calls.Load(), before, "it keeps refreshing for the life of the process")
}

// A failed refresh keeps the last good reading, marked stale.
func TestFailedRefreshKeepsLastGoodReading(t *testing.T) {
	f := &fakeECS{container: func(int) map[string]any { return containerDoc(time.Now(), 3) }}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")
	good := h.waitFor(func(d StatusDoc) bool { return d.ECS.Available })

	f.fail.Store(true)
	d := h.waitFor(func(d StatusDoc) bool { return d.ECS.LastError != "" })
	assert.True(t, d.ECS.Available, "the last good reading is still reported")
	assert.Equal(t, good.ECS.TaskARN, d.ECS.TaskARN)
	require.NotNil(t, d.ECS.RestartCount)
	assert.Equal(t, 3, *d.ECS.RestartCount)
	assert.Contains(t, d.ECS.LastError, "HTTP 500")
	assert.True(t, d.ECS.LastAttemptAt.After(*d.ECS.FetchedAt), "a reader can see the values are older than the last attempt")

	f.fail.Store(false)
	h.waitFor(func(d StatusDoc) bool { return d.ECS.LastError == "" })
}

func TestECSUnavailableIsExplicit(t *testing.T) {
	t.Run("not on ECS", func(t *testing.T) {
		_, h := startECS(t, "")
		d := h.waitFor(func(d StatusDoc) bool { return d.ECS.Reason != "not read yet" })
		assert.False(t, d.ECS.Available)
		assert.Contains(t, d.ECS.Reason, ecsMetadataEnv)
		assert.Nil(t, d.ECS.RestartCount)
	})
	t.Run("endpoint down", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close() // nothing listening
		_, h := startECS(t, srv.URL+"/v4/abc")
		d := h.waitFor(func(d StatusDoc) bool { return d.ECS.Reason != "not read yet" })
		assert.False(t, d.ECS.Available)
		assert.True(t, strings.HasPrefix(d.ECS.Reason, "container metadata:"), d.ECS.Reason)
		assert.Equal(t, d.ECS.Reason, d.ECS.LastError)
	})
}
