# Live Data API Server - Build, Test, and Deployment Tasks
.PHONY: build test proto proto-tools clean server tools site site-modules site-install site-dev site-shots site-shots-mock check-site check-wiring run dev lint fmt docker docker-build docker-run docker-run-dev docker-push docker-clean deploy install help test-meshcore

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
# (protoc@29.3); the plugins below are installed by `make proto-tools` into
# GOPATH/bin, which the proto recipe prepends to PATH (so they win over any
# floating container-provided versions).
PROTOC_GEN_GO_VERSION=v1.36.8       # == google.golang.org/protobuf
PROTOC_GEN_GO_GRPC_VERSION=v1.5.1
GRPC_GATEWAY_VERSION=v2.27.2        # == github.com/grpc-ecosystem/grpc-gateway/v2 (gateway + openapiv2)
CMD_DIR=cmd

# Binary names
SERVER_BINARY=$(BUILD_DIR)/server
TEST_GOOGLE_BINARY=$(BUILD_DIR)/test-google
TEST_CALTRANS_BINARY=$(BUILD_DIR)/test-caltrans
TEST_WEATHER_BINARY=$(BUILD_DIR)/test-weather
TEST_MESHCORE_BINARY=$(BUILD_DIR)/test-meshcore
TEST_GEO_UTILS_BINARY=$(BUILD_DIR)/test-geo-utils
TEST_ALERT_ENHANCER_BINARY=$(BUILD_DIR)/test-alert-enhancer
TEST_ROUTE_MATCHER_BINARY=$(BUILD_DIR)/test-route-matcher

# Docker parameters
DOCKER_IMAGE_NAME=sierra-data
DOCKER_TAG?=latest
DOCKER_REGISTRY?=

# Default target
all: build

## Build Targets

# Build everything (protobuf generation + server + CLI tools)
build: proto server tools

# Build main server only
server: $(SERVER_BINARY)

$(SERVER_BINARY): proto
	$(GOBUILD) -o $(SERVER_BINARY) ./$(CMD_DIR)/server

# Build CLI testing tools only
tools: $(TEST_GOOGLE_BINARY) $(TEST_CALTRANS_BINARY) $(TEST_WEATHER_BINARY) $(TEST_MESHCORE_BINARY)

$(TEST_GOOGLE_BINARY): proto
	$(GOBUILD) -o $(TEST_GOOGLE_BINARY) ./$(CMD_DIR)/test-google

$(TEST_CALTRANS_BINARY): proto
	$(GOBUILD) -o $(TEST_CALTRANS_BINARY) ./$(CMD_DIR)/test-caltrans

$(TEST_WEATHER_BINARY): proto
	$(GOBUILD) -o $(TEST_WEATHER_BINARY) ./$(CMD_DIR)/test-weather

$(TEST_MESHCORE_BINARY): proto
	$(GOBUILD) -o $(TEST_MESHCORE_BINARY) ./$(CMD_DIR)/test-meshcore

# Build the static site with Astro (source in web/) into site/dist. The output
# is a COMMITTED artifact (like the generated *.pb.go) so the Docker `go build`
# stays Node-free — run this after editing anything under web/, then commit
# Uses npm ci into an off-mount dependency store; see SITE_DEPS below.
# Where the site's npm dependencies actually live.
#
# Files written into /workspace — a virtiofs bind mount — bit-corrupt in place
# (the same fault class as the SQLite corruption in internal/store/CLAUDE.md).
# npm writes ~188MB of them, and one flipped byte in @astrojs/compiler's
# astro.wasm or in any dist/*.js kills the build with an error naming an
# innocent .astro file. So node_modules lives OFF the mount and web/node_modules
# is a symlink to it.
#
# Crucially the INSTALL also has to happen off the mount: `npm ci` deletes and
# recreates node_modules, which replaces the symlink with a real directory back
# on the corrupting filesystem. So we keep a copy of package.json +
# package-lock.json in $(SITE_DEPS) and run npm there, then link. Override
# SITE_DEPS for a different host.
SITE_DEPS ?= $(HOME)/.cache/grid-web-deps

