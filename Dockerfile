# Multi-stage build for ERSN Info Server

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
    go build -ldflags="-s -w" -o /ersn-server ./cmd/server

###############################################################################
# Stage 2: Final lightweight runtime image
###############################################################################
FROM alpine:3.19

WORKDIR /app

# Add basic runtime dependencies and security updates
RUN apk add --no-cache ca-certificates tzdata && \
    apk upgrade

# Create a non-root user to run the application
RUN addgroup -S ersn && adduser -S ersn -G ersn

# Copy the binary from the build stage
COPY --from=go-builder /ersn-server /app/ersn-server

# Copy the configuration file
COPY --from=go-builder /app/prefab.yaml /app/prefab.yaml

# Copy the generated OpenAPI specifications
COPY --from=go-builder /app/api/v1/*.swagger.json /app/api/v1/

# Persistent grid event store (SQLite). /data is a volume so events,
# revisions, and source health survive container replacement; prod mounts
# EBS here. Owned by ersn so the WAL sidecar files can be created.
RUN mkdir -p /data && chown ersn:ersn /data
VOLUME ["/data"]
ENV PF__GRID__DBPATH=/data/grid.db

# Set ownership to the non-root user
RUN chown -R ersn:ersn /app

# Switch to the non-root user
USER ersn

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
ENTRYPOINT ["/app/ersn-server"]