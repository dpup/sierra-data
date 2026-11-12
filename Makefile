# Live Data API Server - Build, Test, and Deployment Tasks
.PHONY: openapi test-mesh-relay mesh-relay run-relay relay-token docker-build-relay ecr-push ecr-push-grid ecr-push-relay ingest-token test-pge test-llm burn-line-dry-run build test proto proto-tools clean server tools run dev lint fmt docker docker-build docker-run docker-run-dev docker-push docker-clean deploy install help test-meshcore

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt

# Build directories
BUILD_DIR=bin
PROTO_DIR=api/v1
GRID_PROTO_DIR=api/grid/v1

# Pinned proto codegen toolchain — keep aligned with go.mod so regenerating is
# reproducible. protoc itself is a native binary, pinned in moat.yaml
# (protoc@36.2); the plugins below are installed by `make proto-tools` into
# GOPATH/bin, which the proto recipe prepends to PATH (so they win over any
# floating container-provided versions).
PROTOC_GEN_GO_VERSION=v1.36.12       # == google.golang.org/protobuf
PROTOC_GEN_GO_GRPC_VERSION=v1.6.2
GRPC_GATEWAY_VERSION=v2.33.0        # == github.com/grpc-ecosystem/grpc-gateway/v2 (gateway + openapiv2)
CMD_DIR=cmd

# Binary names
SERVER_BINARY=$(BUILD_DIR)/server
TEST_GOOGLE_BINARY=$(BUILD_DIR)/test-google
TEST_CALTRANS_BINARY=$(BUILD_DIR)/test-caltrans
TEST_WEATHER_BINARY=$(BUILD_DIR)/test-weather
TEST_MESHCORE_BINARY=$(BUILD_DIR)/test-meshcore
TEST_PGE_BINARY=$(BUILD_DIR)/test-pge
TEST_LLM_BINARY=$(BUILD_DIR)/test-llm
BURN_LINE_BINARY=$(BUILD_DIR)/burn-line
MESH_RELAY_BINARY=$(BUILD_DIR)/mesh-relay
TEST_MESH_RELAY_BINARY=$(BUILD_DIR)/test-mesh-relay
TEST_GEO_UTILS_BINARY=$(BUILD_DIR)/test-geo-utils
TEST_ALERT_ENHANCER_BINARY=$(BUILD_DIR)/test-alert-enhancer
TEST_ROUTE_MATCHER_BINARY=$(BUILD_DIR)/test-route-matcher

# Docker parameters
DOCKER_IMAGE_NAME=sierra-data
# The mesh relay's image (Dockerfile --target relay), its own ECR repository.
RELAY_IMAGE_NAME?=sierra-mesh-relay
DOCKER_TAG?=latest
DOCKER_REGISTRY?=

# Default target
all: build

## Build Targets

# Build everything (protobuf generation + server + CLI tools)
build: proto server tools mesh-relay

# Build main server only
server: $(SERVER_BINARY)

$(SERVER_BINARY): proto
	$(GOBUILD) -o $(SERVER_BINARY) ./$(CMD_DIR)/server

# Mesh relay: a SEPARATE service (own process, own SQLite file).
mesh-relay:
	$(GOBUILD) -o $(MESH_RELAY_BINARY) ./$(CMD_DIR)/mesh-relay

# gridctl: the SQLite -> PostgreSQL importer (snapshot, import, verify; see
# docs/import.md). Its own binary so the server never links SQLite or the AWS
# SDK. The version (git describe) lands in the snapshot manifest and import_log.
GRIDCTL_BINARY=$(BUILD_DIR)/gridctl
GRIDCTL_VERSION?=$(shell git describe --always --dirty 2>/dev/null || echo dev)
.PHONY: gridctl
gridctl:
	$(GOBUILD) -ldflags "-X main.version=$(GRIDCTL_VERSION)" -o $(GRIDCTL_BINARY) ./$(CMD_DIR)/gridctl

# Run the relay in the foreground (prefab.yaml meshRelay: port 8090, db
# ./data/relay.db). Add yourself to meshRelay.admins (see relay-token) to use
# the admin API.
run-relay: mesh-relay
	./$(MESH_RELAY_BINARY)

# End-to-end check of a RUNNING relay over the wire protocol (fake bridge + fake
# node + client, on a throwaway gateway). Needs an admin token:
#   make test-mesh-relay TOKEN="$$DPUP_RELAY" [RELAY_URL=https://data.sierragridteam.org]
# The binary also has bridge/client modes for manual testing; see its --help.
# Always rebuilt (Go's cache makes it instant): bin/ is shared with the dev
# container, so an existing file may be a binary for another OS/arch.
$(TEST_MESH_RELAY_BINARY): FORCE
	$(GOBUILD) -o $(TEST_MESH_RELAY_BINARY) ./$(CMD_DIR)/test-mesh-relay

test-mesh-relay: $(TEST_MESH_RELAY_BINARY)
	@./$(TEST_MESH_RELAY_BINARY) -url "$(or $(RELAY_URL),http://localhost:8090)" -token "$(TOKEN)"

# Mint a bootstrap admin token for the relay: make relay-token NAME=dpup
relay-token:
	@$(GOCMD) run ./$(CMD_DIR)/mesh-relay token -name="$(or $(NAME),$(USER))"