site-modules:
	@mkdir -p "$(SITE_DEPS)"
	@# Reinstall only when the lockfile actually changed.
	@if ! cmp -s web/package-lock.json "$(SITE_DEPS)/package-lock.json"; then \
		echo "Installing site deps off-mount → $(SITE_DEPS)"; \
		cp web/package.json web/package-lock.json "$(SITE_DEPS)/"; \
		(cd "$(SITE_DEPS)" && npm ci) || exit 1; \
	fi
	@# `rm -rf` on a symlink removes the link, not the target.
	@rm -rf web/node_modules
	@ln -sfn "$(SITE_DEPS)/node_modules" web/node_modules

site: site-modules check-wiring
	@echo "Building site (Astro → site/dist)..."
	cd web && npm run build
	@# Astro emits build-internal *.mjs at the dist root (content-*.mjs stubs, a
	@# hash-named manifest_*.mjs) that are never served and whose hashes churn the
	@# committed output. Prune all root-level *.mjs; real page JS lives in assets/.
	rm -f site/dist/*.mjs
	@echo "✅ Site built: site/dist"

# Install site build dependencies only (no build).
site-install: site-modules
	@echo "✅ Site deps installed at $(SITE_DEPS) (web/node_modules → symlink)"

# Run the Astro dev server (hot reload) for local site development. Note: /v1 and
# /api calls still need the Go server running — point the dev site at it or proxy.
site-dev: site-modules
	cd web && npm run dev

# Screenshot + layout-metrics of the running site for spacing/layout diagnosis.
# Uses the container's Playwright + Chromium (the moat.yaml `playwright` dep) and
# a server already running — point BASE_URL at it (default :8190). Set LABEL to
# tag a run (e.g. before/after) for numeric diffs. Output lands under
# tools/screenshots/out/<LABEL>/ (git-ignored). NODE_PATH points at the global
# npm root so the ESM script's require() finds the globally-installed playwright.
site-shots:
	cd tools/screenshots && NODE_PATH=$$(npm root -g) \
		BASE_URL=$(or $(BASE_URL),http://localhost:8190) LABEL=$(or $(LABEL),current) node shots.mjs

# Self-contained screenshots with MOCKED /api/v1 data — no server, no API keys,
# deterministic. Builds the site, serves site/dist, intercepts every /api/v1
# fetch from web/screenshots/fixtures.mjs so pages render populated state, and
# captures each page at phone/tablet/desktop into web/screenshots/out/
# (git-ignored). This is the one to reach for when reviewing the rendered UI —
# especially mobile — without standing up the Go server + live upstreams.
# Pass PAGES=events,map or ONLY=mobile to narrow a run. Uses web/'s own
# Playwright devDependency (not the global one site-shots uses).
site-shots-mock: site-modules
	cd web && npm run build
	cd web && node screenshots/capture.mjs $(if $(PAGES),--pages $(PAGES),) $(if $(ONLY),--only $(ONLY),)

# Islands bind to markup by id string, so a rename on one side only surfaces in
# a browser as a null-deref halfway through init. This is that check, statically:
# every getElementById/requireEls id in an island must exist in its page. It runs
# as part of `site`, so the mismatch cannot reach a build.
check-wiring:
	@cd web && node screenshots/wiring-check.mjs

# Guard against deploying a stale site. The Docker image embeds the COMMITTED
# site/dist (the image build is deliberately Node-free), so a change under web/
# that wasn't rebuilt+committed would ship silently stale. This rebuilds the
# site and fails if the committed site/dist drifted from source — run it before
# building the deploy image (it's a docker-build prerequisite). Needs Node (the
# build host has it); the image build itself still never runs Node.
check-site:
	@$(MAKE) --no-print-directory site >/dev/null
	@git diff --quiet -- site/dist || { \
		echo "❌ site/dist is stale — run 'make site' and commit before deploying:"; \
		git --no-pager diff --stat -- site/dist; \
		exit 1; }
	@echo "✅ site/dist is in sync with web/ source"

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

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f $(PROTO_DIR)/*.pb.go
	rm -f $(PROTO_DIR)/*_grpc.pb.go
	rm -f $(PROTO_DIR)/*.swagger.json
	rm -f $(GRID_PROTO_DIR)/*.pb.go
	rm -rf site/dist web/node_modules web/.astro

## Testing Targets

# Run full test suite
test:
	$(GOTEST) -v ./...

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

# Validate configuration without API calls
test-config:
	@echo "Configuration validation not yet implemented"

# Fetch timestamped test data snapshots from live APIs
fetch-test-data: fetch-caltrans-data fetch-google-data fetch-weather-data

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

# Run server with configuration
run: server
	./$(SERVER_BINARY)

# Run server in background for testing
run-bg: server stop
	@echo "Starting server in background..."
	@nohup ./$(SERVER_BINARY) > server.log 2>&1 & echo $$! > server.pid
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
# tree). The image itself only compiles - it does not regenerate or test.
docker-build: proto test check-site
	@echo "Building Docker image: $(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"
	docker build \
		--platform linux/amd64 \
		--build-arg GOOGLE_API_KEY=$(PF__GOOGLE_ROUTES__API_KEY) \
		--build-arg OPENWEATHER_API_KEY=$(PF__OPENWEATHER__API_KEY) \
		--build-arg OPENAI_API_KEY=$(PF__OPENAI__API_KEY) \
		-t $(DOCKER_IMAGE_NAME):$(DOCKER_TAG) .
	@echo "✅ Docker image built: $(DOCKER_IMAGE_NAME):$(DOCKER_TAG)"

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

# Login to ECR and push Docker image
ecr-push: docker-build
	@if [ -z "$(DOCKER_REGISTRY)" ]; then \
		echo "⚠️  DOCKER_REGISTRY not set. Set with: make ecr-push DOCKER_REGISTRY=your-ecr-registry"; \
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

# Deploy to ECS (update existing service with latest image)
ecs-deploy: ecr-push
	@if [ -z "$(ECS_CLUSTER)" ]; then \
		echo "⚠️  ECS_CLUSTER not set. Set with: make ecs-deploy ECS_CLUSTER=your-cluster"; \
		exit 1; \
	fi
	@if [ -z "$(ECS_SERVICE)" ]; then \
		echo "⚠️  ECS_SERVICE not set. Set with: make ecs-deploy ECS_SERVICE=your-service"; \
		exit 1; \
	fi
	@if [ -z "$(ECS_TASK_DEFINITION)" ]; then \
		echo "⚠️  ECS_TASK_DEFINITION not set. Set with: make ecs-deploy ECS_TASK_DEFINITION=your-task-def"; \
		exit 1; \
	fi
	@echo "Deploying $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG) to ECS..."
	@echo "Cluster: $(ECS_CLUSTER)"
	@echo "Service: $(ECS_SERVICE)"
	@echo "Task Definition: $(ECS_TASK_DEFINITION)"
	@echo "Updating task definition with new image..."
	aws ecs describe-task-definition --task-definition $(ECS_TASK_DEFINITION) --query 'taskDefinition' > /tmp/task-def.json
	@# Update the image in the task definition
	cat /tmp/task-def.json | jq --arg IMAGE "$(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG)" '.containerDefinitions[0].image = $$IMAGE | del(.taskDefinitionArn, .revision, .status, .requiresAttributes, .placementConstraints, .compatibilities, .registeredAt, .registeredBy)' > /tmp/new-task-def.json
	@echo "Registering new task definition..."
	aws ecs register-task-definition --cli-input-json file:///tmp/new-task-def.json --query 'taskDefinition.taskDefinitionArn' --output text > /tmp/new-task-arn.txt
	@echo "Updating service to use new task definition..."
	aws ecs update-service --cluster $(ECS_CLUSTER) --service $(ECS_SERVICE) --task-definition $$(cat /tmp/new-task-arn.txt) --force-new-deployment --no-cli-pager --query 'service.taskDefinition' --output text
	@echo "Waiting for deployment to complete..."
	aws ecs wait services-stable --cluster $(ECS_CLUSTER) --services $(ECS_SERVICE)
	@echo "✅ Deployment completed successfully!"
	@rm -f /tmp/task-def.json /tmp/new-task-def.json /tmp/new-task-arn.txt

# Clean up Docker artifacts
docker-clean:
	@echo "Cleaning up Docker artifacts..."
	-docker stop sierra-server sierra-server-dev 2>/dev/null || true
	-docker rm sierra-server sierra-server-dev 2>/dev/null || true
	-docker rmi $(DOCKER_IMAGE_NAME):$(DOCKER_TAG) 2>/dev/null || true
	-docker system prune -f
	@echo "✅ Docker artifacts cleaned"

## Deployment Targets

# Deploy to configured environment
deploy:
	@echo "Deployment not yet implemented"

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
	@echo "  proto       - Generate protobuf code"
	@echo "  site        - Build the static site (Astro, web/ → site/dist; commit the output)"
	@echo "  site-dev    - Run the Astro dev server (hot reload)"
	@echo "  site-shots [BASE_URL=url] [LABEL=tag] - Screenshot + layout metrics of a running site (Playwright)"
	@echo "  check-site  - Fail if committed site/dist drifted from web/ source (docker-build prerequisite)"
	@echo "  clean       - Clean build artifacts"
	@echo ""
	@echo "Testing targets:"
	@echo "  test        - Run full test suite (unit tests, works offline)"
	@echo "  test-unit   - Run unit tests only"
	@echo "  test-contract - Run contract tests"
	@echo "  test-google [ROUTE_ID=id] [VERBOSE=true]   - Test Google Routes API"
	@echo "  test-caltrans [VERBOSE=true] [FORMAT=table] - Test Caltrans KML feeds"
	@echo "  test-weather [LOCATION_ID=id] [VERBOSE=true] - Test OpenWeatherMap API"
	@echo "  test-config - Validate configuration without API calls"
	@echo "  fetch-test-data - Fetch test fixtures from live APIs (requires API keys)"
	@echo ""
	@echo "Development targets:"
	@echo "  run         - Run server (blocks until stopped with Ctrl+C)"
	@echo "  run-bg      - Run server in background (stops existing server first)"
	@echo "  stop        - Stop background server (handles orphaned processes)"
	@echo "  test-server - Quick server startup test (3 seconds)"
	@echo "  dev         - Run server in development mode with auto-restart"
	@echo "  lint        - Run Go linting tools"
	@echo "  fmt         - Format Go code"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build     - Build Docker container image"
	@echo "  docker-run       - Run Docker container with API keys from environment"
	@echo "  docker-run-dev   - Run Docker container with mounted config for development"
	@echo "  ecr-push         - Login to ECR and push Docker image"
	@echo "  docker-push      - Push Docker image to registry (set DOCKER_REGISTRY)"
	@echo "  docker-clean     - Clean up Docker artifacts and containers"
	@echo ""
	@echo "Deployment targets:"
	@echo "  ecs-deploy  - Deploy latest image to ECS (set ECS_CLUSTER, ECS_SERVICE, ECS_TASK_DEFINITION)"
	@echo "  docker      - Alias for docker-build"
	@echo "  deploy      - Deploy to configured environment"
	@echo "  install     - Install CLI tools to system PATH"
	@echo ""
	@echo "Utility targets:"
	@echo "  deps        - Update Go dependencies"
	@echo "  help        - Show this help message"