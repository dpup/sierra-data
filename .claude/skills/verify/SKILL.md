---
name: verify
description: Build, launch, and drive The Grid (S.I.E.R.R.A data service) locally to verify changes end-to-end.
---

# Verifying The Grid (S.I.E.R.R.A data service)

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
(The old `/api/v1/weather|hazards|situation|roads` routes were removed in the
gRPC-gateway migration; these are the current ones. `jq` is not installed —
pipe to `python3 -m json.tool`.)
```bash
curl -s localhost:8181/api/v1/sources
curl -s localhost:8181/api/v1/events?layer=road_incident
curl -s localhost:8181/api/v1/conditions
curl -s localhost:8181/api/v1/places/ebbetts-pass/summary
curl -s localhost:8181/api/v1/places/ebbetts-pass/map/road_segment.geojson
```
Logs are structured JSON in `server.log` — grep for upstream fetches, e.g.
`grep "Fetched NWS zone alerts" server.log`, `grep -c "Processing weather
location" server.log` (should be one per location per cache refresh; repeat
requests within the TTL must not add lines).

## Proto changes (`make proto`)
Toolchain for regenerating. The plugin versions below were verified to reproduce
the checked-in files byte-for-byte; **`protoc` itself is not pinned by the
Makefile** — it comes from PATH, and its version is stamped into a comment at
the top of every generated file. The files currently on `main` say
**`protoc v6.33.1`** (generated on the maintainer's machine). Regenerating with
a different protoc rewrites that one comment line in all seven files, which
looks like a diff and is not one; match the version or expect it.
```bash
# protoc (arm64) — unzip via python (no unzip binary in sandbox). 29.3 is what
# this doc was written against; the tree is currently on 6.33.1.
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
- **Run local servers with `PF__GRID__MESHCORE__ENABLED=false`, and confirm
  teardown by PORT, not by the kill command's exit.** A smoke-test server that
  survived its kill loop ran for six days (2026-10-03 to 10-09) and kept
  production's mesh feed deaf the whole time — see "A deaf broker" in
  `internal/ingest/CLAUDE.md`. After killing, `curl -m2 localhost:<port>` for
  every port you used must get no answer. Leave mesh on only when the change
  under test is the mesh client, and then only on a current build.
- **Never run a server binary built from before 2026-10-09 with mesh enabled
  (`grid.meshcore.enabled: true` is the committed default).** Those builds
  connect to the production MQTT broker as `data.sierragridteam.org`, which is
  production's own client id, and the broker accepts them even with no
  credentials. Each connect knocks production's session off. Current builds
  append a random per-process suffix. When comparing against an old build, set
  `PF__GRID__MESHCORE__ENABLED=false` for it. See "A deaf broker" in
  `internal/ingest/CLAUDE.md`.
- Weather refresh is lazy (request-driven); the first `/weather` hit triggers
  the OpenWeather fan-out. Mind API budgets: don't loop requests that bust
  caches, and never wire `/data/3.0/onecall` back into the server (1,000/day cap).
- `pkill`/`ps` are unavailable; kill by scanning `/proc/*/cmdline`, or keep the
  pid from launch.
- Clean up: kill the server, `rm -f server.log server.pid` (server.log is not
  git-ignored).
