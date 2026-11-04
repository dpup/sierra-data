package handoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "deploy-token-for-tests"

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// fakeService records calls and lets a test hold either step open or fail it.
type fakeService struct {
	mu            sync.Mutex
	activations   int
	quiescences   int
	activateErr   []error // consumed in order; nil = succeed
	quiesceErr    []error
	activateBlock chan struct{} // when set, Activate waits for it (or ctx)
	quiesceBlock  chan struct{}
	open          atomic.Bool // "the database is open"
	exited        atomic.Bool
}

func (f *fakeService) Activate(ctx context.Context) error {
	f.mu.Lock()
	f.activations++
	var err error
	if len(f.activateErr) > 0 {
		err, f.activateErr = f.activateErr[0], f.activateErr[1:]
	}
	block := f.activateBlock
	f.mu.Unlock()
	f.open.Store(true)
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.open.Store(false)
			return ctx.Err()
		}
	}
	if err != nil && !errors.Is(err, ErrStillHeld) {
		f.open.Store(false)
	}
	return err
}

func (f *fakeService) Quiesce(context.Context) error {
	f.mu.Lock()
	f.quiescences++
	var err error
	if len(f.quiesceErr) > 0 {
		err, f.quiesceErr = f.quiesceErr[0], f.quiesceErr[1:]
	}
	block := f.quiesceBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err == nil {
		f.open.Store(false)
	}
	return err
}

func (f *fakeService) Exit(context.Context) { f.exited.Store(true) }

func (f *fakeService) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activations, f.quiescences
}

// harness runs a controller with a control listener on a free port.
type harness struct {
	t        *testing.T
	c        *Controller
	svc      *fakeService
	base     string
	cancel   context.CancelFunc // stands in for SIGTERM
	exitCode int
	done     chan struct{} // closed when Run returns; code is then set
}

func start(t *testing.T, svc *fakeService, standby bool) *harness {
	t.Helper()
	c, err := New(context.Background(), svc, Options{
		Service: "test", Addr: "127.0.0.1:0", TokenSha256: tokenHash(testToken), Standby: standby,
	})
	require.NoError(t, err)
	require.NoError(t, c.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, c: c, svc: svc, base: "http://" + c.ControlAddr(), cancel: cancel, done: make(chan struct{})}
	go func() { h.exitCode = c.Run(ctx); close(h.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
	})
	h.waitFor(func(d StatusDoc) bool { return true })
	return h
}

func (h *harness) status() StatusDoc {
	h.t.Helper()
	resp, err := http.Get(h.base + "/status")
	if err != nil {
		return StatusDoc{}
	}
	defer resp.Body.Close()
	var d StatusDoc
	require.NoError(h.t, json.NewDecoder(resp.Body).Decode(&d))
	return d
}

