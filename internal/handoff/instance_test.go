package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// fakeECS serves the two v4 metadata documents. startedAt returns the
// container start time for the n-th container request (to model lag).
type fakeECS struct {
	calls     atomic.Int32
	startedAt func(n int) *time.Time
	restarts  *int
}

func (f *fakeECS) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v4/abc":
			n := int(f.calls.Add(1))
			doc := map[string]any{
				"DockerId": "cafe01", "Name": "grid", "DockerName": "ecs-sierra-grid",
				"ContainerARN": "arn:aws:ecs:us-east-1:1:container/project-hub/t1/c1",
				"KnownStatus":  "RUNNING",
			}
			if ts := f.startedAt(n); ts != nil {
				doc["StartedAt"] = ts.Format(time.RFC3339Nano)
			}
			if f.restarts != nil {
				doc["RestartCount"] = *f.restarts
			}
			_ = json.NewEncoder(w).Encode(doc)
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

func startECS(t *testing.T, uri string) (*Controller, *harness) {
	t.Helper()
	t.Setenv(ecsMetadataEnv, "")
	c, err := New(context.Background(), &fakeService{}, Options{
		Service: "test", Addr: "127.0.0.1:0", TokenSha256: tokenHash(testToken), Standby: true,
		ECSMetadataURI: uri,
	})
	require.NoError(t, err)
	c.ecsRetryEvery = 10 * time.Millisecond
	c.ecsRetryFor = 3 * time.Second
	require.NoError(t, c.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, c: c, base: "http://" + c.ControlAddr(), cancel: cancel, done: make(chan struct{})}
	go func() { h.exitCode = c.Run(ctx); close(h.done) }()
	t.Cleanup(func() { cancel(); <-h.done })
	return c, h
}

func TestStatusReportsBoundECSIdentity(t *testing.T) {
	two := 2
	f := &fakeECS{restarts: &two}
	base := time.Now() // the process starts just after this
	f.startedAt = func(int) *time.Time { ts := base.Add(-3 * time.Second); return &ts }
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")

	d := h.waitFor(func(d StatusDoc) bool { return d.ECS.BoundToProcess })
	assert.True(t, d.ECS.Available)
	assert.Equal(t, "arn:aws:ecs:us-east-1:1:task/project-hub/t1", d.ECS.TaskARN)
	assert.Equal(t, "project-hub", d.ECS.Cluster)
	assert.Equal(t, "sierra-data", d.ECS.Family)
	assert.Equal(t, "42", d.ECS.Revision)
	assert.Equal(t, "us-east-1b", d.ECS.AvailabilityZone)
	assert.Equal(t, "grid", d.ECS.ContainerName)
	assert.Equal(t, "cafe01", d.ECS.DockerID)
	require.NotNil(t, d.ECS.RestartCount)
	assert.Equal(t, 2, *d.ECS.RestartCount)
	assert.Empty(t, d.ECS.Reason)
}

func TestUnreportedRestartCountIsNullNotZero(t *testing.T) {
	f := &fakeECS{}
	base := time.Now()
	f.startedAt = func(int) *time.Time { ts := base.Add(-time.Second); return &ts }
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")
	h.waitFor(func(d StatusDoc) bool { return d.ECS.BoundToProcess })

	resp, err := http.Get(h.base + "/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(raw), `"restartCount":null`, "unknown must read as unknown, never as 0 restarts")
}

// ECS can lag a container restart: metadata still describing the previous
// incarnation is not bound, and is re-read until it catches up.
func TestStaleECSMetadataIsRetriedUntilBound(t *testing.T) {
	f := &fakeECS{}
	base := time.Now()
	f.startedAt = func(n int) *time.Time {
		ts := base.Add(-time.Second)
		if n <= 3 {
			ts = base.Add(-time.Hour) // the previous incarnation's start
		}
		return &ts
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	_, h := startECS(t, srv.URL+"/v4/abc")

	d := h.waitFor(func(d StatusDoc) bool { return d.ECS.BoundToProcess })
	assert.GreaterOrEqual(t, int(f.calls.Load()), 4, "the stale readings were retried")
	assert.Empty(t, d.ECS.Reason)
}

func TestECSUnavailableIsExplicit(t *testing.T) {
	t.Run("not on ECS", func(t *testing.T) {
		_, h := startECS(t, "")
		d := h.waitFor(func(d StatusDoc) bool { return d.ECS.Reason != "not read yet" })
		assert.False(t, d.ECS.Available)
		assert.False(t, d.ECS.BoundToProcess)
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
	})
}

func TestECSBindWindow(t *testing.T) {
	proc := time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		started time.Time
		bound   bool
	}{
		{"just before the process", proc.Add(-5 * time.Second), true},
		{"at the window edge", proc.Add(-ecsBindWindow), true},
		{"before the window: a previous incarnation", proc.Add(-ecsBindWindow - time.Second), false},
		{"within clock skew after", proc.Add(ecsBindSkew), true},
		{"after the process", proc.Add(ecsBindSkew + time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := tc.started
			srv := httptest.NewServer((&fakeECS{startedAt: func(int) *time.Time { return &ts }}).handler(t))
			defer srv.Close()
			id := fetchECSIdentity(context.Background(), srv.URL+"/v4/abc", proc, proc.Add(time.Minute))
			assert.True(t, id.Available)
			assert.Equal(t, tc.bound, id.BoundToProcess, id.Reason)
			assert.Equal(t, tc.bound, id.Reason == "", fmt.Sprintf("an unbound reading says why: %q", id.Reason))
		})
	}
}
