# Multi-stage build for The Grid (S.I.E.R.R.A data service)

###############################################################################
# Stage 1: Build the static site (Astro: web/ -> site/dist)
###############################################################################
# This stage is why site/dist is NOT committed: the image builds the site from
# source, so a checkout can never ship a stale one and no `check-site` guard is
# needed. Runs on the NATIVE build platform - the output is static HTML/CSS/JS,
# byte-identical for every target arch, so there is nothing to cross-compile and
# no toolchain to emulate.
FROM --platform=$BUILDPLATFORM node:26.13.0-slim AS site-builder

WORKDIR /web

# Dependencies first, from the lockfile, so this layer (the slow one) stays
# cached until web/pnpm-lock.yaml actually changes. Astro is a
# devDependency, so this must NOT be --omit=dev.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN npm install --global pnpm@12.18.0 && pnpm install --frozen-lockfile

COPY web/ ./
# /changelog renders the repo-root CHANGELOG.md at build time; the page imports
# it as ../../../CHANGELOG.md from /web/src/pages, which resolves to /CHANGELOG.md.
COPY CHANGELOG.md /CHANGELOG.md

# astro.config.mjs sets outDir '../site/dist', so WORKDIR /web puts the build at
# /site/dist. The pruned root-level *.mjs are Astro build-internal stubs
# (content-*.mjs, a hash-named manifest) that the served site never references;
# `make site` prunes them the same way.
RUN pnpm run build && rm -f /site/dist/*.mjs && node screenshots/wiring-check.mjs

###############################################################################
# Stage 2: Build the Go application
###############################################################################
# Run the builder on the NATIVE build platform and cross-compile to the target
# arch. Running an amd64 Go toolchain under QEMU emulation (e.g. building
# --platform=linux/amd64 on an arm64 host) makes the Go runtime SIGSEGV in
# netpoll, so we never emulate the toolchain - we cross-compile instead.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS go-builder

WORKDIR /app

# Don't let `go build` silently pull a newer Go toolchain than this image
# provides; fail fast instead.
ENV GOTOOLCHAIN=local

# Provided by BuildKit from the --platform flag (default to linux/amd64).
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Cache Go modules by copying go.mod and go.sum first. Runs on the native build
# platform, so no emulation.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application code. The generated protobuf Go code
# (*.pb.go, *_grpc.pb.go, *.pb.gw.go) and OpenAPI specs are committed to the
# repo, so the image does NOT install protoc/plugins or run code generation -
# it just compiles. Regenerate locally with `make proto` after .proto changes.
COPY . .

# The built site comes from the site-builder stage, never from the build context
# (site/dist is git-ignored AND .dockerignore'd, so whatever a developer happens
# to have built locally cannot leak into the image). This is the tree that
# site/embed.go's `//go:embed all:dist` reads, so it must land before `go build`.
COPY --from=site-builder /site/dist ./site/dist

# Cross-compile a static binary for the target platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /sierra-server ./cmd/server

###############################################################################
# Mesh relay image (docker build --target relay) — a SEPARATE service
###############################################################################
# The relay is its own process with its own database so a relay crash or deploy
# never touches the hazard feeds (the relay is github.com/sierragridteam/sierra-meshrelay/relay). It does not embed the
# site, so this build skips the Node stage entirely. These stages sit BEFORE the
# server's final stage so a plain `docker build` still produces the server.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS relay-builder
WORKDIR /app
ENV GOTOOLCHAIN=local
ARG TARGETOS=linux
ARG TARGETARCH=amd64
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /mesh-relay ./cmd/mesh-relay

FROM alpine:3.24.4 AS relay
RUN apk add --no-cache ca-certificates tzdata && apk upgrade && \
    addgroup -S sierra && adduser -S sierra -G sierra && \
    mkdir -p /data && chown sierra:sierra /data
WORKDIR /app
COPY --from=relay-builder /mesh-relay /app/mesh-relay
# The relay runs on prefab like the Grid and reads the same prefab.yaml (its
# meshRelay section: port, admins, timings). No secrets live in it.
COPY --from=relay-builder /app/prefab.yaml /app/prefab.yaml
# Gateways, client token hashes and the lease audit log. Its OWN file — never
# point this at grid.db (both processes take an exclusive lock on their file).
# Note the underscore: PF__MESH_RELAY__DB_PATH -> meshRelay.dbPath.
VOLUME ["/data"]
ENV PF__MESH_RELAY__DB_PATH=/data/relay.db \
    PF__MESH_RELAY__CLIENT_IP_HEADER=X-Forwarded-For \
    PF__SERVER__HOST=0.0.0.0
USER sierra
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=3s --start-period=120s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8090/mesh/relay/healthz || exit 1
ENTRYPOINT ["/app/mesh-relay"]

###############################################################################
# Stage 3: Final lightweight runtime image
###############################################################################
FROM alpine:3.24.4

WORKDIR /app

# Add basic runtime dependencies and security updates
RUN apk add --no-cache ca-certificates tzdata && \
    apk upgrade

# Create a non-root user to run the application
RUN addgroup -S sierra && adduser -S sierra -G sierra

# Copy the binary from the build stage
COPY --from=go-builder /sierra-server /app/sierra-server

# Copy the configuration file
COPY --from=go-builder /app/prefab.yaml /app/prefab.yaml

# Copy the generated OpenAPI specifications
COPY --from=go-builder /app/api/v1/*.swagger.json /app/api/v1/

# The grid event store is PostgreSQL: the image holds no data and mounts no
# volume. The connection comes from PF__GRID__DATABASE__URL (a secret injected
# by the task definition); the server refuses to start without it.

# Set ownership to the non-root user
RUN chown -R sierra:sierra /app

# Switch to the non-root user
USER sierra

# Expose the application port
EXPOSE 8080

# Server configuration
ENV PORT=8080

# Prefab framework configuration
ENV PF__SERVER__HOST=0.0.0.0
ENV PF__SERVER__PORT=8080

# Health check: /readyz on the production port. Every task serves reads from
# boot (the writer lease is campaigned for in the background and never gates
# the listener), so readiness is the store open at a schema this build reads,
# which takes seconds. It turns 503 while the process drains on SIGTERM, and
# while the schema is older than this build (until a writer migrates it).
HEALTHCHECK --interval=15s --timeout=3s --start-period=15s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/readyz || exit 1

# Run the application
ENTRYPOINT ["/app/sierra-server"]