func (h *harness) waitFor(cond func(StatusDoc) bool) StatusDoc {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := h.status()
		if d.State != "" && cond(d) {
			return d
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("condition not met; last status %+v", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) post(path, token, opID string) (int, opResponse) {
	h.t.Helper()
	var body bytes.Buffer
	if opID != "" {
		_ = json.NewEncoder(&body).Encode(opRequest{OperationID: opID})
	}
	req, _ := http.NewRequest(http.MethodPost, h.base+path, &body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	var out opResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (h *harness) code(path string) int {
	h.t.Helper()
	resp, err := http.Get(h.base + path)
	require.NoError(h.t, err)
	resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) terminate() int {
	h.t.Helper()
	h.cancel()
	select {
	case <-h.done:
		return h.exitCode
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not return after termination")
		return -1
	}
}

func TestStandbyTouchesNothingAndReportsLiveNotReady(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	d := h.status()
	assert.Equal(t, StateStandby, d.State)
	assert.True(t, d.Prepared)
	assert.False(t, d.Ready)
	assert.False(t, d.DatabaseHeld)
	assert.True(t, d.Standby)
	assert.NotEmpty(t, d.InstanceID)
	assert.NotZero(t, d.PID)
	assert.Equal(t, http.StatusOK, h.code("/livez"), "standby is live")
	assert.Equal(t, http.StatusServiceUnavailable, h.code("/readyz"), "standby is not ready")
	a, q := svc.counts()
	assert.Zero(t, a, "standby never activates on its own")
	assert.Zero(t, q)
}

func TestMutatingEndpointsNeedTheDeploymentToken(t *testing.T) {
	h := start(t, &fakeService{}, true)
	code, out := h.post("/activate", "", "op-1")
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Equal(t, "unauthenticated", out.Error.Code)
	code, _ = h.post("/activate", "wrong", "op-1")
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = h.post("/quiesce", "wrong", "op-1")
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Equal(t, StateStandby, h.status().State, "a rejected command changes nothing")
}

func TestActivateIsIdempotentAndConflictsAreExplicit(t *testing.T) {
	svc := &fakeService{activateBlock: make(chan struct{})}
	h := start(t, svc, true)

	code, out := h.post("/activate", testToken, "deploy-42")
	require.Equal(t, http.StatusAccepted, code)
	assert.Equal(t, StateActivating, out.State)
	assert.Equal(t, "deploy-42", out.Operation.ID)

	code, _ = h.post("/activate", testToken, "deploy-42")
	assert.Equal(t, http.StatusAccepted, code, "a duplicate of the running activation is accepted")
	code, out = h.post("/activate", testToken, "deploy-43")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "activation_in_progress", out.Error.Code)
	code, out = h.post("/quiesce", testToken, "deploy-43")
	assert.Equal(t, http.StatusConflict, code, "quiesce during activation is a conflict")
	assert.Equal(t, "activation_in_progress", out.Error.Code)
	assert.True(t, h.status().DatabaseHeld, "an activation in progress counts as holding the database")

	close(svc.activateBlock)
	d := h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	assert.True(t, d.Ready)
	assert.Equal(t, "deploy-42", d.ActivatedBy)
	assert.Equal(t, "succeeded", d.Operation.Status)
	assert.Equal(t, http.StatusOK, h.code("/readyz"))

	code, _ = h.post("/activate", testToken, "deploy-42")
	assert.Equal(t, http.StatusOK, code, "replaying the activation that succeeded is a no-op")
	code, out = h.post("/activate", testToken, "deploy-99")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "already_active", out.Error.Code)
	a, _ := svc.counts()
	assert.Equal(t, 1, a)
}

func TestFailedActivationReturnsToStandbyReleased(t *testing.T) {
	svc := &fakeService{activateErr: []error{errors.New("migration failed")}}
	h := start(t, svc, true)
	code, _ := h.post("/activate", testToken, "a1")
	require.Equal(t, http.StatusAccepted, code)
	d := h.waitFor(func(d StatusDoc) bool { return d.Operation != nil && d.Operation.Status == "failed" })
	assert.Equal(t, StateStandby, d.State)
	assert.False(t, d.DatabaseHeld)
	assert.Contains(t, d.Operation.Error, "migration failed")
	assert.False(t, svc.open.Load())

	code, out := h.post("/activate", testToken, "a1")
	assert.Equal(t, http.StatusConflict, code, "a replay of a failed operation gets its failure back")
	assert.Equal(t, "operation_failed", out.Error.Code)

	code, _ = h.post("/activate", testToken, "a2")
	require.Equal(t, http.StatusAccepted, code, "a new operation id retries")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
}

func TestActivationThatCannotReleaseStaysHeld(t *testing.T) {
	svc := &fakeService{activateErr: []error{errors.Join(errors.New("listener failed"), ErrStillHeld)}}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	d := h.waitFor(func(d StatusDoc) bool { return d.Operation != nil && d.Operation.Status == "failed" })
	assert.Equal(t, StateQuiescing, d.State, "not STANDBY: that would promise the database is free")
	assert.True(t, d.DatabaseHeld)
	code, _ := h.post("/activate", testToken, "a2")
	assert.Equal(t, http.StatusConflict, code)

	code, _ = h.post("/quiesce", testToken, "q1")
	require.Equal(t, http.StatusAccepted, code, "a quiesce is the way out")
	d = h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiesced })
	assert.False(t, d.DatabaseHeld)
}

func TestQuiesceReleasesAndNeverReactivates(t *testing.T) {
	svc := &fakeService{quiesceBlock: make(chan struct{})}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })

	code, out := h.post("/quiesce", testToken, "q1")
	require.Equal(t, http.StatusAccepted, code)
	assert.Equal(t, StateQuiescing, out.State)
	assert.Equal(t, http.StatusServiceUnavailable, h.code("/readyz"), "not ready once quiescing")
	code, _ = h.post("/quiesce", testToken, "q1")
	assert.Equal(t, http.StatusAccepted, code, "duplicate of the running quiesce")
	code, out = h.post("/quiesce", testToken, "q2")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "quiescence_in_progress", out.Error.Code)
	assert.True(t, h.status().DatabaseHeld, "held until the quiesce completes")

	close(svc.quiesceBlock)
	d := h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiesced })
	assert.False(t, d.DatabaseHeld)
	assert.Equal(t, "q1", d.QuiescedBy)
	assert.False(t, svc.open.Load())

	code, _ = h.post("/quiesce", testToken, "q1")
	assert.Equal(t, http.StatusOK, code, "release is terminal; confirming it is safe")
	code, out = h.post("/activate", testToken, "a9")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "released", out.Error.Code, "a released process never reopens the database")
	assert.Equal(t, http.StatusOK, h.code("/livez"), "still live: ECS cleans it up later")
	a, q := svc.counts()
	assert.Equal(t, 1, a)
	assert.Equal(t, 1, q)
}