# Build CLI testing tools only
tools: $(TEST_GOOGLE_BINARY) $(TEST_CALTRANS_BINARY) $(TEST_WEATHER_BINARY) $(TEST_MESHCORE_BINARY) $(TEST_PGE_BINARY) $(TEST_LLM_BINARY) $(BURN_LINE_BINARY) $(TEST_MESH_RELAY_BINARY)

$(TEST_GOOGLE_BINARY): proto
	$(GOBUILD) -o $(TEST_GOOGLE_BINARY) ./$(CMD_DIR)/test-google

$(BURN_LINE_BINARY): proto
	$(GOBUILD) -o $(BURN_LINE_BINARY) ./$(CMD_DIR)/burn-line

$(TEST_CALTRANS_BINARY): proto
	$(GOBUILD) -o $(TEST_CALTRANS_BINARY) ./$(CMD_DIR)/test-caltrans

$(TEST_WEATHER_BINARY): proto
	$(GOBUILD) -o $(TEST_WEATHER_BINARY) ./$(CMD_DIR)/test-weather

$(TEST_MESHCORE_BINARY): proto
	$(GOBUILD) -o $(TEST_MESHCORE_BINARY) ./$(CMD_DIR)/test-meshcore

$(TEST_LLM_BINARY): proto
	$(GOBUILD) -o $(TEST_LLM_BINARY) ./$(CMD_DIR)/test-llm

$(TEST_PGE_BINARY): proto
	$(GOBUILD) -o $(TEST_PGE_BINARY) ./$(CMD_DIR)/test-pge

# Generate protobuf code
# Note: googleapis is a proto-only module (no Go code), so we download it explicitly with @latest.
# Both googleapis and grpc-gateway are resolved via `go mod download -json` (not `go list -m`)
# because that guarantees the module source is extracted on disk before we point protoc at it.
# `go list -m` returns an empty .Dir for a module that hasn't been downloaded yet, which makes
# protoc fail with "Import protoc-gen-openapiv2/options/annotations.proto was not found".
proto-tools:
	@echo "Installing pinned protoc plugins (see Makefile version vars / go.mod)..."
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	@go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@$(GRPC_GATEWAY_VERSION)
	@go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@$(GRPC_GATEWAY_VERSION)

