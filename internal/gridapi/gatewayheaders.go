package gridapi

import (
	"net/textproto"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

// httpHeaderMetadataPrefix marks a gRPC response metadata key as an HTTP
// response header: prefab's serverutil.SendHeader (and so etag.Guard and
// CacheControlInterceptor) writes the header NAME as
// "grpc-metadata-<name>". gRPC metadata keys are always lower case.
const httpHeaderMetadataPrefix = "grpc-metadata-"

// OutgoingHeaderMatcher decides which gRPC response header metadata the
// gateway copies onto the HTTP response, and under what name.
//
// Why it exists: prefab v0.6.1 builds the gateway mux with
// runtime.WithOutgoingHeaderMatcher(runtime.DefaultHeaderMatcher). Since
// grpc-gateway v2.33.0, DefaultHeaderMatcher no longer maps "Grpc-Metadata-*"
// at all (that branch moved to LegacyHeaderMatcher as an INCOMING-header
// hardening); it accepts only IANA permanent headers and renames them
// "grpcgateway-<Name>". So the "grpc-metadata-etag" and
// "grpc-metadata-cache-control" keys prefab sets were dropped, and no client
// ever saw an ETag or Cache-Control from a GridService RPC.
//
// This matcher forwards exactly the keys SendHeader marks, stripping the
// prefix ("grpc-metadata-etag" -> "Etag"), and nothing else: not gRPC's own
// "content-type", not prefab's "x-http-code" (statusCodeForwarder turns it
// into the status) or "prefab-not-modified" (the native-gRPC 304 flag).
// Unknown metadata is never exposed, and no "Grpc-Metadata-*" duplicate is
// written.
func OutgoingHeaderMatcher(key string) (string, bool) {
	if len(key) <= len(httpHeaderMetadataPrefix) ||
		!strings.EqualFold(key[:len(httpHeaderMetadataPrefix)], httpHeaderMetadataPrefix) {
		return "", false
	}
	return textproto.CanonicalMIMEHeaderKey(key[len(httpHeaderMetadataPrefix):]), true
}

// ConfigureGatewayMux applies the Grid's options to the gateway mux prefab
// built (prefab offers no way to pass ServeMuxOptions at construction; an
// option is a plain setter on the mux, so it can be applied in the
// prefab.WithGRPCGateway callback, before any request is served).
func ConfigureGatewayMux(mux *runtime.ServeMux) {
	runtime.WithOutgoingHeaderMatcher(OutgoingHeaderMatcher)(mux)
}
