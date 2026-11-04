package handoff

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dpup/prefab/logging"
)

// State is a lifecycle state. See the package doc for the transitions.
type State string

const (
	StateStandby    State = "STANDBY"
	StateActivating State = "ACTIVATING"
	StateActive     State = "ACTIVE"
	StateQuiescing  State = "QUIESCING"
	StateQuiesced   State = "QUIESCED"
)

// Service is what the controller drives.
type Service interface {
	// Activate opens the database and starts production service. It must
	// honour ctx (SIGTERM cancels a running activation) and, on ANY error,
	// release everything it opened before returning: a failed activation goes
	// back to STANDBY, which promises the database is not held. If it cannot
	// release, it must wrap ErrStillHeld. ctx ends when Activate returns, so
	// nothing long-lived may hang off it.
	Activate(ctx context.Context) error
	// Quiesce stops admitting work, stops every database user, closes the
	// database and releases its lock. nil must mean all of that happened. It
	// may be called again after a failure, so each step must be idempotent.
	Quiesce(ctx context.Context) error
}

// Exiter is optionally implemented by a Service with process-exit cleanup
// that is not part of releasing the database (waiting for prefab's own
// signal-driven shutdown, say). Called once, after the final quiescence.
type Exiter interface {
	Exit(ctx context.Context)
}

// Options configure a Controller.
type Options struct {
	// Service names the process in /status ("grid", "mesh-relay").
	Service string
	// Addr is the control listener's address (":8081"). Empty with Standby
	// unset runs without a control plane: the legacy lifecycle, plus the
	// shared SIGTERM quiescence.
	Addr string
	// TokenSha256 is the hex SHA-256 of the deployment token that POST
	// /activate and /quiesce require. Required whenever Addr is set.
	TokenSha256 string
	// Standby starts in STANDBY and waits for POST /activate. Without it the
	// process activates at boot, as it always has.
	Standby bool
	// ECSMetadataURI overrides ECS_CONTAINER_METADATA_URI_V4 (tests).
	ECSMetadataURI string
}

// Operation is one activation or quiescence.
type Operation struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`   // activate | quiesce
	Origin     string     `json:"origin"` // api | boot | signal
	Status     string     `json:"status"` // running | succeeded | failed
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`
	Error      string     `json:"error,omitempty"`
}

const (
	opRunning   = "running"
	opSucceeded = "succeeded"
	opFailed    = "failed"
)

// Controller owns a process's lifecycle state.
type Controller struct {
	svc  Service
	opts Options
	ctx  context.Context // logger
	tok  []byte          // decoded TokenSha256

	instanceID string
	startedAt  time.Time
	hostname   string
	revision   string

	mu           sync.Mutex
	state        State
	held         bool // the database may be open (conservative)
	op           *Operation
	opDone       chan struct{} // closed when op finishes
	activatedBy  string
	quiescedBy   string
	cancelActive context.CancelFunc // cancels a running activation

	ln     net.Listener
	server *http.Server
	now    func() time.Time
	fail   chan error // Fail's signal to Run; buffered 1

	ecs           ECSIdentity // guarded by mu; see watchECSIdentity
	ecsRetryFor   time.Duration
	ecsRetryEvery time.Duration
}

// New validates opts and returns a controller in its initial state. It does
// not start anything; Run does.
func New(ctx context.Context, svc Service, opts Options) (*Controller, error) {
	c := &Controller{
		svc: svc, opts: opts, ctx: logging.EnsureLogger(ctx),
		instanceID: randomID(), startedAt: time.Now(), now: time.Now,
		state: StateActivating, fail: make(chan error, 1),
		ecs:         ECSIdentity{Reason: "not read yet"},
		ecsRetryFor: 2 * time.Minute, ecsRetryEvery: 2 * time.Second,
	}
	if opts.Standby {
		c.state = StateStandby
	}
	c.hostname, _ = os.Hostname()
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				c.revision = s.Value
			}
		}
	}
	if opts.Standby && opts.Addr == "" {
		return nil, errors.New("handoff: --standby needs a control listener (set the control port and tokenSha256)")
	}
	if opts.Addr != "" {
		h := strings.ToLower(strings.TrimSpace(opts.TokenSha256))
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != sha256.Size {
			return nil, errors.New("handoff: the control tokenSha256 must be 64 hex characters (the SHA-256 of the deployment token)")
		}
		c.tok = b
	}
	return c, nil
}

