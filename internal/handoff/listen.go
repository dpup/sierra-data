package handoff

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrStillHeld marks an activation error whose cleanup could not release the
// database. The controller then keeps the database counted as held (state
// QUIESCING, failed) instead of returning to STANDBY, so the coordinator sees
// an ambiguous handoff rather than a clean refusal.
var ErrStillHeld = errors.New("handoff: the database could not be released")

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
