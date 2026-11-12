# Multi-stage build for The Grid (S.I.E.R.R.A data service)

###############################################################################
# Stage 1: Build the Go application
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

# Cross-compile a static binary for the target platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /sierra-server ./cmd/server

# gridctl, the one-off SQLite -> PostgreSQL importer (usage: gridctl --help). Shipped
# in the Grid image so a one-off ECS task can run it (entryPoint
# /app/gridctl); its own binary, so the server links neither SQLite nor the
# AWS SDK. GRIDCTL_VERSION is recorded in snapshot manifests and import_log.
ARG GRIDCTL_VERSION=image
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.version=${GRIDCTL_VERSION}" -o /gridctl ./cmd/gridctl

###############################################################################
# Mesh relay image (docker build --target relay) — a SEPARATE service
###############################################################################
# The relay is its own process with its own database so a relay crash or deploy
# never touches the hazard feeds (the relay is github.com/sierragridteam/sierra-meshrelay/relay). These stages sit BEFORE the
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
# Stage 2: Final lightweight runtime image (docker build --target grid; also
# the default target, being the last stage)
###############################################################################
FROM alpine:3.24.4 AS grid

WORKDIR /app

# Add basic runtime dependencies and security updates
RUN apk add --no-cache ca-certificates tzdata && \
    apk upgrade

# Create a non-root user to run the application
RUN addgroup -S sierra && adduser -S sierra -G sierra

# Copy the binary from the build stage
COPY --from=go-builder /sierra-server /app/sierra-server
COPY --from=go-builder /gridctl /app/gridctl

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
# which takes seconds (every process applies pending migrations at boot,
# before it listens). It turns 503 while the process drains on SIGTERM.
HEALTHCHECK --interval=15s --timeout=3s --start-period=15s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/readyz || exit 1

# Run the application
ENTRYPOINT ["/app/sierra-server"]