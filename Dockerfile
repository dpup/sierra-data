# Multi-stage build for The Grid (S.I.E.R.R.A data service)

###############################################################################
# Stage 1: Build the Go application
###############################################################################
# Run the builder on the NATIVE build platform and cross-compile to the target
# arch. Running an amd64 Go toolchain under QEMU emulation (e.g. building
# --platform=linux/amd64 on an arm64 host) makes the Go runtime SIGSEGV in
# netpoll, so we never emulate the toolchain - we cross-compile instead.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS go-builder

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

###############################################################################
# Stage 2: Final lightweight runtime image
###############################################################################
FROM alpine:3.19

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

# Persistent grid event store (SQLite). /data is a volume so events, revisions,
# and source health survive container replacement; prod mounts a persistent
# filesystem (EFS) here. The store defaults to the TRUNCATE journal mode, which
# works over NFS/EFS (WAL's -shm is memory-mapped and does not) — override with
# PF__GRID__JOURNAL_MODE if the mount is a real local disk. Owned by sierra so the
# db + rollback journal can be created (the runtime mount must also be writable
# by this user — see docs when provisioning the volume).
RUN mkdir -p /data && chown sierra:sierra /data
VOLUME ["/data"]
# The env var name MUST be PF__GRID__DB_PATH, not PF__GRID__DBPATH. prefab's
# transformEnv maps PF__GRID__DB_PATH -> grid.dbPath (the "_" in DB_PATH is what
# reconstructs the camelCase "dbPath" config key). Without it the var maps to
# grid.dbpath, which never overrides grid.dbPath, so the store silently falls
# back to prefab.yaml's relative "./data/grid.db" (= /app/data/grid.db on the
# ephemeral layer) instead of this EFS mount — and all events/history are lost
# on every task replacement. Same rule for PF__GRID__JOURNAL_MODE (-> journalMode).
ENV PF__GRID__DB_PATH=/data/grid.db

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

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/ || exit 1

# Run the application
ENTRYPOINT ["/app/sierra-server"]