proto: proto-tools
	@echo "Generating protobuf code..."
	@mkdir -p $(BUILD_DIR) $(PROTO_DIR)
	$(eval GOOGLEAPIS_DIR := $(shell go mod download -json github.com/googleapis/googleapis@latest | grep '"Dir"' | head -1 | sed 's/.*"Dir": "//;s/".*//'))
	$(eval GRPC_GATEWAY_DIR := $(shell go mod download -json github.com/grpc-ecosystem/grpc-gateway/v2 | grep '"Dir"' | head -1 | sed 's/.*"Dir": "//;s/".*//'))
	@PATH="$(shell go env GOPATH)/bin:$(PATH)" protoc --proto_path=$(PROTO_DIR) \
		--proto_path=$(GOOGLEAPIS_DIR) \
		--proto_path=$(GRPC_GATEWAY_DIR) \
		--go_out=$(PROTO_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_DIR) --go-grpc_opt=paths=source_relative \
		--grpc-gateway_out=$(PROTO_DIR) --grpc-gateway_opt=paths=source_relative \
		--openapiv2_out=$(PROTO_DIR) --openapiv2_opt=logtostderr=true \
		$(PROTO_DIR)/*.proto
	@PATH="$(shell go env GOPATH)/bin:$(PATH)" protoc --proto_path=$(GRID_PROTO_DIR) \
		--proto_path=$(GOOGLEAPIS_DIR) \
		--proto_path=$(GRPC_GATEWAY_DIR) \
		--go_out=$(GRID_PROTO_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(GRID_PROTO_DIR) --go-grpc_opt=paths=source_relative \
		--grpc-gateway_out=$(GRID_PROTO_DIR) --grpc-gateway_opt=paths=source_relative \
		--openapiv2_out=$(GRID_PROTO_DIR) --openapiv2_opt=logtostderr=true \
		$(GRID_PROTO_DIR)/*.proto
	@echo "Protobuf code generation completed."
	@echo "OpenAPI specifications generated in $(PROTO_DIR)/"
	@$(MAKE) --no-print-directory openapi

# Build the served OpenAPI document (/api/openapi.json): the generated
# grid.swagger.json converted to OpenAPI 3.0 with the hand-written overlay merged
# in (cmd/openapi-merge, internal/openapispec). Run after editing the overlay;
# `make proto` runs it too. Fails, writing nothing, on a merge conflict, a
# dangling $ref or an invalid result.
openapi:
	go run ./cmd/openapi-merge \
		-swagger $(GRID_PROTO_DIR)/grid.swagger.json \
		-overlay $(GRID_PROTO_DIR)/openapi.overlay.yaml \
		-out $(GRID_PROTO_DIR)/openapi.json

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f $(PROTO_DIR)/*.pb.go
	rm -f $(PROTO_DIR)/*_grpc.pb.go
	rm -f $(PROTO_DIR)/*.swagger.json
	rm -f $(GRID_PROTO_DIR)/*.pb.go

## Database (local PostgreSQL in Docker)

# The Grid store is PostgreSQL. Locally it runs in Docker from
# docker-compose.test.yml: one server holding the dev database (`sierra`) and
# the test databases (internal/storetest creates sierra_template plus one
# throwaway database per test). Without the compose plugin (the Moat sandbox
# has none) db-up runs the same container with `docker run`; keep the two in
# step. The credentials are local-only throwaways.
PG_CONTAINER = sierra-data-pg
PG_IMAGE = postgres:18.6-alpine
PG_PORT = 55432
DB_URL = postgres://sierra:sierra@127.0.0.1:$(PG_PORT)/sierra?sslmode=disable
# The admin database the test harness connects to before creating its own.
TEST_DATABASE_URL ?= postgres://sierra:sierra@127.0.0.1:$(PG_PORT)/postgres?sslmode=disable
# make run / run-bg use the compose database unless the environment says otherwise.
PF__GRID__DATABASE__URL ?= $(DB_URL)

.PHONY: db-up db-down db-url
db-up:
	@if docker compose version >/dev/null 2>&1; then \
		docker compose -f docker-compose.test.yml up -d --wait >/dev/null; \
	elif docker container inspect $(PG_CONTAINER) >/dev/null 2>&1; then \
		docker start $(PG_CONTAINER) >/dev/null; \
	else \
		docker run -d --name $(PG_CONTAINER) \
			-e POSTGRES_USER=sierra -e POSTGRES_PASSWORD=sierra -e POSTGRES_DB=sierra \
			-e POSTGRES_INITDB_ARGS="--locale=C --encoding=UTF8" \
			-p 127.0.0.1:$(PG_PORT):5432 -v sierra-data-pg:/var/lib/postgresql \
			$(PG_IMAGE) postgres -c fsync=off -c full_page_writes=off \
			-c synchronous_commit=off -c max_connections=400 >/dev/null; \
	fi
	@for i in $$(seq 1 60); do \
		docker exec $(PG_CONTAINER) pg_isready -q -h 127.0.0.1 -U sierra -d sierra && exit 0; \
		sleep 1; \
	done; echo "PostgreSQL ($(PG_CONTAINER)) did not become ready"; exit 1

db-down:
	@if docker compose version >/dev/null 2>&1; then \
		docker compose -f docker-compose.test.yml stop; \
	else \
		docker stop $(PG_CONTAINER) >/dev/null 2>&1 || true; \
	fi

db-url:
	@echo "$(DB_URL)"

## Testing Targets

# Run full test suite against the Docker PostgreSQL (db-up). Plain `go test ./...` without
# TEST_DATABASE_URL skips every store-backed test with a loud SKIP line.
# CI provides its own PostgreSQL service and sets SKIP_DB_UP=1.
test: $(if $(SKIP_DB_UP),,db-up)
	TZ=UTC TEST_DATABASE_URL="$(TEST_DATABASE_URL)" $(GOTEST) -v ./...

# Test incident content processing functionality
test-incident: $(TEST_CALTRANS_BINARY)
	./$(TEST_CALTRANS_BINARY) --test-content-hash $(if $(VERBOSE),--verbose)

# Test individual API clients (optional parameters)
test-google: $(TEST_GOOGLE_BINARY)
	./$(TEST_GOOGLE_BINARY) --config=prefab.yaml $(if $(ROUTE_ID),--route-id=$(ROUTE_ID)) $(if $(VERBOSE),--verbose)

test-caltrans: $(TEST_CALTRANS_BINARY)
	./$(TEST_CALTRANS_BINARY) $(if $(OFFLINE),-offline) $(if $(FEED),-feed=$(FEED)) $(if $(FILTER),-filter) $(if $(LAT),-lat=$(LAT)) $(if $(LON),-lon=$(LON)) $(if $(RADIUS),-radius=$(RADIUS))

test-weather: $(TEST_WEATHER_BINARY)
	./$(TEST_WEATHER_BINARY) --config=prefab.yaml $(if $(LOCATION_ID),--location-id=$(LOCATION_ID)) $(if $(VERBOSE),--verbose)

# MeshCore bridge subscribe probe. Broker/creds via env (MC_HOST/MC_USER/MC_PASS,
# etc. — see cmd/test-meshcore); nothing sensitive is passed on the command line.
test-meshcore: $(TEST_MESHCORE_BINARY)
	./$(TEST_MESHCORE_BINARY)

# PG&E outage + PSPS probe. Prints what the poller would see, including
# whether PG&E's own ETL stamp says the outage feed has FROZEN — the failure
# these undocumented endpoints show instead of an error.
test-pge: $(TEST_PGE_BINARY)
	./$(TEST_PGE_BINARY) $(if $(BOUNDS),--bounds=$(BOUNDS)) $(if $(JSON),--json)

# Probe an OpenAI-compatible provider with the exact requests the AI features
# send (strict json_schema with the production schemas, token-budget field,
# reasoning control, latency, audio). Prints a Markdown report. Spends model
# calls, so it is manual; .github/workflows/llm-probe.yml runs it from CI where
# the keys live. See docs/design/lunaroute-compatibility.md §6.
#
#   OPENAI_API_KEY=lr_... make test-llm BASE_URL=https://gw.lunaroute.com/v1
#   make test-llm MODELS=gpt-5-mini TRANSCRIBE=whisper-1 AUDIO=recording.mp3
test-llm: $(TEST_LLM_BINARY)
	./$(TEST_LLM_BINARY) $(if $(BASE_URL),-base-url="$(BASE_URL)") \
		$(if $(MODELS),-models="$(MODELS)") $(if $(TRANSCRIBE),-transcribe-model="$(TRANSCRIBE)") \
		$(if $(AUDIO),-audio="$(AUDIO)") $(if $(RUNS),-runs=$(RUNS)) $(if $(CONCURRENCY),-concurrency=$(CONCURRENCY))

# Call the configured county burn lines and print the readings WITHOUT pushing.
# This places REAL phone calls and spends Twilio + OpenAI credit, so it is
# dry-run only here; the pushing path runs from .github/workflows/burn-line.yml.
#
# Lines come from grid.burn.lines in prefab.yaml (the single source of truth), so
# there is nothing to pass; LINE=<id> narrows it to one.
#
#   make burn-line-dry-run
#   make burn-line-dry-run LINE=calaveras-apcd
burn-line-dry-run: $(BURN_LINE_BINARY)
	./$(BURN_LINE_BINARY) -dry-run $(if $(LINE),-line="$(LINE)")

# Mint a push-ingest credential for a reporter (see internal/pushingest).
#
# Prints the TOKEN — give it to the operator over a private channel; it goes on
# their machine and nowhere else — and writes only its SHA-256 HASH into
# prefab.yaml. The hash is safe to commit (it cannot be replayed or reversed),
# which is the whole reason the config stores it rather than the token.
#
#   make ingest-token                        # just print a pair, change nothing
#   make ingest-token REPORTER=alan-pi       # add it to prefab.yaml
#   make ingest-token REPORTER=alan-pi NAME="Alan's repeater monitor"
#
# Re-running with an existing id ROTATES that reporter's token in place, leaving
# every other setting on the entry alone. Review with `git diff prefab.yaml`.
#
# It is a Go program rather than a shell one-liner for two reasons: the YAML
# edit has to preserve this file's comments (it is half prose), and `openssl` +
# `shasum` are not portable — shasum is absent on many Linux images, sha256sum
# on macOS. crypto/rand and crypto/sha256 are neither.
# Values are double-quoted because NAME routinely contains spaces and an
# apostrophe ("Alan's repeater monitor"); make substitutes them into the recipe
# textually, so unquoted they reach /bin/sh as broken syntax.
ingest-token:
	@$(GOCMD) run ./$(CMD_DIR)/ingest-token \
		$(if $(REPORTER),-reporter="$(REPORTER)") \
		$(if $(NAME),-name="$(NAME)") \
		$(if $(STREAMS),-streams="$(STREAMS)") \
		$(if $(CONFIG),-config="$(CONFIG)")

# Validate configuration without API calls
test-config:
	@echo "Configuration validation not yet implemented"

# Fetch timestamped test data snapshots from live APIs
fetch-test-data: fetch-caltrans-data fetch-cwwp2-data fetch-google-data fetch-weather-data

# Fetch Caltrans KML test data
fetch-caltrans-data:
	@echo "Fetching Caltrans KML test data snapshots..."
	@mkdir -p tests/testdata/caltrans
	$(eval TIMESTAMP := $(shell date +%Y%m%d_%H%M%S))
	@echo "Timestamp: $(TIMESTAMP)"
	@curl -s "https://quickmap.dot.ca.gov/data/lcs2way.kml" > tests/testdata/caltrans/lane_closures_$(TIMESTAMP).kml
	@curl -s "https://quickmap.dot.ca.gov/data/chp-only.kml" > tests/testdata/caltrans/chp_incidents_$(TIMESTAMP).kml
	@curl -s "https://quickmap.dot.ca.gov/data/cc.kml" > tests/testdata/caltrans/chain_controls_$(TIMESTAMP).kml
	@echo "✅ Caltrans test data snapshots saved"

# Fetch Caltrans CWWP2 (District 10) snapshots. Timestamped, never overwriting
# the committed fixtures the tests depend on — see tests/testdata/cwwp2/README.md.
fetch-cwwp2-data:
	@echo "Fetching Caltrans CWWP2 District 10 snapshots..."
	@mkdir -p tests/testdata/cwwp2
	$(eval TIMESTAMP := $(shell date +%Y%m%d_%H%M%S))
	@curl -sf "https://cwwp2.dot.ca.gov/data/d10/cc/ccStatusD10.json" > tests/testdata/cwwp2/cc_d10_$(TIMESTAMP).json
	@curl -sf "https://cwwp2.dot.ca.gov/data/d10/lcs/lcsStatusD10.json" > tests/testdata/cwwp2/lcs_d10_full_$(TIMESTAMP).json
	@curl -sf "https://cwwp2.dot.ca.gov/data/d10/cms/cmsStatusD10.json" > tests/testdata/cwwp2/cms_d10_$(TIMESTAMP).json
	@curl -sf "https://cwwp2.dot.ca.gov/data/d10/cctv/cctvStatusD10.json" > tests/testdata/cwwp2/cctv_d10_full_$(TIMESTAMP).json
	@echo "✅ CWWP2 snapshots saved (cc_d10_$(TIMESTAMP).json, lcs_d10_full_$(TIMESTAMP).json, cms_d10_$(TIMESTAMP).json, cctv_d10_full_$(TIMESTAMP).json)"

# Fetch Google Routes API test data
fetch-google-data:
	@echo "Fetching Google Routes API test data..."
	@mkdir -p tests/testdata/google
	$(eval TIMESTAMP := $(shell date +%Y%m%d_%H%M%S))
	@if [ -z "$(PF__GOOGLE_ROUTES__API_KEY)" ]; then \
		echo "⚠️  PF__GOOGLE_ROUTES__API_KEY not set, skipping Google API fixtures"; \
		echo "   Set environment variable: export PF__GOOGLE_ROUTES__API_KEY=your-api-key"; \
	else \
		echo "Fetching sample route data (Seattle to Portland)..."; \
		curl -s -X POST "https://routes.googleapis.com/directions/v2:computeRoutes" \
			-H "X-Goog-Api-Key: $(PF__GOOGLE_ROUTES__API_KEY)" \
			-H "X-Goog-FieldMask: routes.duration,routes.staticDuration,routes.distanceMeters,routes.polyline.encodedPolyline,routes.travelAdvisory.speedReadingIntervals" \
			-H "Content-Type: application/json" \
			-d '{"origin":{"location":{"latLng":{"latitude":47.6062,"longitude":-122.3321}}},"destination":{"location":{"latLng":{"latitude":45.5152,"longitude":-122.6784}}},"travelMode":"DRIVE","routingPreference":"TRAFFIC_AWARE_OPTIMAL","extraComputations":["TRAFFIC_ON_POLYLINE"]}' \
			> tests/testdata/google/seattle_portland_$(TIMESTAMP).json; \
		echo "✅ Google Routes test data saved"; \
	fi

# Fetch Weather API test data  
fetch-weather-data:
	@echo "Fetching OpenWeatherMap test data..."
	@mkdir -p tests/testdata/weather
	$(eval TIMESTAMP := $(shell date +%Y%m%d_%H%M%S))
	@if [ -z "$(PF__OPENWEATHER__API_KEY)" ]; then \
		echo "⚠️  PF__OPENWEATHER__API_KEY not set, skipping Weather API fixtures"; \
		echo "   Set environment variable: export PF__OPENWEATHER__API_KEY=your-api-key"; \
	else \
		echo "Fetching current weather data (Seattle)..."; \
		curl -s "https://api.openweathermap.org/data/2.5/weather?lat=47.6062&lon=-122.3321&appid=$(PF__OPENWEATHER__API_KEY)&units=metric" \
			> tests/testdata/weather/seattle_current_$(TIMESTAMP).json; \
		echo "Fetching weather alerts data (Seattle)..."; \
		curl -s "https://api.openweathermap.org/data/3.0/onecall?lat=47.6062&lon=-122.3321&appid=$(PF__OPENWEATHER__API_KEY)&exclude=minutely,hourly,daily" \
			> tests/testdata/weather/seattle_alerts_$(TIMESTAMP).json; \
		echo "✅ Weather API test data saved"; \
	fi

# Update symlinks to use the most recent timestamped test data
use-latest-test-data:
	@echo "Updating symlinks to use latest test data..."
	@# Update Caltrans data
	@if [ -d tests/testdata/caltrans ]; then \
		cd tests/testdata/caltrans && \
		ln -sf $$(ls -1 lane_closures_*.kml | tail -1) lane_closures.kml && \
		ln -sf $$(ls -1 chp_incidents_*.kml | tail -1) chp_incidents.kml && \
		ln -sf $$(ls -1 chain_controls_*.kml | tail -1) chain_controls.kml; \
	fi
	@# Update Google Routes data
	@if [ -d tests/testdata/google ]; then \
		cd tests/testdata/google && \
		ln -sf $$(ls -1 seattle_portland_*.json | tail -1) seattle_portland.json; \
	fi
	@# Update Weather data  
	@if [ -d tests/testdata/weather ]; then \
		cd tests/testdata/weather && \
		ln -sf $$(ls -1 seattle_current_*.json | tail -1) seattle_current.json && \
		ln -sf $$(ls -1 seattle_alerts_*.json | tail -1) seattle_alerts.json; \
	fi
	@echo "✅ Symlinks updated to latest snapshots"
	@if [ -d tests/testdata/caltrans ]; then ls -la tests/testdata/caltrans/*.kml | grep -E "(lane_closures|chp_incidents|chain_controls)\.kml"; fi
	@if [ -d tests/testdata/google ]; then ls -la tests/testdata/google/*.json | grep -E "seattle_portland\.json"; fi
	@if [ -d tests/testdata/weather ]; then ls -la tests/testdata/weather/*.json | grep -E "(seattle_current|seattle_alerts)\.json"; fi

## Development Targets

# Run server with configuration, against the Docker PostgreSQL unless
# PF__GRID__DATABASE__URL is already set.
run: server db-up
	PF__GRID__DATABASE__URL="$(PF__GRID__DATABASE__URL)" ./$(SERVER_BINARY)

# Run server in background for testing
run-bg: server stop db-up
	@echo "Starting server in background..."
	@PF__GRID__DATABASE__URL="$(PF__GRID__DATABASE__URL)" nohup ./$(SERVER_BINARY) > server.log 2>&1 & echo $$! > server.pid
	@sleep 2
	@if [ -f server.pid ] && kill -0 $$(cat server.pid) 2>/dev/null; then \
		echo "Server started in background (PID: $$(cat server.pid))"; \
		echo "Server logs: tail -f server.log"; \
		echo "Use 'make stop' to stop the server"; \
	else \
		echo "Failed to start server"; \
		rm -f server.pid; \
		exit 1; \
	fi

# Stop background server
stop:
	@echo "Stopping server..."
	@STOPPED=false; \
	if [ -f server.pid ]; then \
		PID=$$(cat server.pid); \
		if kill -0 $$PID 2>/dev/null; then \
			kill $$PID && echo "Stopped server (PID: $$PID)"; \
			STOPPED=true; \
		fi; \
		rm -f server.pid; \
	fi; \
	PORT_PID=$$(lsof -ti :8181 2>/dev/null); \
	if [ -n "$$PORT_PID" ]; then \
		kill $$PORT_PID 2>/dev/null && echo "Stopped process on port 8181 (PID: $$PORT_PID)"; \
		STOPPED=true; \
	fi; \
	if [ "$$STOPPED" = "false" ]; then \
		echo "No running server found"; \
	fi

# Gate 3 of the PostgreSQL migration: two servers on one throwaway database
# (REHEARSE_DB, default sierra_rehearsal, dropped and recreated), a SIGTERM
# and a SIGKILL handover under load; prints takeover times and failed
# requests. Ports REHEARSE_PORT_A/B (8197/8198). docs/deploy-handoff.md.
.PHONY: rehearse-bluegreen
rehearse-bluegreen: server db-up
	./scripts/rehearse-bluegreen.sh

# Test server startup (quick test that exits after a few seconds)
test-server: server
	@echo "Testing server startup..."
	./$(SERVER_BINARY) & \
	SERVER_PID=$$!; \
	sleep 3; \
	kill $$SERVER_PID; \
	echo "✅ Server startup test completed successfully"

# Run server in development mode with auto-restart
dev: server
	@echo "Development mode with auto-restart not yet implemented"
	@echo "For now, use: make run CONFIG=prefab.yaml"

# Go code formatting
fmt:
	$(GOFMT) ./...

# Run Go linting tools
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found. Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		$(GOCMD) vet ./...; \
	fi

## Docker Targets

# Build Docker container image
docker: docker-build

# Build Docker container image.
# Depends on `proto` (regenerate the committed *.pb.go so the image is built
# from code matching the current .proto) and `test` (never build/deploy a red
# tree). The image itself only compiles — it does not regenerate or test.
docker-build: proto test
	@echo "Building Docker image: $(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"
	docker build --platform linux/amd64 --target grid -t $(DOCKER_IMAGE_NAME):$(DOCKER_TAG) .
	@echo "✅ Docker image built: $(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"

# The relay image: same Dockerfile, its own target (no API keys).
docker-build-relay:
	docker build --platform linux/amd64 --target relay -t $(RELAY_IMAGE_NAME):$(DOCKER_TAG) .
	@echo "✅ Docker image built: $(RELAY_IMAGE_NAME):$(DOCKER_TAG)"

# Login to ECR and push the relay image (to its own repository).
ecr-push-relay: docker-build-relay
	@if [ -z "$(DOCKER_REGISTRY)" ]; then \
		echo "⚠️  DOCKER_REGISTRY not set. Set with: make ecr-push-relay DOCKER_REGISTRY=your-ecr-registry"; \
		exit 1; \
	fi
	aws ecr get-login-password --region $$(echo $(DOCKER_REGISTRY) | cut -d'.' -f4) | docker login --username AWS --password-stdin $(DOCKER_REGISTRY)
	docker tag $(RELAY_IMAGE_NAME):$(DOCKER_TAG) $(DOCKER_REGISTRY)/$(RELAY_IMAGE_NAME):$(DOCKER_TAG)
	docker push $(DOCKER_REGISTRY)/$(RELAY_IMAGE_NAME):$(DOCKER_TAG)
	@echo "✅ Docker image pushed: $(DOCKER_REGISTRY)/$(RELAY_IMAGE_NAME):$(DOCKER_TAG)"

# Run Docker container with environment variables
docker-run: docker-build
	@echo "Running Docker container: $(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"
	docker run -it --rm -p 8080:8080 \
		-e PF__GOOGLE_ROUTES__API_KEY=$(PF__GOOGLE_ROUTES__API_KEY) \
		-e PF__OPENWEATHER__API_KEY=$(PF__OPENWEATHER__API_KEY) \
		-e PF__OPENAI__API_KEY=$(PF__OPENAI__API_KEY) \
		--name sierra-server \
		$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)

# Run Docker container in development mode with config mounted
docker-run-dev: docker-build
	@echo "Running Docker container in development mode..."
	docker run -it --rm -p 8080:8080 \
		-e PF__GOOGLE_ROUTES__API_KEY=$(PF__GOOGLE_ROUTES__API_KEY) \
		-e PF__OPENWEATHER__API_KEY=$(PF__OPENWEATHER__API_KEY) \
		-e PF__OPENAI__API_KEY=$(PF__OPENAI__API_KEY) \
		-v $(PWD)/prefab.yaml:/app/prefab.yaml:ro \
		--name sierra-server-dev \
		$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)

# Build and push both images without starting a deployment: a manual first
# push (the Grid account's ECR before the deploy role exists) or a local
# check. Production deploys go through `make deploy` and the Deploy workflow.
ecr-push: ecr-push-grid ecr-push-relay
	@echo "✅ Both images pushed to $(DOCKER_REGISTRY) as :$(DOCKER_TAG)"

# Login to ECR and push the Grid server image.
ecr-push-grid: docker-build
	@if [ -z "$(DOCKER_REGISTRY)" ]; then \
		echo "⚠️  DOCKER_REGISTRY not set. Set with: make ecr-push-grid DOCKER_REGISTRY=your-ecr-registry"; \
		exit 1; \
	fi
	@echo "Logging into ECR..."
	aws ecr get-login-password --region $$(echo $(DOCKER_REGISTRY) | cut -d'.' -f4) | docker login --username AWS --password-stdin $(DOCKER_REGISTRY)
	@echo "Pushing Docker image to $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"
	docker tag $(DOCKER_IMAGE_NAME):$(DOCKER_TAG) $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)
	docker push $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)
	@echo "✅ Docker image pushed: $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"

# Push Docker image to registry
docker-push: docker-build
	@if [ -z "$(DOCKER_REGISTRY)" ]; then \
		echo "⚠️  DOCKER_REGISTRY not set. Set with: make docker-push DOCKER_REGISTRY=your-registry.com"; \
		exit 1; \
	fi
	@echo "Pushing Docker image to $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"
	docker tag $(DOCKER_IMAGE_NAME):$(DOCKER_TAG) $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)
	docker push $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)
	@echo "✅ Docker image pushed: $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"

## Deployment (main -> the Grid account; docs/deploy-handoff.md)
#
# CI on main passes -> the Deploy workflow (.github/workflows/deploy.yml)
# builds and pushes the images, registers task-definition revisions and
# updates the ECS services (native blue/green, bake, automatic rollback).
# `make deploy` runs that workflow by hand: for HEAD (on main, pushed, clean,
# CI green) or REF=<tag or commit on main> to redeploy an earlier release.
# It needs gh, not AWS credentials.
.PHONY: deploy deploy-status test-deploy-client ecs-deploy ecs-status ecs-rollback
deploy: test-deploy-client
	python3 scripts/deploy.py dispatch$(if $(REF), --ref "$(REF)")$(if $(ALLOW_DIRTY), --allow-dirty)$(if $(PUSH_ONLY), --push-only)

deploy-status:
	gh run list --workflow deploy.yml --limit 10

test-deploy-client:
	python3 -m unittest discover -s scripts -p 'test_deploy.py' -v
	python3 -m unittest discover -s scripts -p 'test_rehearse_import.py' -v

# Import rehearsal (migration gate 5): AWS Backup restore of grid.db, snapshot
# in the old account, import + verify in the Grid account, report in the
# workspace's notes/local/. Runbook: docs/import.md. Try REHEARSE_ARGS=--dry-run first.
# make rehearse-import REHEARSE_ARGS="--old-profile <old> --new-profile <new> [--new-base-url <url>]"
.PHONY: rehearse-import
rehearse-import: test-deploy-client
	python3 scripts/rehearse_import.py $(REHEARSE_ARGS)

# The legacy stack's commands (the coordinator in the old account). They exist
# only on legacy/efs-sqlite; here they refuse, so muscle memory cannot ship the
# PostgreSQL build to the SQLite stack.
ecs-deploy ecs-status ecs-rollback:
	@echo "make $@ is the legacy stack's (SQLite on EFS, old account) and runs only from a checkout of" >&2
	@echo "legacy/efs-sqlite (repos/sierra-data). main deploys to the Grid account with make deploy." >&2
	@exit 2

# API diff gate (migration plan section 8, gate 2): the legacy SQLite server
# and the new PostgreSQL server on the same snapshot, every endpoint compared.
# make apidiff LEGACY=http://localhost:8199 NEW=http://localhost:8200 [APIDIFF_ARGS="--events -1"]
.PHONY: apidiff test-apidiff
apidiff: test-apidiff
	@test -n "$(LEGACY)" -a -n "$(NEW)" || { echo "usage: make apidiff LEGACY=<url> NEW=<url>"; exit 2; }
	python3 scripts/apidiff.py --legacy "$(LEGACY)" --new "$(NEW)" $(APIDIFF_ARGS)

test-apidiff:
	python3 -m unittest discover -s scripts -p 'test_apidiff.py'

# Clean up Docker artifacts
docker-clean:
	@echo "Cleaning up Docker artifacts..."
	-docker stop sierra-server sierra-server-dev 2>/dev/null || true
	-docker rm sierra-server sierra-server-dev 2>/dev/null || true
	-docker rmi $(DOCKER_IMAGE_NAME):$(DOCKER_TAG) 2>/dev/null || true
	-docker system prune -f
	@echo "✅ Docker artifacts cleaned"

# Install CLI tools to system PATH
install: tools
	@echo "Installing CLI tools to system PATH..."
	cp $(TEST_GOOGLE_BINARY) /usr/local/bin/
	cp $(TEST_CALTRANS_BINARY) /usr/local/bin/
	cp $(TEST_WEATHER_BINARY) /usr/local/bin/
	cp $(TEST_GEO_UTILS_BINARY) /usr/local/bin/
	cp $(TEST_ALERT_ENHANCER_BINARY) /usr/local/bin/
	cp $(TEST_ROUTE_MATCHER_BINARY) /usr/local/bin/
	@echo "CLI tools installed: test-google, test-caltrans, test-weather, test-geo-utils, test-alert-enhancer, test-route-matcher"

## Utility Targets

# Update Go dependencies
deps:
	$(GOMOD) tidy
	$(GOMOD) download

# Show help
help:
	@echo "Live Data API Server - Available targets:"
	@echo ""
	@echo "Build targets:"
	@echo "  build       - Build server and all CLI tools (default)"
	@echo "  server      - Build main server only"
	@echo "  tools       - Build CLI testing tools only"
	@echo "  proto       - Generate protobuf code (and the OpenAPI spec)"
	@echo "  openapi     - Rebuild api/grid/v1/openapi.json from the swagger + overlay"
	@echo "  gridctl     - Build the SQLite -> PostgreSQL importer (bin/gridctl; docs/import.md)"
	@echo "  clean       - Clean build artifacts"
	@echo ""
	@echo "Operations targets:"
	@echo "  relay-token NAME=id - Mint a mesh-relay bootstrap admin token (prints the meshRelay.admins entry)"
	@echo "  ingest-token [REPORTER=id] [NAME=\"...\"] - Mint a push-ingest credential;"
	@echo "                with REPORTER it writes the hash into prefab.yaml (rotates if it exists)"
	@echo ""
	@echo "Database targets:"
	@echo "  db-up       - Start the local PostgreSQL (Docker) and wait for it"
	@echo "  db-down     - Stop it (the volume keeps the dev data)"
	@echo "  db-url      - Print the dev database URL"
	@echo ""
	@echo "Testing targets:"
	@echo "  test        - Run full test suite against the local PostgreSQL (works offline)"
	@echo "  test-google [ROUTE_ID=id] [VERBOSE=true]   - Test Google Routes API"
	@echo "  test-caltrans [VERBOSE=true] [FORMAT=table] - Test Caltrans KML feeds"
	@echo "  test-weather [LOCATION_ID=id] [VERBOSE=true] - Test OpenWeatherMap API"
	@echo "  test-config - Validate configuration without API calls"
	@echo "  fetch-test-data - Fetch test fixtures from live APIs (requires API keys)"
	@echo ""
	@echo "Development targets:"
	@echo "  run         - Run server (blocks until stopped with Ctrl+C)"
	@echo "  run-bg      - Run server in background (stops existing server first)"
	@echo "  run-relay   - Run the mesh relay in the foreground on :8090 (separate service)"
	@echo "  test-mesh-relay TOKEN=smc_... [RELAY_URL=url] - E2E check of a running mesh relay"
	@echo "  stop        - Stop background server (handles orphaned processes)"
	@echo "  test-server - Quick server startup test (3 seconds)"
	@echo "  dev         - Run server in development mode with auto-restart"
	@echo "  lint        - Run Go linting tools"
	@echo "  fmt         - Format Go code"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build     - Build Docker container image"
	@echo "  docker-build-relay - Build the mesh relay image (Dockerfile --target relay)"
	@echo "  docker-run       - Run Docker container with API keys from environment"
	@echo "  docker-run-dev   - Run Docker container with mounted config for development"
	@echo "  ecr-push         - Login to ECR and push both images"
	@echo "  ecr-push-grid    - Login to ECR and push the Grid server image"
	@echo "  ecr-push-relay   - Login to ECR and push the mesh relay image (RELAY_IMAGE_NAME)"
	@echo "  docker-push      - Push Docker image to registry (set DOCKER_REGISTRY)"
	@echo "  docker-clean     - Clean up Docker artifacts and containers"
	@echo ""
	@echo "Deployment targets (docs/deploy-handoff.md):"
	@echo "  deploy [REF=tag|commit] - Run the Deploy workflow for HEAD (main, pushed, clean, CI green) or REF, and watch it"
	@echo "  deploy-status - Recent Deploy workflow runs"
	@echo "  test-deploy-client - Test scripts/deploy.py and the rehearsal script"
	@echo "  rehearse-import REHEARSE_ARGS=... - Import rehearsal from an AWS Backup restore (migration gate 5; docs/import.md)"
	@echo "  (ecs-deploy/ecs-status/ecs-rollback are the legacy stack's; they run only on legacy/efs-sqlite)"
	@echo "  apidiff LEGACY=url NEW=url - Diff the API of the legacy and new servers (migration gate 2)"
	@echo "  docker      - Alias for docker-build"
	@echo "  install     - Install CLI tools to system PATH"
	@echo ""
	@echo "Utility targets:"
	@echo "  deps        - Update Go dependencies"
	@echo "  help        - Show this help message"

# Forces a file target to rebuild every time.
FORCE:

