package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The single-writer guard. One process at a time may write a given database:
// Open takes a session-level advisory lock and holds it until Close.
//
// The lock lives on a DEDICATED connection, taken from the pool with Acquire
// and then Hijack()ed out of it, never on a pooled connection: a session lock
// belongs to the backend that took it, and a pooled connection would carry it
// into whatever query borrowed the connection next (and an idle-timeout
// recycle would silently drop it). The connection dying releases the lock
// server-side, the same way process death released the old flock, so a
// hard-killed task never leaves the database locked.
//
// What this is NOT yet (P3 of the migration plan, notes in internal/store/
// AGENTS.md): a lease with epoch fencing. An advisory lock alone does not stop
// a writer that has lost its session from finishing a transaction it already
// started on another connection. The heartbeat below only DETECTS the loss and
// calls the hook; P3 adds the writer_lease row checked inside every write
// transaction and the reader/writer roles.

// writerLockKey is the pg_try_advisory_lock key for the Grid writer. Advisory
// locks are scoped to the current database, so separate databases (one per
// test) never contend.
const writerLockKey int64 = 0x5349455252414731 // "SIERRAG1"

// lockHeartbeat is how often the holder checks its lock connection is alive.
const lockHeartbeat = 5 * time.Second

// DefaultLockTimeout bounds how long Open waits for the writer lock before
// failing. The contention case is a deploy whose previous writer has not
// released yet; waiting beats dying, because a task that gives up is simply
// restarted to wait again (a crash loop). The ceiling is the container's
// health-check grace when the listener opens only after the lock (today's
// handoff); WithLockContext ends the wait on SIGTERM.
const DefaultLockTimeout = 90 * time.Second

// lockAcquireTimeout is the effective default, overridden per Open with
// WithLockTimeout, and by tests.
var lockAcquireTimeout = DefaultLockTimeout

// ErrLockHeld is wrapped by Open's error when another process holds the
// writer lock for the whole wait.
var ErrLockHeld = errors.New("store: the grid writer lock is held by another process")

type writerLock struct {
	mu     sync.Mutex // serializes use of conn (a pgx.Conn is not concurrency-safe)
	conn   *pgx.Conn
	stop   chan struct{}
	done   chan struct{}
	onLost func(error)
}

// acquireWriterLock waits up to timeout (or until ctx ends) for the writer
// lock on a dedicated connection. onLost, when non-nil, is called once if the
// lock connection later fails its heartbeat: the lock is then gone and
// another process may already be writing.
func acquireWriterLock(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, onLost func(error)) (*writerLock, error) {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: connecting for the writer lock: %w", err)
	}
	conn := pc.Hijack() // ours alone from here; the pool forgets it
	start := time.Now()
	deadline := start.Add(timeout)
	announced := false
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, writerLockKey).Scan(&ok); err != nil {
			conn.Close(context.Background())
			if ctx.Err() != nil {
				return nil, fmt.Errorf("store: stopped waiting for the writer lock: %w", ctx.Err())
			}
			return nil, fmt.Errorf("store: taking the writer lock: %w", err)
		}
		if ok {
			if announced {
				// The line that tells "the deploy was slow because the previous
				// writer was draining" apart from every other slow start.
				log.Printf("store: acquired the writer lock after waiting %s for the previous writer to release it",
					time.Since(start).Round(100*time.Millisecond))
			}
			break
		}
		if !time.Now().Before(deadline) {
			conn.Close(context.Background())
			return nil, fmt.Errorf("%w: still held after %s. On a deploy this means the previous "+
				"writer has not released it yet (check that it was quiesced, and the load balancer's "+
				"deregistration delay); otherwise stop the other server or point "+
				"PF__GRID__DATABASE__URL at a different database", ErrLockHeld, timeout)
		}
		if !announced {
			log.Printf("store: the writer lock is held by another process; waiting up to %s for it "+
				"(normal during a deploy while the previous writer drains)", timeout)
			announced = true
		}
		select {
		case <-ctx.Done():
			conn.Close(context.Background())
			return nil, fmt.Errorf("store: stopped waiting for the writer lock: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	l := &writerLock{conn: conn, stop: make(chan struct{}), done: make(chan struct{}), onLost: onLost}
	go l.heartbeat()
	return l, nil
}

// heartbeat pings the lock connection until release. A failed ping means the
// session, and with it the lock, is gone: report it once and stop.
func (l *writerLock) heartbeat() {
	defer close(l.done)
	t := time.NewTicker(lockHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		l.mu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := l.conn.Ping(ctx)
		cancel()
		l.mu.Unlock()
		if err != nil {
			log.Printf("store: LOST the writer lock connection: %v", err)
			if l.onLost != nil {
				l.onLost(err)
			}
			return
		}
	}
}

// release unlocks and closes the lock connection. Closing alone would
// release the lock too; the explicit unlock makes it immediate even if the
// close is slow.
func (l *writerLock) release() error {
	if l == nil {
		return nil
	}
	close(l.stop)
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, writerLockKey)
	if cerr := l.conn.Close(ctx); err == nil {
		err = cerr
	}
	return err
}