// Listen binds the control listener, so a caller can report its address
// before Run. Run calls it if the caller has not.
func (c *Controller) Listen() error {
	if c.opts.Addr == "" || c.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", c.opts.Addr)
	if err != nil {
		return fmt.Errorf("handoff: control listener: %w", err)
	}
	c.ln = ln
	return nil
}

// ControlAddr is the bound control address ("" without a control plane).
func (c *Controller) ControlAddr() string {
	if c.ln == nil {
		return ""
	}
	return c.ln.Addr().String()
}

// Run serves the control API, activates at boot unless in standby, and
// blocks until SIGTERM/SIGINT (or ctx), then quiesces and returns the process
// exit code: 0 when the database ended released, 1 otherwise.
func (c *Controller) Run(ctx context.Context) int {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	if err := c.Listen(); err != nil {
		logging.Errorw(c.ctx, "Control listener failed", "error", err)
		return 1
	}
	ecsCtx, stopECS := context.WithCancel(c.ctx)
	defer stopECS()
	go c.watchECSIdentity(ecsCtx)
	if c.ln != nil {
		c.server = &http.Server{Handler: c.routes(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := c.server.Serve(c.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logging.Errorw(c.ctx, "Control listener stopped", "error", err)
			}
		}()
		logging.Infow(c.ctx, "Deployment control listener ready", "addr", c.ControlAddr(),
			"service", c.opts.Service, "instanceId", c.instanceID)
	}

	var bootDone <-chan struct{}
	if c.opts.Standby {
		logging.Infow(c.ctx, "Standby preparation complete; waiting for activation",
			"service", c.opts.Service, "sinceProcessStart", time.Since(c.startedAt))
	} else {
		// Legacy boot: activate now. Asynchronous only so a SIGTERM can cancel
		// it; a failure is still fatal, as it always was.
		c.mu.Lock()
		c.state = StateStandby
		op, actCtx := c.beginLocked("activate", "boot", "boot")
		bootDone = c.opDone
		c.mu.Unlock()
		go func() { _ = c.runActivate(actCtx, op) }()
	}

	var sig os.Signal
	var fatal error
wait:
	for {
		select {
		case sig = <-sigs:
			break wait
		case <-ctx.Done():
			break wait
		case fatal = <-c.fail:
			logging.Errorw(c.ctx, "Service failed; shutting down", "error", fatal, "state", c.State())
			break wait
		case <-bootDone:
			bootDone = nil
			if c.State() != StateActive {
				c.closeControl()
				return 1
			}
		}
	}
	logging.Infow(c.ctx, "Shutdown requested", "signal", fmt.Sprint(sig), "state", c.State())
	go func() {
		if s, ok := <-sigs; ok {
			logging.Warnw(c.ctx, "Second signal during shutdown; exiting now", "signal", fmt.Sprint(s))
			os.Exit(1)
		}
	}()
	code := c.shutdown()
	if fatal != nil {
		code = 1 // released or not, a failed service exits non-zero
	}
	if ex, ok := c.svc.(Exiter); ok {
		exitCtx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		ex.Exit(exitCtx)
		cancel()
	}
	c.closeControl()
	logging.Infow(c.ctx, "Process exiting", "service", c.opts.Service, "state", c.State(),
		"databaseHeld", c.Held(), "exitCode", code)
	return code
}

// Fail tells the controller the service can no longer serve (its production
// server stopped after activation). Run then quiesces, as on SIGTERM, and
// exits 1: the pre-handoff main's log.Fatalf("Server failed") behaviour, but
// with the database released first. Only the first call counts.
func (c *Controller) Fail(err error) {
	select {
	case c.fail <- err:
	default:
	}
}

// shutdown drives whatever state we are in to QUIESCED (or as far as it can).
func (c *Controller) shutdown() int {
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		switch c.state {
		case StateStandby:
			c.fenceLocked("signal")
			c.mu.Unlock()
			return 0
		case StateQuiesced:
			c.mu.Unlock()
			return 0
		case StateActivating:
			// Abort it: the coordinator never got an ACTIVE, so nothing is
			// lost by not finishing. Activate releases on cancellation.
			if c.cancelActive != nil {
				c.cancelActive()
			}
			done := c.opDone
			c.mu.Unlock()
			<-done
			attempt-- // the state moved on; this was not a quiesce attempt
			continue
		case StateQuiescing:
			if c.op != nil && c.op.Status == opRunning {
				done := c.opDone
				c.mu.Unlock()
				<-done
				attempt--
				continue
			}
			// A failed quiescence: try again, once per attempt.
			op, _ := c.beginLocked("quiesce", "signal", "")
			c.mu.Unlock()
			c.runQuiesce(op)
		case StateActive:
			op, _ := c.beginLocked("quiesce", "signal", "")
			c.mu.Unlock()
			c.runQuiesce(op)
		}
	}
	if c.Held() {
		logging.Errorw(c.ctx, "Exiting with the database possibly still held; process exit releases the lock",
			"state", c.State())
		return 1
	}
	return 0
}

