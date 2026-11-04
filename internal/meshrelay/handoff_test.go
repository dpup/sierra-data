package meshrelay

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relay half of a handoff release (cmd/mesh-relay relayApp.Quiesce):
// closing the gate refuses new work, Stop ends every lease and bridge,
// waits for their handlers, flushes the audit queue, and only then is the
// store closed. A second relay can then open the same file.
func TestHandoffReleaseClosesSessionsAndFlushesAudit(t *testing.T) {
	db := filepath.Join(t.TempDir(), "relay.db")
	h := newHarnessAt(t, Config{}, db)
	tok := h.createGateway("gw-a")
	op := h.createClient("op", RoleOperator)
	b := h.bridge("gw-a", tok, pubkeyA)
	c := h.client("gw-a", op, "")
	expectControl(t, c, "lease")
	require.EqualValues(t, 2, h.relay.LiveSessions())

	h.relay.Gate().Close()
	resp, err := http.Get(h.base + PathPrefix + "/healthz")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a closed gate admits nothing, healthz included")
	code, _ := h.api("GET", "/gateways", h.admin, nil)
	assert.Equal(t, http.StatusServiceUnavailable, code)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, h.relay.Stop(ctx))
	assert.EqualValues(t, 0, h.relay.LiveSessions(), "every websocket handler has returned")
	b.expectClose(CloseGoingAway)
	expectControl(t, c, "lease_ending")

	// After Stop, nothing can take a lease or attach behind the close sweep.
	_, err = h.relay.Acquire(LeaseRequest{Gateway: "gw-a", Client: "op", Role: RoleOperator, Holder: nopHolder{}})
	var unavailable *UnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Equal(t, ReasonShutdown, unavailable.Code)

	// The audit queue was flushed before Stop returned: the lease's end is on disk.
	recent, err := h.store.RecentLeases(10)
	require.NoError(t, err)
	require.Len(t, recent, 1)
	assert.Equal(t, ReasonShutdown, recent[0].EndReason)

	res := h.relay.Gate().Drain(time.Second, time.Second)
	assert.Zero(t, res.Remaining)
	require.NoError(t, h.store.Close())

	// Released: a new owner opens the file without waiting on the lock, and
	// the old store refuses queries rather than touching it.
	st2, err := OpenStore(context.Background(), db, 100*time.Millisecond)
	require.NoError(t, err, "the lock was released")
	defer st2.Close()
	_, err = h.store.Gateways()
	assert.Error(t, err, "a released store must not reach the database")

	// Stop is idempotent (prefab's own Shutdown runs it again on SIGTERM).
	require.NoError(t, h.relay.Stop(ctx))
	require.NoError(t, h.relay.Shutdown(ctx))
}

type nopHolder struct{}

func (nopHolder) Deliver([]byte) {}
func (nopHolder) Notify(any)     {}
func (nopHolder) End(string)     {}

// Stopped before prefab got round to Init (a cancelled activation): a late
// Init must not start the loop against what may be a closed store.
func TestStopBeforeInitPreventsTheLoop(t *testing.T) {
	st, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "relay.db"), time.Second)
	require.NoError(t, err)
	r := New(context.Background(), Config{}, st)
	require.NoError(t, r.Stop(context.Background()))
	require.NoError(t, st.Close())

	require.NoError(t, r.Init(context.Background(), nil))
	select {
	case <-r.stopped:
	default:
		t.Fatal("a halted relay's Init must not leave a loop running")
	}
	require.NoError(t, r.Shutdown(context.Background()), "and Shutdown still works")
}