func TestFailedQuiescenceStaysHeldAndCanRetry(t *testing.T) {
	svc := &fakeService{quiesceErr: []error{errors.New("scheduler would not stop")}}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	h.post("/quiesce", testToken, "q1")
	d := h.waitFor(func(d StatusDoc) bool { return d.Operation.Kind == "quiesce" && d.Operation.Status == "failed" })
	assert.Equal(t, StateQuiescing, d.State)
	assert.True(t, d.DatabaseHeld, "an ambiguous release counts as held")

	code, out := h.post("/quiesce", testToken, "q1")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "operation_failed", out.Error.Code)
	code, _ = h.post("/quiesce", testToken, "q2")
	require.Equal(t, http.StatusAccepted, code)
	h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiesced })
}

func TestQuiesceInStandbyFencesIt(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	code, out := h.post("/quiesce", testToken, "abort-rollout")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, StateQuiesced, out.State)
	code, _ = h.post("/activate", testToken, "a1")
	assert.Equal(t, http.StatusConflict, code, "a fenced standby never activates")
	a, q := svc.counts()
	assert.Zero(t, a)
	assert.Zero(t, q, "nothing to release: it never opened anything")
}

// --- termination (SIGTERM) in each state ---

func TestTerminateInStandbyExitsWithoutTouchingTheDatabase(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	assert.Equal(t, 0, h.terminate())
	a, q := svc.counts()
	assert.Zero(t, a)
	assert.Zero(t, q)
	assert.True(t, svc.exited.Load())
}

func TestTerminateWhileActivatingCancelsIt(t *testing.T) {
	svc := &fakeService{activateBlock: make(chan struct{})}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActivating })
	assert.Equal(t, 0, h.terminate())
	assert.False(t, svc.open.Load(), "the cancelled activation released what it opened")
	assert.Equal(t, StateQuiesced, h.c.State())
}

func TestTerminateWhileActiveQuiescesFirst(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	assert.Equal(t, 0, h.terminate())
	_, q := svc.counts()
	assert.Equal(t, 1, q)
	assert.False(t, svc.open.Load())
	assert.Equal(t, "signal", h.c.Status().Operation.Origin)
}

