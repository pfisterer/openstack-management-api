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
EMBED_FILE := $(DOC_DIR)/embedded.go

# Docker Image details
DOCKER_REPO ?= ghcr.io/pfisterer/$(PROJECT_NAME)
DOCKER_TAG ?= $(shell cat VERSION)
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64

RP_API_DIR        := ./internal/roleprovider/api
# The role-provider-service release whose API this client is generated from.
# Keep it in step with the version the umbrella chart pins next to this service
# — those two being one number is what makes a mismatch visible.
#
# Only stable releases carry the swagger.json asset: a "-test.N" push creates no
# GitHub release, and that omission is deliberate over there — it is what keeps
# production from resolving a prerelease.
#
# So while an API change is still in flight on a prerelease, override the URL
# rather than the version. role-provider-service serves its own spec at
# /swagger.json ahead of the bearer middleware, so any running instance is a
# valid source, and curl reads file:// as happily as https://:
#
#   make generate-role-provider-client \
#     RP_SWAGGER_URL=https://role-provider.staging.dhbw.cloud/swagger.json
#
#   make generate-role-provider-client \
#     RP_SWAGGER_URL=file://$$PWD/../role-provider-service/internal/generated_docs/swagger.json
#
# That is the old sibling-checkout behaviour, minus the part where it was the
# silent default. The committed value below stays a released version, so what
# lands on main is always reproducible from something published — and since the
# generated client is committed, an override made mid-flight shows up in the
# diff instead of hiding in someone's working copy.
RP_VERSION        ?= v0.6.7
RP_SWAGGER_URL    ?= https://github.com/pfisterer/role-provider-service/releases/download/$(RP_VERSION)/swagger.json

.DEFAULT_GOAL := all

.PHONY: all image build clean doc convert client bundle npm-package npm-pack npm-publish check swag run help install-npm docker docker-login docker-build multi-arch-build dev helm-update test generate-role-provider-client print-rp-swagger-url bump version-check

all: test bundle build

# Like `all` but WITHOUT the test suite — used by the Docker image build so the
# CI image build stays fast and deterministic. Run tests locally via `make test`.
image: bundle build

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

# Embed the API description into the binary. The TypeScript client used to be
# bundled in here too and served under /client; consumers take it from npm now
# ($(NPM_PKG_NAME)), so this step needs neither Node nor esbuild.
bundle: generate-swagger-json
	@echo "🧩 Copying VERSION file to $(DOC_DIR)..."
	@cp VERSION $(DOC_DIR)/VERSION
	@echo "🧩 Generating embedded.go for generated docs..."
	@mkdir -p $(DOC_DIR)
	@printf '%s\n' \
		'package generated_docs' \
		'' \
		'import _ "embed"' \
		'' \
		'//go:embed swagger.json' \
		'var SwaggerJSON string' \
		'//go:embed VERSION' \
		'var Version string' \
		> $(EMBED_FILE)
	@echo "✅ Embedded docs written to $(EMBED_FILE)"

# Where CI reads the URL from, so the pin lives in exactly one place. A copy of
# it in the workflow would be a second thing to move when RP_VERSION changes,
# and the check would keep passing against the old one.
print-rp-swagger-url:
	@echo "$(RP_SWAGGER_URL)"

# Generate Go client from the role-provider-service OpenAPI description.
#
# The spec comes from a released version of that service, not from a checkout
# next to this one. The old filesystem path meant the client could be generated
# from whatever happened to be on the developer's disk — and when the path was
# missing, the target printed a warning and carried on with the committed copy,
# which is how a client stays silently out of date. A failed download now stops
# the target.
generate-role-provider-client: install-npm
	@echo "🔁 Fetching swagger.json from role-provider-service $(RP_VERSION)..."
	@# Into a temporary file first: writing straight to the destination would
	@# leave a truncated or half-downloaded spec behind on failure, and the
	@# committed one is the only copy there is.
	@curl -fsSL "$(RP_SWAGGER_URL)" -o "$(RP_API_DIR)/swagger.json.tmp" || { \
		rm -f "$(RP_API_DIR)/swagger.json.tmp"; \
		echo "❌ $(RP_SWAGGER_URL) is not available."; \
		echo "   Releases made before role-provider-service started attaching the"; \
		echo "   spec carry no such asset. Either point RP_VERSION at a newer"; \
		echo "   release, or attach it to this one once, from that repository:"; \
		echo "     make generate-docs && gh release upload $(RP_VERSION) internal/generated_docs/swagger.json"; \
		exit 1; \
	}
	@mv "$(RP_API_DIR)/swagger.json.tmp" "$(RP_API_DIR)/swagger.json"
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
	@# Deliberately not a CI step: publishing is a rare, deliberate act and the
	@# login is interactive (npm opens a browser). Only prompts when the stored
	@# credentials are missing or expired, so publishing both services in a row
	@# usually asks once, not twice.
	@npm whoami >/dev/null 2>&1 || npm login
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

# --- Versioning -------------------------------------------------------------
# VERSION is the single source of truth; Chart.yaml has to agree with it. That
# matters more than it used to: once the chart is published as an OCI artifact,
# the number in Chart.yaml IS the artifact version, so a mismatch ships one
# release's contents under another release's name. `version-check` runs in CI
# and FAILS on a mismatch rather than quietly rewriting the file — rewriting
# would hide exactly the mistake it is meant to catch.

# make bump V=1.2.3
bump:
	@test -n "$(V)" || { echo "usage: make bump V=<x.y.z>"; exit 1; }
	@printf '%s' "$(V)" > VERSION
	@$(MAKE) --no-print-directory helm-update

version-check:
	@v=$$(cat VERSION | tr -d '\n'); \
	cv=$$(awk '/^version:/{print $$2}' helm-chart/Chart.yaml); \
	av=$$(awk '/^appVersion:/{gsub(/"/,"",$$2); print $$2}' helm-chart/Chart.yaml); \
	if [ "$$v" != "$$cv" ] || [ "$$v" != "$$av" ]; then \
		echo "✗ VERSION is $$v, Chart.yaml says version=$$cv appVersion=$$av"; \
		echo "  fix with: make bump V=$$v"; \
		exit 1; \
	fi; \
	echo "✅ version $$v is consistent"

# Update helm chart version from VERSION file
helm-update:
	helm lint helm-chart/
	@echo "✅ Helm chart linted successfully."
	@VERSION=$$(cat VERSION | tr -d '\n'); \
	sed -e "s/^version: .*/version: $$VERSION/" \
	    -e "s/^appVersion: .*/appVersion: \"$$VERSION\"/" \
	    helm-chart/Chart.yaml > helm-chart/Chart.yaml.tmp; \
	mv helm-chart/Chart.yaml.tmp helm-chart/Chart.yaml; \
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
	@echo "              → Bundle web UI dependencies"
	@echo "  generate-role-provider-client      → Regenerate Go client from role-provider-service swagger.json"
	@echo "  docker-build            → Build Docker image"
	@echo "  docker-run              → Run Docker container"
	@echo "  docker-multi-arch-build → Build and push multi-architecture Docker image (requires buildx & Docker login)"
	@echo "  update-deps             → Update Go and npm dependencies"
	@echo "  helm-update             → Update Helm chart"