func (c *Controller) closeControl() {
	if c.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.server.Shutdown(ctx)
}

// State is the current lifecycle state.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Held reports whether the database may be open.
func (c *Controller) Held() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

// beginLocked records a new operation and moves to its in-progress state.
// Caller holds mu and has checked the transition is legal. For an activation
// it returns the context to run it under, which shutdown can cancel.
func (c *Controller) beginLocked(kind, origin, id string) (*Operation, context.Context) {
	if id == "" {
		id = origin + "-" + randomID()
	}
	op := &Operation{ID: id, Kind: kind, Origin: origin, Status: opRunning, StartedAt: c.now()}
	c.op = op
	c.opDone = make(chan struct{})
	// Derived from c.ctx, never context.Background(): the service logs
	// through this context, and prefab's logging panics on a context with no
	// logger. (It shipped that way once; the first mesh log line at boot
	// crash-looped production.)
	ctx := c.ctx
	switch kind {
	case "activate":
		c.state = StateActivating
		c.held = true // conservative from the first open attempt
		ctx, c.cancelActive = context.WithCancel(ctx)
	case "quiesce":
		c.state = StateQuiescing
	}
	return op, ctx
}

// finishLocked closes out op. Caller holds mu.
func (c *Controller) finishLocked(op *Operation, err error) {
	t := c.now()
	op.FinishedAt = &t
	op.DurationMs = t.Sub(op.StartedAt).Milliseconds()
	if err != nil {
		op.Status, op.Error = opFailed, err.Error()
	} else {
		op.Status = opSucceeded
	}
	close(c.opDone)
}

// runActivate performs an activation synchronously and records the outcome.
// ctx is the one beginLocked returned; shutdown cancels it.
func (c *Controller) runActivate(ctx context.Context, op *Operation) error {
	start := time.Now()
	logging.Infow(c.ctx, "Activation started", "operationId", op.ID, "origin", op.Origin)
	err := c.callService("activation", func() error { return c.svc.Activate(ctx) })

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelActive != nil {
		c.cancelActive()
		c.cancelActive = nil
	}
	if err != nil && errors.Is(err, ErrStillHeld) {
		// Activation failed AND could not release what it opened. Not STANDBY:
		// that promises the database is free. QUIESCING with a failed
		// operation is the ambiguous state the coordinator must not activate
		// past; a quiesce (or termination) is the way out.
		c.state, c.held = StateQuiescing, true
		c.finishLocked(op, err)
		logging.Errorw(c.ctx, "Activation failed and the database may still be held",
			"operationId", op.ID, "duration", time.Since(start), "error", err)
		return err
	}
	if err != nil {
		// Activate released what it opened; back to STANDBY, which promises
		// the database is not held. A retry needs a new operation id.
		c.state, c.held = StateStandby, false
		c.finishLocked(op, err)
		logging.Errorw(c.ctx, "Activation failed; database released, back in standby",
			"operationId", op.ID, "duration", time.Since(start), "error", err)
		return err
	}
	c.state, c.held, c.activatedBy = StateActive, true, op.ID
	c.finishLocked(op, nil)
	logging.Infow(c.ctx, "Activation complete; serving production",
		"operationId", op.ID, "duration", time.Since(start),
		"sinceProcessStart", time.Since(c.startedAt))
	return nil
}

// runQuiesce performs a quiescence synchronously and records the outcome.
func (c *Controller) runQuiesce(op *Operation) {
	start := time.Now()
	logging.Infow(c.ctx, "Quiescence started", "operationId", op.ID, "origin", op.Origin)
	err := c.callService("quiescence", func() error { return c.svc.Quiesce(c.ctx) })

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// Stay QUIESCING and count the database as held: an ambiguous release
		// must never let a coordinator activate a second writer.
		c.finishLocked(op, err)
		logging.Errorw(c.ctx, "Quiescence failed; the database may still be held",
			"operationId", op.ID, "duration", time.Since(start), "error", err)
		return
	}
	c.state, c.held, c.quiescedBy = StateQuiesced, false, op.ID
	c.finishLocked(op, nil)
	logging.Infow(c.ctx, "Quiescence complete; database ownership released",
		"operationId", op.ID, "duration", time.Since(start))
}

