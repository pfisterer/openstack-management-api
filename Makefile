PROJECT_NAME := openstack-management-api
BINARY_NAME := $(PROJECT_NAME)
SRC_DIR := ./cmd
DOC_DIR := ./internal/generated_docs
BUILD_DIR := ./tmp/build
GO_MOD := go.mod

SWAGGER_JSON := $(DOC_DIR)/swagger.json
OPENAPI_YAML := $(DOC_DIR)/openapi3.json
CLIENT_DIR := $(DOC_DIR)/client-typescript
CLIENT_PKG_DIR := ./client-npm
NPM_PKG_NAME := @dhbw-cloud/os-mgt-client
CLIENT_TS := $(CLIENT_DIR)/client.gen.ts
CLIENT_SDK := $(CLIENT_DIR)/sdk.gen.ts
DIST_DIR := $(DOC_DIR)/client-dist
EMBED_FILE := $(DOC_DIR)/embedded.go

# Docker Image details
DOCKER_REPO ?= ghcr.io/pfisterer/$(PROJECT_NAME)
DOCKER_TAG ?= $(shell cat VERSION)
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64

RP_API_DIR        := ./internal/roleprovider/api
RP_SWAGGER_SRC    := ../role-provider-service/internal/generated_docs/swagger.json

.DEFAULT_GOAL := all

.PHONY: all image build clean doc convert client bundle npm-package npm-pack npm-publish check swag run help install-npm bundle-deps docker docker-login docker-build multi-arch-build dev helm-update test generate-role-provider-client

all: test bundle build bundle-deps

# Like `all` but WITHOUT the test suite — used by the Docker image build so the
# CI image build stays fast and deterministic. Run tests locally via `make test`.
image: bundle build bundle-deps

# Start development server with live reload
dev:
	API_MODE=development air

# Install npm dependencies
install-npm:
	@echo "⬇️ Installing npm dependencies..."
	@npm install --silent
	@echo "✅ npm dependencies installed"

# Ensure swag is installed
check-swag:
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@latest

# Generate swagger.json using swag (OpenAPI 2.0)
generate-swagger-json: check-swag
	@echo "📚 Generating swagger.json..."
	@set -e; swag init -g $(SRC_DIR)/main.go -o $(DOC_DIR) --outputTypes json
	@echo "✅ swagger.json generated"

# Convert swagger.json (OpenAPI 2.0) to openapi3.json (OpenAPI 3.0)
convert-to-openapi3: generate-swagger-json install-npm
	@echo "🔁 Converting Swagger 2 → OpenAPI 3..."
	@set -e; \
	npx swagger2openapi $(SWAGGER_JSON) --outfile $(OPENAPI_YAML) --yaml=false --patch --warnOnly
	@echo "✅ OpenAPI v3 spec: $(OPENAPI_YAML)"

# Generate TypeScript client from OpenAPI 3 spec
client: convert-to-openapi3 install-npm
	@echo "📦 Generating TypeScript client..."
	@mkdir -p $(CLIENT_DIR)
	@set -e; \
	npx openapi-ts -i "file://$(abspath $(OPENAPI_YAML))" -o "$(CLIENT_DIR)" -c @hey-api/client-fetch
	rm -f $(OPENAPI_YAML)
	@echo "✅ TS client generated in $(CLIENT_DIR)"

# Bundle web UI dependencies into single JS file and embed into Go
bundle: client install-npm generate-role-provider-client
	@echo "📦 Bundling into a single JS file with esbuild..."
	@mkdir -p $(DIST_DIR)
	set -e; \
	npx esbuild "$(CLIENT_TS)" "$(CLIENT_SDK)" --bundle --outdir="$(DIST_DIR)" --format=esm --out-extension:.js=".mjs" --sourcemap
	npx esbuild "$(CLIENT_TS)" "$(CLIENT_SDK)" --bundle --outdir="$(DIST_DIR)" --format=cjs --sourcemap
	@echo "🧩 Copying VERSION file to $(DOC_DIR)..."
	@cp VERSION $(DOC_DIR)/VERSION
	@echo "🧩 Generating embedded.go for generated docs..."
	@mkdir -p $(DOC_DIR)
	@printf '%s\n' \
		'package generated_docs' \
		'' \
		'import "embed"' \
		'' \
		'//go:embed swagger.json' \
		'var SwaggerJSON string' \
		'//go:embed client-dist/*' \
		'var ClientDist embed.FS' \
		'//go:embed VERSION' \
		'var Version string' \
		> $(EMBED_FILE)
	@echo "✅ Bundled JS in $(DIST_DIR)/"
	@echo "✅ Embedded docs written to $(EMBED_FILE)"
	@echo "Deleting intermediate client files in $(CLIENT_DIR)..."
	@rm -rf $(CLIENT_DIR)
	@echo "✅ Bundled JS in $(DIST_DIR)/"

