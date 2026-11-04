package handoff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGateAdmitsUntilClosed(t *testing.T) {
	g := NewGate()
	h := g.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusTeapot, rec.Code)

	g.Close()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))

	_, err := g.UnaryInterceptor()(context.Background(), nil, nil, func(context.Context, any) (any, error) {
		t.Fatal("a closed gate must not call the handler")
		return nil, nil
	})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestDrainWaitsForInFlightRequests(t *testing.T) {
	g := NewGate()
	release := make(chan struct{})
	entered := make(chan struct{})
	h := g.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered
	require.Equal(t, 1, g.InFlight())
	go func() { time.Sleep(30 * time.Millisecond); close(release) }()
	res := g.Drain(5*time.Second, time.Second)
	assert.Equal(t, 1, res.InFlight)
	assert.Zero(t, res.Cancelled, "it finished inside the grace")
	assert.Zero(t, res.Remaining)
	assert.Zero(t, g.InFlight())
}

func TestDrainCancelsRequestsThatOutlastTheGrace(t *testing.T) {
	g := NewGate()
	entered := make(chan struct{})
	sawCancel := make(chan struct{})
	h := g.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(sawCancel)
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered
	res := g.Drain(20*time.Millisecond, time.Second)
	assert.Equal(t, 1, res.Cancelled)
	assert.Zero(t, res.Remaining)
	select {
	case <-sawCancel:
	default:
		t.Fatal("the handler's context was not cancelled")
	}
}

func TestDrainReportsRequestsThatIgnoreCancellation(t *testing.T) {
	g := NewGate()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := g.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered
	res := g.Drain(10*time.Millisecond, 10*time.Millisecond)
	assert.Equal(t, 1, res.Remaining)
}

func TestDrainOfAnIdleGateIsImmediate(t *testing.T) {
	g := NewGate()
	res := g.Drain(time.Hour, time.Hour)
	assert.Zero(t, res.InFlight)
	assert.Less(t, res.Duration, time.Second)
	assert.True(t, g.Closed())
}
