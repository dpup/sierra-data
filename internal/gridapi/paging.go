package gridapi

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Response size. The gateway reaches the GridService over a loopback gRPC
// connection, and a gRPC client refuses a message larger than its receive
// limit, 4 MiB by default (prefab dials it with no call options). A page of
// 200 perimeter-heavy events crossed it, and the gateway answered 429
// RESOURCE_EXHAUSTED: "received message larger than max (9282591 vs.
// 4194304)" in production, 2026-11-07. A client could only guess a smaller
// pageSize, and no pageSize helps once one page's worth of events is large.
//
// Two limits fix that, one per job:
//
//   - PageByteBudget bounds a PAGE. The list RPCs stop adding items before
//     the encoded response would pass it and hand back a nextPageToken, so a
//     page can hold fewer than pageSize items. It sits under the old 4 MiB
//     limit, so pages stay a size every client already handled.
//   - GatewayMaxMessageBytes bounds a single MESSAGE on the loopback
//     connection. A page always carries at least one item, so an event (or
//     revision) bigger than the budget is served alone on its page, and
//     GetEvent / GetPlace return one item however large. This limit is what
//     lets those through. The largest stored event in production is about
//     0.8 MB, so 64 MiB is headroom, not a target.
const (
	PageByteBudget         = 3 << 20
	GatewayMaxMessageBytes = 64 << 20
)

// pageTokenReserve is room left in the budget for the next_page_token field
// and the message framing. Tokens are base64 JSON keysets, well under this.
const pageTokenReserve = 1 << 10

// GatewayDialOptions are the options the gRPC-Gateway's loopback connection
// needs on top of prefab's: the raised receive limit above. Pass them in the
// prefab.WithGRPCGateway callback.
func GatewayDialOptions() []grpc.DialOption {
	return []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(GatewayMaxMessageBytes))}
}

// fitToBudget returns how many leading items fit, as the repeated field of a
// list response, within budget bytes of encoded protobuf: what the gateway's
// receive limit measures. It measures the items as they will be sent, so call
// it after every rewrite that changes their size (I/O stripping, the mesh
// overlay, the geometry format). It is at least 1 for a non-empty slice: an
// item bigger than the budget is served alone rather than never.
func fitToBudget[T proto.Message](items []T, budget int) int {
	total := pageTokenReserve
	for i, it := range items {
		// Field 1 of EventList / EventRevisionList: a tag, a length, the bytes.
		total += protowire.SizeTag(1) + protowire.SizeBytes(proto.Size(it))
		if total > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// interruptedErr is the status for a request whose context ended before the
// store answered. The client is not at fault and the server is not broken:
// the deploy handoff drains in-flight requests and, after its grace period,
// cancels them (internal/handoff Gate.Drain). Reported as INTERNAL that was
// a 500 a client had no reason to retry, though the same page token succeeds
// a moment later. A deadline is DEADLINE_EXCEEDED (504); a cancellation is
// UNAVAILABLE (503), retryable, like the gate's own refusal.
func interruptedErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request timed out before it finished; retry")
	}
	return status.Error(codes.Unavailable, "request interrupted before it finished (the service may be handing over to a new instance); retry")
}