func TestTerminateWhileQuiescingWaitsForIt(t *testing.T) {
	svc := &fakeService{quiesceBlock: make(chan struct{})}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	h.post("/quiesce", testToken, "q1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiescing })
	go func() { time.Sleep(50 * time.Millisecond); close(svc.quiesceBlock) }()
	assert.Equal(t, 0, h.terminate())
	_, q := svc.counts()
	assert.Equal(t, 1, q, "the running quiesce is waited for, not started twice")
}

func TestTerminateWhenQuiescedIsImmediate(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	h.post("/quiesce", testToken, "q1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiesced })
	assert.Equal(t, 0, h.terminate())
	_, q := svc.counts()
	assert.Equal(t, 1, q)
}

func TestTerminateWhenReleaseKeepsFailingExitsNonZero(t *testing.T) {
	boom := errors.New("store close failed")
	svc := &fakeService{quiesceErr: []error{boom, boom, boom}}
	h := start(t, svc, true)
	h.post("/activate", testToken, "a1")
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	assert.Equal(t, 1, h.terminate(), "exit code says the release did not complete")
}

// --- legacy boot (no --standby) ---

func TestBootActivatesWithoutControlPlane(t *testing.T) {
	svc := &fakeService{}
	c, err := New(context.Background(), svc, Options{Service: "test"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- c.Run(ctx) }()
	require.Eventually(t, func() bool { return c.State() == StateActive }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "boot", c.Status().ActivatedBy)
	cancel()
	assert.Equal(t, 0, <-exit)
	_, q := svc.counts()
	assert.Equal(t, 1, q, "SIGTERM runs the same quiescence on the legacy path")
}

func TestBootActivationFailureIsFatal(t *testing.T) {
	svc := &fakeService{activateErr: []error{errors.New("cannot open")}}
	c, err := New(context.Background(), svc, Options{Service: "test"})
	require.NoError(t, err)
	assert.Equal(t, 1, c.Run(context.Background()))
}

func TestBootWithControlPlaneCanBeQuiesced(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, false)
	d := h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	assert.False(t, d.Standby)
	code, _ := h.post("/quiesce", testToken, "q1")
	require.Equal(t, http.StatusAccepted, code)
	h.waitFor(func(d StatusDoc) bool { return d.State == StateQuiesced })
}

func TestOptionsValidation(t *testing.T) {
	_, err := New(context.Background(), &fakeService{}, Options{Standby: true})
	assert.Error(t, err, "standby without a control plane could never be activated")
	_, err = New(context.Background(), &fakeService{}, Options{Addr: ":0", TokenSha256: "not-a-hash"})
	assert.Error(t, err)
	_, err = New(context.Background(), &fakeService{}, Options{Addr: ":0"})
	assert.Error(t, err, "a control plane without a token would be unauthenticated")
}

// loggingService logs through the contexts the controller hands it, as the
// real services do. prefab's logging panics on a context without a logger,
// which is how a context.Background()-based activation context crash-looped
// production the first time this shipped.
type loggingService struct{ fakeService }

func (s *loggingService) Activate(ctx context.Context) error {
	logging.Infow(ctx, "test: activating through the handed-in context")
	return s.fakeService.Activate(ctx)
}

func (s *loggingService) Quiesce(ctx context.Context) error {
	logging.Infow(ctx, "test: quiescing through the handed-in context")
	return s.fakeService.Quiesce(ctx)
}

func (s *loggingService) Exit(ctx context.Context) {
	logging.Infow(ctx, "test: exiting through the handed-in context")
	s.fakeService.Exit(ctx)
}

func TestServiceContextsCarryTheLogger(t *testing.T) {
	ctx := logging.With(context.Background(), logging.NewDevLogger())
	for _, standby := range []bool{false, true} {
		svc := &loggingService{}
		c, err := New(ctx, svc, Options{Service: "test", Addr: "127.0.0.1:0",
			TokenSha256: tokenHash(testToken), Standby: standby})
		require.NoError(t, err)
		require.NoError(t, c.Listen())
		runCtx, cancel := context.WithCancel(context.Background())
		exit := make(chan int, 1)
		go func() { exit <- c.Run(runCtx) }()
		if standby {
			h := &harness{t: t, c: c, base: "http://" + c.ControlAddr()}
			h.waitFor(func(StatusDoc) bool { return true })
			code, _ := h.post("/activate", testToken, "a1")
			require.Equal(t, http.StatusAccepted, code)
		}
		require.Eventually(t, func() bool { return c.State() == StateActive }, 5*time.Second, 5*time.Millisecond)
		cancel()
		assert.Equal(t, 0, <-exit, "standby=%v: activate, quiesce and exit all logged without panicking", standby)
		assert.True(t, svc.exited.Load())
	}
}

type panickingService struct{ fakeService }

func (s *panickingService) Activate(context.Context) error { panic("boom in activation") }

func TestPanicDuringActivationCountsAsHeld(t *testing.T) {
	svc := &panickingService{}
	h := start(t, &svc.fakeService, true)
	h.c.svc = svc
	code, _ := h.post("/activate", testToken, "a1")
	require.Equal(t, http.StatusAccepted, code)
	d := h.waitFor(func(d StatusDoc) bool { return d.Operation != nil && d.Operation.Status == "failed" })
	assert.Equal(t, StateQuiescing, d.State, "unknown resources may be open: not STANDBY")
	assert.True(t, d.DatabaseHeld)
	assert.Contains(t, d.Operation.Error, "boom in activation")
}

func TestPanicDuringBootActivationExitsNonZero(t *testing.T) {
	c, err := New(context.Background(), &panickingService{}, Options{Service: "test"})
	require.NoError(t, err)
	assert.Equal(t, 1, c.Run(context.Background()))
}

// A service whose production server dies after activation calls Fail: the
// controller quiesces (releasing the database) and the process exits 1, as
// the pre-handoff main's log.Fatalf("Server failed") did.
func TestFailQuiescesAndExitsNonZero(t *testing.T) {
	svc := &fakeService{}
	h := start(t, svc, false)
	h.waitFor(func(d StatusDoc) bool { return d.State == StateActive })
	h.c.Fail(errors.New("listener died"))
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Fail did not end Run")
	}
	assert.Equal(t, 1, h.exitCode)
	_, q := svc.counts()
	assert.Equal(t, 1, q, "the database was released before exit")
	assert.False(t, svc.open.Load())
}
