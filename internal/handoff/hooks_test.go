package handoff

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rolesService is a fakeService that also reports readiness and whether it
// holds its database, the optional interfaces the Grid implements.
type rolesService struct {
	fakeService
	ready, holds atomic.Bool
}

func (r *rolesService) Ready() bool        { return r.ready.Load() }
func (r *rolesService) DatabaseHeld() bool { return r.holds.Load() }

func startWith(t *testing.T, svc Service, opts Options) (*Controller, string) {
	t.Helper()
	opts.Service, opts.Addr, opts.TokenSha256 = "test", "127.0.0.1:0", tokenHash(testToken)
	c, err := New(context.Background(), svc, opts)
	require.NoError(t, err)
	require.NoError(t, c.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return c, "http://" + c.ControlAddr()
}

func code(t *testing.T, method, url string, auth bool) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// A service that reports readiness owns /readyz and /status.ready, in any
// state (the Grid serves reads in STANDBY); without the interface it is
// "ACTIVE", as before.
func TestReadinessInterfaceOwnsReadyz(t *testing.T) {
	svc := &rolesService{}
	c, base := startWith(t, svc, Options{Standby: true})
	assert.Equal(t, http.StatusServiceUnavailable, code(t, http.MethodGet, base+"/readyz", false))
	svc.ready.Store(true)
	assert.Equal(t, http.StatusOK, code(t, http.MethodGet, base+"/readyz", false), "ready in STANDBY")
	assert.True(t, c.Status().Ready)
	assert.Equal(t, StateStandby, c.State())
}

// databaseHeld narrows to the service's own report while the controller's
// conservative flag is set, and never widens it.
func TestHoldReporterNarrowsDatabaseHeld(t *testing.T) {
	svc := &rolesService{}
	c, _ := startWith(t, svc, Options{})
	require.Eventually(t, func() bool { return c.State() == StateActive }, 2*time.Second, 5*time.Millisecond)
	assert.False(t, c.Status().DatabaseHeld, "ACTIVE, but the service holds nothing (between leases)")
	svc.holds.Store(true)
	assert.True(t, c.Status().DatabaseHeld)
	assert.True(t, c.Held())
}

// Extra commands are authenticated like /activate, honour
// expectedInstanceId, and map ErrCommandRefused to 409.
func TestExtraCommands(t *testing.T) {
	var calls atomic.Int32
	var seen atomic.Value
	opts := Options{Commands: map[string]Command{
		"ok":  func(_ context.Context, st State) error { calls.Add(1); seen.Store(st); return nil },
		"no":  func(context.Context, State) error { return errors.Join(ErrCommandRefused, errors.New("not now")) },
		"bad": func(context.Context, State) error { return errors.New("broke") },
	}}
	c, base := startWith(t, &rolesService{}, opts)
	require.Eventually(t, func() bool { return c.State() == StateActive }, 2*time.Second, 5*time.Millisecond)

	assert.Equal(t, http.StatusUnauthorized, code(t, http.MethodPost, base+"/ok", false))
	assert.Zero(t, calls.Load())
	assert.Equal(t, http.StatusOK, code(t, http.MethodPost, base+"/ok", true))
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, StateActive, seen.Load())
	assert.Equal(t, http.StatusConflict, code(t, http.MethodPost, base+"/no", true))
	assert.Equal(t, http.StatusInternalServerError, code(t, http.MethodPost, base+"/bad", true))

	req, _ := http.NewRequest(http.MethodPost, base+"/ok",
		stringsReader(`{"expectedInstanceId":"someone-else"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, int32(1), calls.Load(), "a command for another incarnation never runs")
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
