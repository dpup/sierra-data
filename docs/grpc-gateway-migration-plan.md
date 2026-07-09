# Plan: return the core data API to gRPC + gRPC-Gateway

Status: proposed (2026-07-09). Supersedes the hand-built `/v1` surface described
in `docs/v2-api-spec.md` §7. GeoJSON map layers are explicitly out of scope and
stay hand-built.

## 1. Goal & motivation

Move the core (JSON) data API from hand-written `net/http` handlers back to a
**protobuf-defined gRPC service exposed over gRPC-Gateway**, so that:

- the wire contract is generated from proto (`google.api.http` annotations), not
  hand-maintained;
- OpenAPI/Swagger is regenerated automatically (`protoc-gen-openapiv2`) — the
  thing we lost when `/api/v1` was deleted;
- typed clients and gRPC reflection come for free;
- the project reads as a **standard Prefab project** (Prefab is
  `github.com/dpup/prefab`, which we own) — no bespoke gateway plumbing.

The `.geojson` map endpoints remain dedicated HTTP handlers: proto3 models RFC
7946 geometry poorly, and the spec deliberately kept them off proto.

## 2. Decisions (locked with the owner, 2026-07-09)

This is a **breaking** change; the project is days old and has one owner-operated
consumer set, so breakage is acceptable.

1. **camelCase JSON.** Accept Prefab's default marshaler (`UseProtoNames:false`).
   Consumers are rewritten from snake_case (scripted).
2. **Errors = gRPC codes + standard details.** Use Prefab's gateway error
   handler, which already emits `{code, codeName, message, details[]}` (grpc
   `google.rpc.Status` fields + a convenience `codeName`). Handlers return
   `status.Error(codes.X, …)` / `status.WithDetails(…)`.
3. **Path = `/api/v1/…`** (versioned). Prefab mounts the gateway at `/api/`;
   annotations carry the full `/api/v1/...` path. Versioned on purpose — and an
   unversioned `/api/…` would collide with Prefab's own `/api/` gateway mount.
   The former `/v1/...` surface is removed.
4. **ETag + conditional GET.** Worth adding, done properly (§4) — the one place
   we extend Prefab.
5. **GeoJSON stays HTTP.** `/api/v1/places/{place}/map/{layer}.geojson` remain
   `WithHTTPHandler` handlers.