# Generate Go client from the role-provider-service Swagger spec
generate-role-provider-client: install-npm
	@if [ -f "$(RP_SWAGGER_SRC)" ]; then \
		echo "🔁 Copying swagger.json from $(RP_SWAGGER_SRC)..."; \
		cp "$(RP_SWAGGER_SRC)" "$(RP_API_DIR)/swagger.json"; \
	else \
		echo "⚠️  Warning: $(RP_SWAGGER_SRC) not found — using existing $(RP_API_DIR)/swagger.json"; \
	fi
	@echo "🔁 Converting role-provider-service swagger.json → OpenAPI 3..."
	@npx swagger2openapi $(RP_API_DIR)/swagger.json \
		--outfile $(RP_API_DIR)/openapi3.json \
		--yaml=false --patch --warnOnly
	@echo "📦 Generating Go client..."
	@command -v oapi-codegen >/dev/null 2>&1 || go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
	@oapi-codegen -config $(RP_API_DIR)/oapi-codegen.yaml $(RP_API_DIR)/openapi3.json
	@echo "✅ Go client generated in $(RP_API_DIR)/client.gen.go"

# ── npm client package ──────────────────────────────────────────────────────
# Builds the generated client as a publishable npm package. This is the same
# code the server embeds today; packaging it lets the UI depend on a VERSION
# instead of fetching it at runtime, so a missing operation becomes a build
# error there rather than a silent no-op in the browser.
npm-package: client
	@echo "📦 Assembling $(NPM_PKG_NAME)..."
	@rm -rf $(CLIENT_PKG_DIR)
	@mkdir -p $(CLIENT_PKG_DIR)/src
	@cp -R $(CLIENT_DIR)/. $(CLIENT_PKG_DIR)/src/
	@# The generated index.ts exports every operation and type, but not the
	@# client instance itself — and that is what the consumer configures.
	@printf '%s\n' \
		"export * from './index';" \
		"export { client } from './client.gen';" \
		> $(CLIENT_PKG_DIR)/src/entry.ts
	@VERSION=$$(cat VERSION | tr -d '\n'); printf '%s\n' \
		'{' \
		'  "name": "$(NPM_PKG_NAME)",' \
		"  \"version\": \"$$VERSION\"," \
		'  "description": "Generated TypeScript client for the DHBW Cloud $(PROJECT_NAME).",' \
		'  "license": "Apache-2.0",' \
		'  "type": "module",' \
		'  "exports": { ".": { "types": "./dist/entry.d.ts", "default": "./dist/index.mjs" } },' \
		'  "types": "./dist/entry.d.ts",' \
		'  "files": ["dist"],' \
		'  "publishConfig": { "access": "public" },' \
		'  "repository": { "type": "git", "url": "git+https://github.com/pfisterer/$(PROJECT_NAME).git" }' \
		'}' \
		> $(CLIENT_PKG_DIR)/package.json
	@# Bundled rather than compiled file-by-file: the generated sources import
	@# each other without file extensions, which every bundler tolerates and
	@# plain Node does not. One .mjs sidesteps the question.
	@npx esbuild $(CLIENT_PKG_DIR)/src/entry.ts --bundle --format=esm \
		--outfile=$(CLIENT_PKG_DIR)/dist/index.mjs
	@npx tsc $(CLIENT_PKG_DIR)/src/entry.ts --declaration --emitDeclarationOnly \
		--outDir $(CLIENT_PKG_DIR)/dist --module esnext --moduleResolution bundler \
		--target es2022 --skipLibCheck
	@echo "✅ Package assembled in $(CLIENT_PKG_DIR)"

# Build the tarball an npm publish would upload — for trying the package out
# in a consumer without publishing anything.
npm-pack: npm-package
	@cd $(CLIENT_PKG_DIR) && npm pack

# Publish the package to npm. A prerelease goes to the `next` dist-tag: npm
# would otherwise point `latest` at it, so a plain `npm install` would hand a
# consumer a -test.N build. Same channel split the images and chart tags use.
# Needs `npm login` once; publishConfig.access=public is already in the manifest.
npm-publish: npm-package
	@VERSION=$$(cat VERSION | tr -d '\n'); \
	case "$$VERSION" in \
		*-*) TAG=next ;; \
		*)   TAG=latest ;; \
	esac; \
	echo "📤 Publishing $(NPM_PKG_NAME)@$$VERSION (dist-tag: $$TAG)"; \
	cd $(CLIENT_PKG_DIR) && npm publish --tag "$$TAG"

