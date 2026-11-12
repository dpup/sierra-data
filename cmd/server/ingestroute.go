package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dpup/prefab/logging"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"

	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/lib/httperr"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Push ingest on a process that is not the writer (migration plan D9).
// mesh.repeater and mesh.packet buffer in the writer's memory and burn.line
// writes through the store, so a push must reach the writer; behind a load
// balancer it can land on any task. The route is mounted on every process:
// the writer serves it with its term's registry, any other process forwards
// the request (same method, path, query, bearer and body) to the address in
// writer_lease and relays the answer. With no live writer, or when the
// forward fails, it answers 503 with Retry-After, which every reporter
// already treats as "try again".
//
// A forwarded request carries forwardedHeader; a process that receives one
// and is not the writer answers 503 instead of forwarding again, so a stale
// address can never loop.

const forwardedHeader = "X-Grid-Forwarded-By"

// ingestPath is pushingest.IngestPath, restated as a local constant so the
// spec-coverage test (openapi_routes_test.go) can read it.
const ingestPath = "/api/v1/ingest/{stream}"

// ingestRetryAfter is the Retry-After (seconds) of a 503 from a reader: about
// how long a takeover takes.
const ingestRetryAfter = 2

type ingestRouter struct {
	store  *store.Store
	self   string // instance id, sent in forwardedHeader
	holder string // this process's writer_lease.holder

	maxBody int64
	client  *http.Client
	local   atomic.Pointer[pushingest.Registry]
}

func newIngestRouter(st *store.Store, instanceID, holder string, cfg *config.Config) *ingestRouter {
	return &ingestRouter{
		store:   st,
		self:    instanceID,
		holder:  holder,
		maxBody: cfg.Grid.Ingest.MaxBodyBytesOrDefault(),
		client:  &http.Client{Timeout: cfg.Grid.Writer.Forward()},
	}
}

// setLocal installs the writer term's registry (nil when the term ends).
func (r *ingestRouter) setLocal(reg *pushingest.Registry) { r.local.Store(reg) }

// Mount registers the route on the gateway mux for every method, as
// pushingest.Registry.Mount does (the registry answers 405 for non-POST).
func (r *ingestRouter) Mount(mux *runtime.ServeMux, wrap func(runtime.HandlerFunc) runtime.HandlerFunc) error {
	h := runtime.HandlerFunc(func(w http.ResponseWriter, req *http.Request, pp map[string]string) {
		r.serve(w, req, pp["stream"])
	})
	if wrap != nil {
		h = wrap(h)
	}
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodHead,
		http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if err := mux.HandlePath(m, ingestPath, h); err != nil {
			return fmt.Errorf("mounting %s %s: %w", m, ingestPath, err)
		}
	}
	return nil
}

func (r *ingestRouter) serve(w http.ResponseWriter, req *http.Request, stream string) {
	if reg := r.local.Load(); reg != nil {
		reg.ServeStream(w, req, stream)
		return
	}
	ctx := logging.EnsureLogger(req.Context())
	if by := req.Header.Get(forwardedHeader); by != "" {
		// Forwarded to us as the writer, and we are not (any more).
		unavailable(w, "this process is not the writer (forwarded from another task); retry")
		return
	}
	wl, err := r.store.WriterLease(ctx)
	if err != nil {
		logging.Warnw(ctx, "Push ingest: reading the writer lease", "error", err)
		unavailable(w, "the writer cannot be located; retry")
		return
	}
	if !wl.Live {
		unavailable(w, "no writer is running (a takeover is in progress); retry")
		return
	}
	if wl.Holder == r.holder {
		// We hold the lease but our term has not finished starting (or is
		// stopping): never forward to ourselves.
		unavailable(w, "the writer is starting; retry")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httperr.Write(w, http.StatusRequestEntityTooLarge, codes.InvalidArgument,
				fmt.Sprintf("body exceeds %d bytes", r.maxBody))
			return
		}
		httperr.Write(w, http.StatusBadRequest, codes.InvalidArgument, "could not read request body")
		return
	}
	start := time.Now()
	status, err := r.forward(ctx, w, req, wl.Address, body)
	if err != nil {
		logging.Warnw(ctx, "Push ingest: forwarding to the writer failed",
			"stream", stream, "writer", wl.Holder, "address", wl.Address, "epoch", wl.Epoch,
			"error", err, "duration", time.Since(start))
		unavailable(w, "the writer did not answer; retry")
		return
	}
	logging.Infow(ctx, "Push ingest: forwarded to the writer", "stream", stream,
		"writer", wl.Holder, "epoch", wl.Epoch, "status", status, "duration", time.Since(start))
}

// relayedHeaders are the response headers a reporter may act on.
var relayedHeaders = []string{"Content-Type", "Cache-Control", "Retry-After", "WWW-Authenticate", "Allow"}

// forward sends the request to the writer and copies its answer into w. An
// error means nothing was written to w.
func (r *ingestRouter) forward(ctx context.Context, w http.ResponseWriter, req *http.Request,
	address string, body []byte) (int, error) {
	out, err := http.NewRequestWithContext(ctx, req.Method, "http://"+address+req.URL.RequestURI(),
		bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for _, h := range []string{"Authorization", "Content-Type", "User-Agent"} {
		if v := req.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	out.Header.Set(forwardedHeader, r.self)
	resp, err := r.client.Do(out)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	for _, h := range relayedHeaders {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return resp.StatusCode, nil
}

func unavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(ingestRetryAfter))
	w.Header().Set("Cache-Control", "no-store")
	httperr.Write(w, http.StatusServiceUnavailable, codes.Unavailable, msg)
}
