---
name: verify
description: Build, launch, and drive the ERSN info server locally to verify changes end-to-end.
---

# Verifying the ERSN Info Server

## Toolchain
Go and protoc are NOT preinstalled in the sandbox. Install Go (arm64):
```bash
curl -fsSL -o /tmp/go.tgz https://go.dev/dl/go1.26.4.linux-arm64.tar.gz
mkdir -p ~/sdk && tar -C ~/sdk -xzf /tmp/go.tgz
export PATH="$HOME/sdk/go/bin:$PATH"
```

## Build & launch
`make server`/`make run-bg` depend on the `proto` target, which needs protoc +
plugins. If the diff has no proto changes, build directly instead:
```bash
go build -o bin/server ./cmd/server
set -a && source .envrc && set +a    # real API keys (git-ignored)
nohup ./bin/server > server.log 2>&1 & echo $! > server.pid
sleep 3   # startup triggers roads warmup (Google Routes calls) immediately
```
Server listens on **http://localhost:8181**. OpenAI key is required or the
server exits at startup.

## Drive
```bash
curl -s localhost:8181/api/v1/weather | jq .
curl -s localhost:8181/api/v1/weather/alerts
curl -s localhost:8181/api/v1/hazards/calaveras/weather_alert.geojson
curl -s localhost:8181/api/v1/situation/calaveras
curl -s localhost:8181/api/v1/roads
```
Logs are structured JSON in `server.log` — grep for upstream fetches, e.g.
`grep "Fetched NWS zone alerts" server.log`, `grep -c "Processing weather
location" server.log` (should be one per location per cache refresh; repeat
requests within the TTL must not add lines).

## Proto changes (`make proto`)
Pinned toolchain that reproduces the checked-in generated files byte-for-byte
(verified by regenerating unchanged protos → no diff):
```bash
# protoc 29.3 (arm64) — unzip via python (no unzip binary in sandbox)
curl -fsSL -o /tmp/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v29.3/protoc-29.3-linux-aarch_64.zip
python3 -c "import zipfile; zipfile.ZipFile('/tmp/protoc.zip').extractall('$HOME/sdk/protoc')" && chmod +x ~/sdk/protoc/bin/protoc
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.33.0
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@v2.19.1   # NOT go.mod's v2.27.2 — newer versions rewrite the .gw.go files
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@v2.27.2
export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$HOME/sdk/protoc/bin:$PATH"
make proto   # then: git status api/ — only files for protos you edited should change
```

## Gotchas
- Weather refresh is lazy (request-driven); the first `/weather` hit triggers
  the OpenWeather fan-out. Mind API budgets: don't loop requests that bust
  caches, and never wire `/data/3.0/onecall` back into the server (1,000/day cap).
- `pkill`/`ps` are unavailable; kill by scanning `/proc/*/cmdline`, or keep the
  pid from launch.
- Clean up: kill the server, `rm -f server.log server.pid` (server.log is not
  git-ignored).
