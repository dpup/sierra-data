package gridapi

import (
	"context"
	"fmt"

	"github.com/dpup/prefab/serverutil"
	"google.golang.org/grpc"
)

// CacheControlInterceptor sets `Cache-Control: public, max-age=<maxAge>` on every
// successful GridService response. All GridService RPCs are read-only, so a
// blanket max-age is safe; it complements the etag plugin — the ETag gives
// conditional revalidation (304), this bounds how long a client/proxy may serve
// a cached copy before revalidating. On a 304 the etag interceptor returns a nil
// error, so the header is set there too (correct — a 304 carries cache headers).
//
// serverutil.SendHeader only reaches the HTTP response on the gRPC-Gateway path;
// it is a no-op we ignore for native gRPC callers. It is set after the handler
// so a handler that sets its own Cache-Control (none today) would not be
// overridden by an earlier metadata write.
func CacheControlInterceptor(maxAge int) grpc.UnaryServerInterceptor {
	cc := fmt.Sprintf("public, max-age=%d", maxAge)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			_ = serverutil.SendHeader(ctx, "cache-control", cc)
		}
		return resp, err
	}
}