6. **Drop as nice-to-haves:** `application/proto` responses (JSON only), and
   compact-vs-pretty JSON (accept Prefab's indented default for now).

## 3. Gap analysis — what Prefab already does vs. the one gap

| Requirement | Prefab native? | Action |
|---|---|---|
| camelCase JSON | ✅ default `JSONMarshalOptions` | use as-is |
| `/api/v1/…` routing | ✅ gateway mounted at `/api/` | annotations carry full path |
| grpc codes + standard `details` | ✅ `gatewayErrorHandler` → `CustomErrorResponse` | use as-is |
| `Cache-Control` header | ✅ unary interceptor → response metadata → header | reuse the old pattern |
| gRPC reflection, CORS/security | ✅ | `WithGRPCReflection`, automatic |
| GeoJSON dedicated handlers | ✅ `WithHTTPHandler` | keep |
| **ETag + `If-None-Match`→304** | ❌ | **needs a Prefab change (§4)** |
| application/proto | ❌ | dropped |

**The single gap:** ETag requires hashing the marshaled response body and
short-circuiting to 304 — i.e. HTTP middleware wrapping the gateway. Prefab
hardcodes its `runtime.NewServeMux(...)` option set (`builder.go`) and exposes no
seam to add `runtime.WithMiddlewares`, extra marshalers, or forward-response
options. A `Cache-Control` header we can already set via an interceptor; ETag we
cannot.

## 4. The Prefab change: `WithMiddleware` + first-class `WithETag`

Two layered additions (one focused PR) — a general mechanism plus the policy
built on it:

```go
// WithMiddleware wraps every mounted handler (the gateway AND WithHTTPHandler
// mounts) with the given net/http middleware, inside Prefab's security wrapper
// so 304s and errors still get CORS/security headers. Applied in order.
func WithMiddleware(mw ...func(http.Handler) http.Handler) ServerOption

// WithETag enables conditional GET (ETag + If-None-Match -> 304) on read
// responses. It is a Prefab-provided middleware registered via WithMiddleware.
func WithETag(opts ...ETagOption) ServerOption
```

**`WithETag` defaults (from SIERRA's needs):**
- `GET`/`HEAD` only; only `2xx` responses.
- Weak validator: `ETag: W/"<base64(sha256(body))>"`.
- Buffers the body up to a cap (~4 MiB); above the cap it streams through with no
  ETag (protects memory; irrelevant for these small payloads).
- Never overrides an `ETag` a handler already set.
- On `If-None-Match` match → `304` with empty body, other headers preserved.
- Independent of `Cache-Control` (which stays app-side, §9).
- Skips SSE/streaming and `Content-Range` responses.

**Why not `WithServeMuxOption(runtime.ServeMuxOption)`:** it would (a) wrap only
the gateway, not the `.geojson` `WithHTTPHandler` mounts — so caching wouldn't be
uniform — and (b) leak grpc-gateway's `runtime` types into Prefab's public API.
`WithMiddleware` is stdlib-typed and server-wide; `WithETag` builds on it so
consumers get correct conditional GET with one line and zero maintenance
(conditional requests are a framework concern — cf. Rails/Express).

**Dev flow:** clone `dpup/prefab`, branch, add `WithMiddleware` + `WithETag` +
tests, push, tag `vX.Y.Z`; `replace` in `sierra-data/go.mod` while iterating,
then bump and drop the `replace`.

**Deferred for now (this repo):** we assume `WithETag` lands later. SIERRA wires a
commented `// prefab.WithETag()` with a `TODO` and ships `Cache-Control`-only
(native interceptor) until the Prefab release is available.

## 5. Target architecture (as built)

Standard Prefab wiring in `cmd/server/main.go` — one `GridService`, one gateway
callback:

```
prefab.New(
  prefab.WithContext(ctx),
  prefab.WithGRPCReflection(),
  // prefab.WithETag(),                                         // TODO: enable once prefab WithETag lands (§4)
  prefab.WithGRPCService(&gridv1.GridService_ServiceDesc, gridServer),
  prefab.WithGRPCGateway(func(ctx, mux, endpoint, opts) error {
    gridv1.RegisterGridServiceHandlerFromEndpoint(ctx, mux, endpoint, opts) // the proto RPCs
    return gridServer.RegisterGatewayRoutes(mux)                            // summary + .geojson, hand-built on the same mux
  }),
  prefab.WithHTTPHandlerFunc("/mcp", mcpHandler.ServeHTTP),  // MCP calls the gateway mux in-process
  prefab.WithHTTPHandlerFunc("/", siteHandler),
)
```

The gateway dials the in-process gRPC server; the RPC impl (`gridapi.GridServer`)
wraps the existing `gridapi.Service`, which reads the store. The two endpoints
that stay hand-built — the place `summary` (evac `active_evacuations` must
serialize as an explicit JSON `null`) and the `.geojson` map layers (RFC 7946
geometry) — are mounted on the **same gateway mux** via `mux.HandlePath`
(`RegisterGatewayRoutes`), so there is no second HTTP handler competing for
`/api/v1/` and no routing-precedence problem.

## 6. Proto contract (as built)

- **Messages:** reuse `api/grid/v1` (`Event`, `Place`, `EventRevision`, `Source`,
  …) unchanged.
- **One service:** `GridService`, added to **`api/grid/v1/grid.proto`** (gateway +
  openapi generation enabled there via `make proto`) rather than a separate
  `api/v1` service — a single service is the simpler organization and keeps the
  event model and its query surface in one proto. It carries the
  event/place/source/history surface **plus** `GetConditions` and `ListScanners`,
  all with `google.api.http` annotations under `/api/v1/`.
- **No re-exposed Roads/Weather services.** The event model is the core: road
  incidents are events (`ListEvents` with a corridor `place` + `layer=road_incident`),
  so there is no dedicated `/api/v1/roads`. Current, non-event conditions (weather
  + the region's fire-weather state) are served by a single small `GetConditions`
  RPC (`/api/v1/conditions`), which reads the in-process Weather service and
  **drops per-location alerts** (alerts are events: `/api/v1/events?layer=weather_alert`).
- **Regenerate** `*.pb.go`, `*_grpc.pb.go`, `*.pb.gw.go`, and `*.swagger.json`
  via `make proto` (protoc + the pinned plugins per root `CLAUDE.md`).

## 7. Endpoint mapping (removed hand-built `/v1` → new gateway `/api/v1`)

| Old `/v1` | New RPC | New HTTP |
|---|---|---|
| `GET /v1/events` | `GridService.ListEvents` | `GET /api/v1/events` |
| `GET /v1/events/{id}` | `GetEvent` | `GET /api/v1/events/{id}` |
| `GET /v1/events/{id}/history` | `GetEventHistory` | `GET /api/v1/events/{id}/history` |
| `GET /v1/history` | `ListHistory` | `GET /api/v1/history` |
| `GET /v1/places` | `ListPlaces` | `GET /api/v1/places` |
| `GET /v1/places/{place}` | `GetPlace` | `GET /api/v1/places/{place}` |
| `GET /v1/places/resolve` | `ResolvePlace` | `GET /api/v1/places:resolve` (AIP colon custom-verb — avoids the `{place}` collision) |
| `GET /v1/sources` | `ListSources` | `GET /api/v1/sources` |
| `GET /v1/scanners` | `ListScanners` | `GET /api/v1/scanners` |
| `GET /v1/roads`, `/v1/roads/{id}` | *(none — roads are events)* | `GET /api/v1/events?place={corridor}&layer=road_incident` |
| `GET /v1/weather` | `GetConditions` (weather + fire-weather, no alerts) | `GET /api/v1/conditions` |
| `GET /v1/places/{place}/summary` | *(hand-built on the gateway mux)* | `GET /api/v1/places/{place}/summary` (snake_case) |
| `GET /v1/places/{place}/map/{layer}.geojson` | *(hand-built on the gateway mux)* | `GET /api/v1/places/{place}/map/{layer}.geojson` (snake_case) |

Shape changes for the proto RPC rows: field names camelCase; error bodies become
`CustomErrorResponse` (`{code, codeName, message, details}`);
`page_token`/`next_page_token` stay as message fields (`nextPageToken` on the
wire). The two hand-built rows keep their snake_case bodies (the summary shape;
GeoJSON's RFC 7946 contract).

## 8. Service implementation

Port the logic out of the hand-built handlers into RPC methods:

- The store queries and projections already exist (`internal/store`,
  `internal/gridapi/project.go`, place/summary/sources logic). Wrap them in
  `GridService` methods that return `grid.v1` protos.
- Replace `writeStatus`/`google.rpc.Status` hand-encoding with
  `status.Error(codes.NotFound, …)` etc.; `ErrNotFound` → `codes.NotFound`.
- Keyset pagination: `page_token` (request) / `next_page_token` (response) fields.
- `ResolvePlace` (`?lat=&lng=` or `?address=`) → request fields; the Census
  geocode path stays in the impl.
- Delete the hand-built router/handlers in `internal/gridapi` as each RPC lands,
  keeping only the store/projection helpers and the GeoJSON map handlers.

## 9. ETag + Cache-Control

- **Cache-Control:** a unary interceptor sets `cache-control: public, max-age=60`
  on the read RPCs via response metadata (the mechanism the deleted
  `cacheHeadersInterceptor` used). Native.
- **ETag/304:** `prefab.WithETag()` (§4) applies conditional GET **server-wide**,
  covering both the gateway JSON responses and the `.geojson` `WithHTTPHandler`
  mounts uniformly — no per-handler ETag code, and no separate application to
  GeoJSON. Deferred until the Prefab release; wired as a commented `// prefab.WithETag()`
  + TODO in the meantime.
- **GeoJSON routing:** the gateway and the GeoJSON handler both live under
  `/api/v1/` — mount the GeoJSON handler at the more specific `/api/v1/places/`
  (or a `.geojson`-suffix check) so the two don't fight in `http.ServeMux`;
  confirm precedence during Phase 3.

## 10. MCP rewiring

`internal/mcp` currently calls the hand-built `/v1` handler in-process through a
`captureWriter`. After the cutover, point the MCP tools at the **`GridService`
impl methods directly** (in-process Go calls → protojson) instead of faking HTTP.
Cleaner and faster; removes `callV1`/`captureWriter`. The MCP JSON shapes shift
to camelCase — update the MCP guide page examples accordingly.

## 11. Swagger / docs

- `make proto` regenerates `api/v1/*.swagger.json`. Serve them (restore the
  `openAPIHandler` + `/api/docs/*.swagger.json` mounts) and link from `/docs`.
- Rewrite `web/src/partials/docs-body.html` to document `/api/v1` camelCase, and
  re-add the "machine-readable spec" links.

## 12. Consumers

- **Site (`web/`):** rewrite fetches from `/v1` snake_case to `/api/v1` camelCase
  (scriptable: path prefix swap + field-name map). Rebuild `site/dist`.
- **External (ersn.net, sierragridteam.org):** owner-controlled; update after cutover.
- **CHANGELOG:** one BREAKING entry — surface moved `/v1` → `/api/v1`, snake_case
  → camelCase, error body shape, per the mapping in §7.

## 13. Phased execution & checklist

**Phase 0 — Prefab enablement (DEFERRED)**
- [ ] Clone `dpup/prefab`; add `WithMiddleware` + `WithETag` + tests; push/tag.
- [ ] Bump `sierra-data/go.mod`; replace the commented `// prefab.WithETag()`.
- Until then: SIERRA ships `Cache-Control`-only and leaves the ETag TODO.

**Phase 1 — Spike (prove the whole path on one endpoint)**
- [ ] `GridService` proto with just `ListSources` + `google.api.http`; `make proto`.
- [ ] Impl `ListSources` from the store; register via `WithGRPCService` + `WithGRPCGateway`.
- [ ] `cacheControlInterceptor` (native); leave `// prefab.WithETag()` + TODO.
- [ ] Verify: `GET /api/v1/sources` → camelCase JSON, `Cache-Control`;
      `sources.swagger.json` regenerates. Boot smoke test.

**Phase 2 — Full proto contract**
- [ ] Add the remaining `GridService` RPCs (§7), including `GetConditions` (weather + fire-weather, no alerts) and `ListScanners` — no re-exposed Roads/Weather.
- [ ] `make proto`; commit generated code + swagger.

**Phase 3 — Implement + cut over**
- [ ] Port each RPC from the hand-built handler; parity test old vs new (§14).
- [x] GeoJSON routing precedence resolved: `summary` + `.geojson` mount on the
      gateway mux via `mux.HandlePath` (no second `/api/v1/` handler). Weather-alerts
      decision: `GetConditions` drops per-location alerts (alerts are events).
- [ ] Delete `internal/gridapi` router/handlers as each RPC lands (keep store/
      projection helpers + the summary/GeoJSON handlers, which stay hand-built).

**Phase 4 — MCP + docs + consumers**
- [ ] Rewire MCP to call the impl directly; update MCP guide examples.
- [ ] Restore swagger mounts; rewrite `/docs`.
- [ ] Rewrite the site to `/api/v1` camelCase; rebuild `site/dist`.
- [ ] CHANGELOG breaking entry; update root/`gridapi`/`mcp` `CLAUDE.md`.

**Phase 5 — Finish Prefab**
- [ ] Land the Prefab tag; drop the `replace`; bump `go.mod`.

## 14. Testing & parity

- **Parity harness (transitional):** capture the current hand-built `/v1` JSON for
  a representative request set, then assert the new gateway response equals it
  modulo the intended shape changes (camelCase, error body). Run per endpoint
  during Phase 3; delete the harness after cutover.
- **Unit:** RPC impls tested directly (construct request protos, assert response
  protos / status codes) — no HTTP needed.
- **ETag middleware:** unit-tested standalone (200 sets ETag; matching
  `If-None-Match` → 304 empty; non-match → 200).
- **Boot smoke test:** every `/api/v1` endpoint 200s; `.geojson` still served;
  reflection works (`grpcurl`).

## 15. Risks & non-goals

- **Risk — ETag buffering:** the middleware buffers responses in memory; fine for
  these small payloads. Skip buffering for the (streaming-free) GeoJSON if large.
- **Risk — GeoJSON/gateway routing overlap** under `/api/v1/` — pinned in Phase 3
  (§9).
- **Risk — camelCase consumer churn** — scripted; the site is the only in-repo
  consumer.
- **Non-goals:** `application/proto` responses; compact JSON; moving GeoJSON onto
  proto; any change to the store, ingest, or the hazard/severity model.
- **Prefab exposure:** the migration needed **no** new Prefab option — the
  existing `WithGRPCService` / `WithGRPCGateway` / `mux.HandlePath` surface covers
  it. The one deferred addition is first-class `WithETag` (§4), left as a
  commented `// prefab.WithETag()` + TODO. If we later want compact JSON, that's a
  separate small Prefab tweak (per-server marshaler config) — not part of this plan.
