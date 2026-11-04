package handoff

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/dpup/prefab/logging"
)

// ErrStillHeld marks an activation error whose cleanup could not release the
// database. The controller then keeps the database counted as held (state
// QUIESCING, failed) instead of returning to STANDBY, so the coordinator sees
// an ambiguous handoff rather than a clean refusal.
var ErrStillHeld = errors.New("handoff: the database could not be released")

// PortFree reports an error if the TCP port is already bound on this host's
// network namespace. Activation checks it before starting its server, so it
// never mistakes another process's listener (a released predecessor keeps its
// port bound) for its own.
func PortFree(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("handoff: production port %d is already in use: %w", port, err)
	}
	return ln.Close()
}

// WaitListening blocks until something accepts TCP connections on
// 127.0.0.1:port, the server exits (exited is closed), ctx ends, or timeout
// passes. prefab's Start binds its listener on its own goroutine and reports
// nothing until it stops, so this is how an activation knows production is
// actually being served before it reports ACTIVE.
func WaitListening(ctx context.Context, port int, exited <-chan struct{}, timeout time.Duration) error {
	if port <= 0 {
		return fmt.Errorf("handoff: no production port configured")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for {
		// Exited first: a dial that succeeds might be reaching someone else's
		// listener (in a shared network namespace, a released predecessor
		// keeps its port bound), so it proves nothing once our server is gone.
		select {
		case <-exited:
			return fmt.Errorf("handoff: server exited before listening on %s", addr)
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("handoff: server exited before listening on %s", addr)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("handoff: nothing listening on %s after %s", addr, timeout)
		}
	}
}

// WaitClosed waits up to d for ch to close and reports whether it did. A
// closed channel always wins, even with d <= 0.
func WaitClosed(ch <-chan struct{}, d time.Duration) bool { return waitClosed(ch, d) }

// WithLoggerOf returns ctx carrying the prefab logger of from (or a default
// one when from has none). A service re-attaches its process logger to the
// context the controller hands it with this, so a context without a logger,
// which panics in prefab's logging, can never reach it.
func WithLoggerOf(ctx, from context.Context) context.Context {
	return logging.With(ctx, logging.FromContext(logging.EnsureLogger(from)))
}