# Run Go tests
test: check-modules
	@echo "🧪 Running Go tests..."
	@go test -cover -coverpkg=./... ./...
	@echo "✅ Tests complete"

# Build Go binary
build: check-modules
	@echo "🔨 Building Go binary..."
	@mkdir -p $(BUILD_DIR)
	@set -e; CGO_ENABLED=1 go build -o $(BUILD_DIR)/$(BINARY_NAME) $(SRC_DIR)/main.go
	@echo "✅ Go binary built (./$(BUILD_DIR)/$(BINARY_NAME))"

# Check for go.mod file
check-modules:
	@test -f $(GO_MOD) || (echo "❌ $(GO_MOD) is missing; run 'go mod init' first."; exit 1)

# Clean build and doc directories
clean:
	@echo "🧹 Cleaning directories..."
	@rm -rf $(BUILD_DIR) $(DOC_DIR)
	@echo "✅ Cleanup complete"

# Run the built Go binary
run: build
	@echo "🚀 Running the Go app..."
	@./$(BUILD_DIR)/$(BINARY_NAME)

# Build Docker image
docker-build:
	@echo "🏗️ Building Docker image $(DOCKER_REPO):$(DOCKER_TAG)..."
	docker build --progress=plain -t "$(DOCKER_REPO):$(DOCKER_TAG)" .
	@echo "✅ Docker image $(DOCKER_REPO):$(DOCKER_TAG) built."
	@echo "You can push it with: docker push $(DOCKER_REPO):$(DOCKER_TAG)"

# Run the docker container
docker-run: docker-build
	@echo "🚀 Running Docker container from image $(DOCKER_REPO):$(DOCKER_TAG)..."
	docker run --rm -p 8083:8083 --env-file .env "$(DOCKER_REPO):$(DOCKER_TAG)"

# Build and push multi-architecture Docker image
docker-multi-arch-build: helm-update
	@echo "🏗️ Building multi-architecture Docker image for $(DOCKER_PLATFORMS)..."
	docker buildx build \
		--progress plain \
		--platform $(DOCKER_PLATFORMS) \
		--tag "$(DOCKER_REPO):latest" \
		--tag "$(DOCKER_REPO):$(DOCKER_TAG)" \
		--push \
		.
	@echo "✅ Multi-architecture image $(DOCKER_REPO):$(DOCKER_TAG) built and pushed."
	@echo "You can pull it with: docker pull $(DOCKER_REPO):$(DOCKER_TAG)"

# Update helm chart version from VERSION file
helm-update:
	helm lint helm-chart/
	echo "✅ Helm chart linted successfully."

	@VERSION=$$(cat VERSION | tr -d '\n'); \
	sed -i '' "s/^version: .*/version: $$VERSION/" helm-chart/Chart.yaml; \
	sed -i '' "s/^appVersion: .*/appVersion: \"$$VERSION\"/" helm-chart/Chart.yaml; \
	echo "✅ Updated helm-chart/Chart.yaml to version $$VERSION"


# Update and install all dependencies
update-deps:
	@echo "📦 Updating Go dependencies..."
	go get -u ./...
	go mod tidy
	@echo "✅ Go dependencies updated."
	@echo "📦 Updating npm dependencies..."
	ncu -u && npm install
	@echo "✅ npm dependencies updated."

# Help target
help:
	@echo "Usage: make <target>"
	@echo "  all                     → Build and generate everything"
	@echo "  dev                     → Start development server with live reload (requires air)"
	@echo "  test                    → Run Go tests"
	@echo "  run                     → Run Go app"
	@echo "  build                   → Compile Go binary"
	@echo "  clean                   → Remove build artifacts"
	@echo "  install-npm             → Install npm dependencies from package.json"
	@echo "  check-swag              → Ensure swag is installed"
	@echo "  generate-swagger-json   → Generate swagger.json"
	@echo "  convert-to-openapi3     → Convert swagger.json → openapi3.json"
	@echo "  client                  → Generate TypeScript client"
	@echo "  bundle                  → Bundle client into JS"
	@echo "  bundle-deps             → Bundle web UI dependencies"
	@echo "  generate-role-provider-client      → Regenerate Go client from role-provider-service swagger.json"
	@echo "  docker-build            → Build Docker image"
	@echo "  docker-run              → Run Docker container"
	@echo "  docker-multi-arch-build → Build and push multi-architecture Docker image (requires buildx & Docker login)"
	@echo "  update-deps             → Update Go and npm dependencies"
	@echo "  helm-update             → Update Helm chart"