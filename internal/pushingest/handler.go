package pushingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dpup/prefab/logging"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
)

// streamHandler consumes one validated request body for one stream. Adding a
// stream is adding an entry to the map in dispatch — routing, auth, rate
// limiting, size limits and health are already done by the time it is called.
type streamHandler func(rep *reporter, body []byte, maxItems int) (accepted int, warnings []string, err error)

func (r *Registry) dispatch(stream string) (streamHandler, bool) {
	switch stream {
	case MeshStream:
		return r.ingestMesh, true
	default:
		return nil, false
	}
}

// KnownStreams lists every stream the handler accepts, so a reporter can be
// authorized for one without guessing at the spelling. `make ingest-token`
// validates against this, which is why it is exported: a reporter granted a
// stream that does not dispatch would authenticate cleanly and then collect
// 404s, and the config is the wrong place to discover that.
//
// TestKnownStreamsAllDispatch keeps this honest.
func KnownStreams() []string { return []string{MeshStream} }

// ingestResponse is the 202 body. camelCase, like the rest of /api/v1.
type ingestResponse struct {
	ReporterID string   `json:"reporterId"`
	Stream     string   `json:"stream"`
	Accepted   int      `json:"accepted"`
	Warnings   []string `json:"warnings,omitempty"`
	ReceivedAt string   `json:"receivedAt"`
}

// ServeStream handles POST /api/v1/ingest/{stream}. The stream comes from the
// router; everything else is derived from the request.
//
// Ordering is deliberate: method, then auth, then authorization, then rate
// limit, then size, then parse. Nothing about the request body is examined
// before the caller is known, so an unauthenticated client cannot make the
// server do work.
func (r *Registry) ServeStream(w http.ResponseWriter, req *http.Request, stream string) {
	ctx := logging.EnsureLogger(req.Context())

	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeStatus(w, http.StatusMethodNotAllowed, codes.InvalidArgument, "method not allowed")
		return
	}

	rep, ok := r.authenticate(bearerToken(req))
	if !ok {
		// No detail, ever: not whether the reporter exists, not which part was
		// wrong. The client already knows what it sent.
		w.Header().Set("WWW-Authenticate", `Bearer realm="grid-ingest"`)
		writeStatus(w, http.StatusUnauthorized, codes.Unauthenticated, "unauthorized")
		return
	}

	handler, known := r.dispatch(stream)
	if !known {
		writeStatus(w, http.StatusNotFound, codes.NotFound, fmt.Sprintf("unknown stream %q", stream))
		return
	}
	if !rep.streams[stream] {
		// 403, not 404: the credential is good, the grant is not. Saying so lets
		// an operator debug a config mistake without guessing.
		writeStatus(w, http.StatusForbidden, codes.PermissionDenied,
			fmt.Sprintf("reporter is not authorized for stream %q", stream))
		return
	}

	now := r.now()
	r.mu.Lock()
	rep.lastAttemptAt = now
	// Rate limit against the last ACCEPTED report, not the last attempt: limiting
	// on attempts would let a client that is being rejected lock itself out
	// indefinitely, since every retry would push the window forward.
	minInterval := rep.cfg.MinIntervalOrDefault()
	tooSoon := !rep.lastAcceptedAt.IsZero() && now.Sub(rep.lastAcceptedAt) < minInterval
	retryAfter := time.Duration(0)
	if tooSoon {
		retryAfter = minInterval - now.Sub(rep.lastAcceptedAt)
	}
	r.mu.Unlock()

	if tooSoon {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
		writeStatus(w, http.StatusTooManyRequests, codes.ResourceExhausted,
			fmt.Sprintf("reporting faster than the configured minimum interval of %s", minInterval))
		return
	}

	maxBody := r.cfg.MaxBodyBytesOrDefault()
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			r.recordFailure(rep, "body too large")
			writeStatus(w, http.StatusRequestEntityTooLarge, codes.InvalidArgument,
				fmt.Sprintf("body exceeds %d bytes", maxBody))
			return
		}
		r.recordFailure(rep, "read error")
		writeStatus(w, http.StatusBadRequest, codes.InvalidArgument, "could not read request body")
		return
	}

	accepted, warnings, err := handler(rep, body, r.cfg.MaxItemsOrDefault())
	if err != nil {
		r.recordFailure(rep, err.Error())
		logging.Warnw(ctx, "Push ingest: rejected report",
			"reporter", rep.cfg.ID, "stream", stream, "error", err)
		writeStatus(w, http.StatusBadRequest, codes.InvalidArgument, err.Error())
		return
	}

	logging.Infow(ctx, "Push ingest: accepted report",
		"reporter", rep.cfg.ID, "stream", stream,
		"accepted", accepted, "warnings", len(warnings), "bytes", len(body))

	// 202, not 200: the report has been accepted and validated, but it is not yet
	// in the store — the scheduler's next tick merges it. Saying 200 would claim
	// a durability this endpoint deliberately does not provide.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(ingestResponse{
		ReporterID: rep.cfg.ID,
		Stream:     stream,
		Accepted:   accepted,
		Warnings:   warnings,
		ReceivedAt: now.UTC().Format(time.RFC3339),
	})
}

// ServeHTTP lets the registry be mounted as a plain handler under a prefix,
// taking the stream from the final path segment. The gateway mount uses
// ServeStream with the router's own path parameter.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimSuffix(req.URL.Path, "/")
	stream := path[strings.LastIndex(path, "/")+1:]
	r.ServeStream(w, req, stream)
}

// recordFailure notes a rejected report on the reporter's health. It does NOT
// touch lastAcceptedAt, so a stream of malformed reports still ages the reporter
// into STALE — a monitor that is posting garbage is not a healthy monitor, and
// /api/v1/sources should say so rather than showing a green light because
// packets are arriving.
func (r *Registry) recordFailure(rep *reporter, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep.lastError = msg
}

// bearerToken extracts the credential from the Authorization header.
func bearerToken(req *http.Request) string {
	h := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// decodeJSON strictly decodes a body. DisallowUnknownFields is deliberately NOT
// set: a monitor adding a field to its own document must not start failing
// against an older server, and ignoring what we do not understand is the only
// version-skew policy that survives an operator upgrading their Pi before we
// upgrade the Grid. Trailing content IS rejected, because two JSON documents in
// one body means the client is confused about what it sent.
func decodeJSON[T any](body []byte) (T, error) {
	var out T
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("malformed JSON: %w", err)
	}
	if dec.More() {
		return out, errors.New("malformed JSON: trailing content after the document")
	}
	return out, nil
}

// writeStatus emits a google.rpc.Status body, matching the error convention the
// rest of /api/v1 uses (internal/gridapi/errors.go) so a client needs one error
// parser for the whole surface.
func writeStatus(w http.ResponseWriter, httpCode int, code codes.Code, msg string) {
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(
		&spb.Status{Code: int32(code), Message: msg})
	if err != nil {
		body = []byte(`{"code":13,"message":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(httpCode)
	_, _ = w.Write(body)
}
