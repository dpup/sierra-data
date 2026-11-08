package gridapi

import (
	"context"
	"net/http"

	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"

	"github.com/sierragridteam/sierra-data/internal/lib/httperr"
)

// writeStatus emits the /api/v1 error envelope ({code, codeName, message,
// details}, the shape the gateway routes return) under the given HTTP status.
// grpcCode is the gRPC code numeric value; the HTTP status carries the
// transport semantics. Errors are never cacheable (Cache-Control: no-store).
func writeStatus(w http.ResponseWriter, httpCode int, grpcCode int, msg string) {
	httperr.Write(w, httpCode, codes.Code(grpcCode), msg) //nolint:gosec // small constants
}

// invalidArgument is 400 / INVALID_ARGUMENT.
func invalidArgument(w http.ResponseWriter, msg string) {
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
