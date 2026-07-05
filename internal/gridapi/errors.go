package gridapi

import (
	"context"
	"net/http"

	"github.com/dpup/prefab/logging"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
)

// writeStatus emits a google.rpc.Status body as protojson with proto field
// names ({"code":<grpc code int>,"message":"..."}) under the given HTTP
// status — the /v1 error convention (plan §2.3). code is the gRPC code
// numeric value; the HTTP status carries the transport semantics. Errors are
// never cacheable, so no ETag and Cache-Control: no-store.
func writeStatus(w http.ResponseWriter, httpCode int, grpcCode int, msg string) {
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(
		&spb.Status{Code: int32(grpcCode), Message: msg})
	if err != nil {
		// Marshal of a Status literal cannot realistically fail; keep the body
		// shape either way.
		body = []byte(`{"code":13,"message":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(httpCode)
	_, _ = w.Write(body)
}

// badRequest is 400 / INVALID_ARGUMENT: the client sent an unusable parameter.
func badRequest(w http.ResponseWriter, msg string) {
	writeStatus(w, http.StatusBadRequest, int(codes.InvalidArgument), msg)
}

// notFound is 404 / NOT_FOUND.
func notFound(w http.ResponseWriter, msg string) {
	writeStatus(w, http.StatusNotFound, int(codes.NotFound), msg)
}

// notImplemented is 501 / UNIMPLEMENTED (the T12b endpoint stubs).
func notImplemented(w http.ResponseWriter, msg string) {
	writeStatus(w, http.StatusNotImplemented, int(codes.Unimplemented), msg)
}

// methodNotAllowed is 405; UNIMPLEMENTED is the closest gRPC code (there is
// no 405 entry in the canonical mapping — the HTTP status is authoritative).
// Callers set the Allow header first.
func methodNotAllowed(w http.ResponseWriter, method string) {
	writeStatus(w, http.StatusMethodNotAllowed, int(codes.Unimplemented), "method not allowed: "+method)
}

// unavailable is 503 / UNAVAILABLE: a dependency (e.g. the Census geocoder)
// failed transiently; the client may retry.
func unavailable(w http.ResponseWriter, msg string) {
	writeStatus(w, http.StatusServiceUnavailable, int(codes.Unavailable), msg)
}

// internal is 500 / INTERNAL. The real error is logged server-side only — a
// generic message goes on the wire so store/upstream details never leak.
func internal(ctx context.Context, w http.ResponseWriter, err error) {
	logError(ctx, "gridapi: internal error", err)
	writeStatus(w, http.StatusInternalServerError, int(codes.Internal), "internal error")
}

// logError logs via the prefab logger. EnsureLogger guards the no-logger
// case (bare contexts in tests; prefab middleware injects one in prod) —
// prefab's logging.Errorw nil-panics without it.
func logError(ctx context.Context, msg string, err error) {
	logging.Errorw(logging.EnsureLogger(ctx), msg, "error", err)
}