// callService runs one service step, turning a panic into an error. A panic
// leaves unknown resources open, so it wraps ErrStillHeld: the state machine
// then counts the database as held (activation: QUIESCING, failed) instead of
// either crashing the process or claiming a release that did not happen.
func (c *Controller) callService(what string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logging.Errorw(c.ctx, "Panic during "+what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("panic during %s: %v: %w", what, r, ErrStillHeld)
		}
	}()
	return f()
}

// fenceLocked moves STANDBY straight to QUIESCED: a standby that was never
// activated is told it never will be. Caller holds mu.
func (c *Controller) fenceLocked(origin string) {
	t := c.now()
	c.op = &Operation{ID: origin + "-" + randomID(), Kind: "quiesce", Origin: origin,
		Status: opSucceeded, StartedAt: t, FinishedAt: &t}
	c.opDone = make(chan struct{})
	close(c.opDone)
	c.state, c.held, c.quiescedBy = StateQuiesced, false, c.op.ID
	logging.Infow(c.ctx, "Standby fenced: will never activate", "operationId", c.op.ID)
}

// --- control API ------------------------------------------------------------

type opRequest struct {
	OperationID string `json:"operationId"`
	// ExpectedInstanceID, when set, must equal this process's instanceId or
	// the command is refused (409 instance_mismatch) before any transition.
	// A restarted container comes back at the same IP and port with a new
	// instanceId, so this is what stops a delayed command meant for the
	// previous process from acting on its successor. The coordinator always
	// sends it; a hand-run command may omit it.
	ExpectedInstanceID string `json:"expectedInstanceId"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StatusDoc is the GET /status body.
type StatusDoc struct {
	Service      string     `json:"service"`
	State        State      `json:"state"`
	Prepared     bool       `json:"prepared"`
	Ready        bool       `json:"ready"`
	DatabaseHeld bool       `json:"databaseHeld"`
	Standby      bool       `json:"standby"`
	InstanceID   string     `json:"instanceId"`
	PID          int        `json:"pid"`
	Hostname     string     `json:"hostname"`
	Revision     string     `json:"revision,omitempty"`
	StartedAt    time.Time  `json:"startedAt"`
	Operation    *Operation `json:"operation,omitempty"`
	ActivatedBy  string     `json:"activatedBy,omitempty"`
	QuiescedBy   string     `json:"quiescedBy,omitempty"`
	// ECS is the ECS task/container this process runs in (see ECSIdentity).
	ECS ECSIdentity `json:"ecs"`
}

type opResponse struct {
	State      State      `json:"state"`
	InstanceID string     `json:"instanceId"`
	Operation  *Operation `json:"operation,omitempty"`
	Error      *apiError  `json:"error,omitempty"`
}

// Status snapshots the controller.
func (c *Controller) Status() StatusDoc {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := StatusDoc{
		Service: c.opts.Service, State: c.state, Prepared: true,
		Ready: c.state == StateActive, DatabaseHeld: c.held, Standby: c.opts.Standby,
		InstanceID: c.instanceID, PID: os.Getpid(), Hostname: c.hostname, Revision: c.revision,
		StartedAt: c.startedAt, ActivatedBy: c.activatedBy, QuiescedBy: c.quiescedBy,
		ECS: c.ecs,
	}
	if c.op != nil {
		op := *c.op
		d.Operation = &op
	}
	return d
}

func (c *Controller) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, c.Status())
	})
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"live": true, "state": c.State()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		st := c.State()
		code := http.StatusServiceUnavailable
		if st == StateActive {
			code = http.StatusOK
		}
		writeJSON(w, code, map[string]any{"ready": st == StateActive, "state": st})
	})
	mux.HandleFunc("POST /activate", c.authed(c.handleActivate))
	mux.HandleFunc("POST /quiesce", c.authed(c.handleQuiesce))
	return mux
}

func (c *Controller) authed(h func(http.ResponseWriter, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer ")
		sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
		if !ok || subtle.ConstantTimeCompare(sum[:], c.tok) != 1 {
			writeJSON(w, http.StatusUnauthorized, opResponse{State: c.State(), InstanceID: c.instanceID,
				Error: &apiError{Code: "unauthenticated", Message: "a valid deployment token is required"}})
			return
		}
		var req opRequest
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeJSON(w, http.StatusBadRequest, opResponse{State: c.State(), InstanceID: c.instanceID,
					Error: &apiError{Code: "bad_request",
						Message: "body must be {\"operationId\": \"...\", \"expectedInstanceId\": \"...\"}"}})
				return
			}
		}
		if len(req.OperationID) > 128 {
			writeJSON(w, http.StatusBadRequest, opResponse{State: c.State(), InstanceID: c.instanceID,
				Error: &apiError{Code: "bad_request", Message: "operationId is too long"}})
			return
		}
		// Before any state is read for a transition: a command addressed to a
		// different incarnation never reaches the handlers, not even as an
		// idempotent replay.
		if req.ExpectedInstanceID != "" && req.ExpectedInstanceID != c.instanceID {
			logging.Warnw(c.ctx, "Refused a control command addressed to another process incarnation",
				"path", r.URL.Path, "operationId", req.OperationID,
				"expectedInstanceId", req.ExpectedInstanceID, "instanceId", c.instanceID)
			writeJSON(w, http.StatusConflict, opResponse{State: c.State(), InstanceID: c.instanceID,
				Error: &apiError{Code: "instance_mismatch",
					Message: "expectedInstanceId " + req.ExpectedInstanceID + " is not this process (" + c.instanceID + ")"}})
			return
		}
		h(w, req.OperationID)
	}
}

func (c *Controller) handleActivate(w http.ResponseWriter, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case StateStandby:
		if c.op != nil && c.op.Kind == "activate" && c.op.ID == id && id != "" {
			// A replay of the activation that failed: same id, same answer.
			c.conflictLocked(w, "operation_failed", "this operation already failed; retry with a new operationId")
			return
		}
		op, ctx := c.beginLocked("activate", "api", id)
		go func() { _ = c.runActivate(ctx, op) }()
		c.replyLocked(w, http.StatusAccepted)
	case StateActivating:
		if id == "" || id == c.op.ID {
			c.replyLocked(w, http.StatusAccepted)
			return
		}
		c.conflictLocked(w, "activation_in_progress", "activation "+c.op.ID+" is in progress")
	case StateActive:
		if id == "" || id == c.activatedBy {
			c.replyLocked(w, http.StatusOK)
			return
		}
		c.conflictLocked(w, "already_active", "already active under operation "+c.activatedBy)
	default:
		c.conflictLocked(w, "released", "this process has released its database and never reactivates")
	}
}

func (c *Controller) handleQuiesce(w http.ResponseWriter, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case StateStandby:
		c.fenceLocked("api")
		c.replyLocked(w, http.StatusOK)
	case StateActivating:
		c.conflictLocked(w, "activation_in_progress", "activation "+c.op.ID+" is in progress; wait for it to finish")
	case StateActive:
		op, _ := c.beginLocked("quiesce", "api", id)
		go c.runQuiesce(op)
		c.replyLocked(w, http.StatusAccepted)
	case StateQuiescing:
		if c.op.Status == opRunning {
			if id == "" || id == c.op.ID {
				c.replyLocked(w, http.StatusAccepted)
				return
			}
			c.conflictLocked(w, "quiescence_in_progress", "quiescence "+c.op.ID+" is in progress")
			return
		}
		// The last attempt failed and the database may still be held. A new
		// id retries; the failed id gets its failure back.
		if id != "" && id == c.op.ID {
			c.conflictLocked(w, "operation_failed", "this operation already failed; retry with a new operationId")
			return
		}
		op, _ := c.beginLocked("quiesce", "api", id)
		go c.runQuiesce(op)
		c.replyLocked(w, http.StatusAccepted)
	case StateQuiesced:
		// Release is a terminal fact; confirming it to any caller is safe.
		c.replyLocked(w, http.StatusOK)
	}
}

func (c *Controller) replyLocked(w http.ResponseWriter, code int) {
	var op *Operation
	if c.op != nil {
		cp := *c.op
		op = &cp
	}
	writeJSON(w, code, opResponse{State: c.state, InstanceID: c.instanceID, Operation: op})
}

func (c *Controller) conflictLocked(w http.ResponseWriter, code, msg string) {
	var op *Operation
	if c.op != nil {
		cp := *c.op
		op = &cp
	}
	writeJSON(w, http.StatusConflict, opResponse{State: c.state, InstanceID: c.instanceID, Operation: op,
		Error: &apiError{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
