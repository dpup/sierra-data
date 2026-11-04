package handoff

import (
	"context"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Gate admits production requests until it is closed, and counts the ones in
// flight so quiescence can wait for them. Every route that can reach a
// database goes through one.
//
// It exists because prefab owns the production listener and offers no way to
// stop it short of a signal, so the listener stays bound after a programmatic
// quiesce. Closing the gate is what stops admitting: every gated route then
// answers 503 (gRPC Unavailable). The database guarantee does not depend on
// the gate's coverage, though: once quiescence closes the store, any straggler
// gets "database is closed", never a query.
//
// A hijacked websocket counts as in flight until its handler returns, so the
// relay's sessions hold a drain open until the relay has closed them.
type Gate struct {
	mu       sync.Mutex
	closed   bool
	inFlight int
	idle     chan struct{} // closed while inFlight == 0; replaced when it rises

	cancelCtx context.Context // cancelled to abort in-flight requests
	cancel    context.CancelFunc
}

// NewGate returns an open gate.
func NewGate() *Gate {
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &Gate{idle: idle, cancelCtx: ctx, cancel: cancel}
}

// enter admits one request, returning its context and the function that
// retires it, or ok=false when the gate is closed.
func (g *Gate) enter(parent context.Context) (ctx context.Context, done func(), ok bool) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, nil, false
	}
	if g.inFlight == 0 {
		g.idle = make(chan struct{})
	}
	g.inFlight++
	g.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(g.cancelCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
		g.mu.Lock()
		g.inFlight--
		if g.inFlight == 0 {
			close(g.idle)
		}
		g.mu.Unlock()
	}, true
}

// Handler gates h. A closed gate answers 503 with Retry-After, so a client
// retries against whichever task now holds the database.
func (g *Gate) Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, done, ok := g.enter(r.Context())
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "service is handing over to a new instance; retry", http.StatusServiceUnavailable)
			return
		}
		defer done()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HandlerFunc gates f.
func (g *Gate) HandlerFunc(f func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return g.Handler(http.HandlerFunc(f)).ServeHTTP
}

// Enter is the low-level form for handler shapes Handler cannot wrap (a
// JSON handler that returns a value). The caller must call done exactly once.
func (g *Gate) Enter(parent context.Context) (ctx context.Context, done func(), ok bool) {
	return g.enter(parent)
}

// UnaryInterceptor gates gRPC calls. The gRPC-Gateway reaches the services
// over a loopback gRPC connection, so this covers every gateway-routed RPC.
func (g *Gate) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, done, ok := g.enter(ctx)
		if !ok {
			return nil, status.Error(codes.Unavailable, "service is handing over to a new instance; retry")
		}
		defer done()
		return handler(ctx, req)
	}
}

// Close stops admitting. Idempotent; a closed gate never reopens.
func (g *Gate) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// Closed reports whether the gate has stopped admitting.
func (g *Gate) Closed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// InFlight is the number of admitted requests that have not finished.
func (g *Gate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight
}

func (g *Gate) idleCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.idle
}

// DrainResult says how a drain ended.
type DrainResult struct {
	// InFlight is how many requests were running when the drain began.
	InFlight int
	// Cancelled is how many were still running at the grace deadline and had
	// their contexts cancelled.
	Cancelled int
	// Remaining is how many were still running when Drain gave up. Nonzero
	// means a handler ignored cancellation.
	Remaining int
	Duration  time.Duration
}

// Drain closes the gate and waits for in-flight requests: up to grace for
// them to finish on their own, then it cancels their contexts and waits up to
// cancelWait more. Remaining > 0 in the result means some did not stop; the
// caller decides whether that blocks the handoff.
func (g *Gate) Drain(grace, cancelWait time.Duration) DrainResult {
	start := time.Now()
	g.Close()
	res := DrainResult{InFlight: g.InFlight()}
	if waitClosed(g.idleCh(), grace) {
		res.Duration = time.Since(start)
		return res
	}
	res.Cancelled = g.InFlight()
	g.cancel()
	waitClosed(g.idleCh(), cancelWait)
	res.Remaining = g.InFlight()
	res.Duration = time.Since(start)
	return res
}

func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}
