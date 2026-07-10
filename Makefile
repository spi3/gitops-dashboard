VITE_API_TARGET ?= http://127.0.0.1:18080
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
VERSION ?= dev-$(COMMIT)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/example/gitops-dashboard/internal/version.Version=$(VERSION) -X github.com/example/gitops-dashboard/internal/version.Commit=$(COMMIT) -X github.com/example/gitops-dashboard/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: build check check-local dev-server dev-ui format format-check lint test ui-build ui-lint ui-test ui-e2e go-test release-test

build: ui-build
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "$(LDFLAGS)" ./cmd/gitops-dashboard

check:
	@set -eu; \
	tmp=""; \
	checkout=""; \
	trap 'if [ -n "$$tmp" ]; then rm -rf "$$tmp"; fi' EXIT; \
	tmp_base="$${TMPDIR:-/tmp}"; \
	tmp="$$(mktemp -d "$$tmp_base/gitops-dashboard-check.XXXXXX")"; \
	checkout="$$tmp/checkout"; \
	manifest="$$tmp/release-test-files.nul"; \
	mkdir "$$checkout"; \
	if [ -z "$$tmp" ] || [ ! -d "$$tmp" ] || [ -z "$$checkout" ] || [ ! -d "$$checkout" ] || [ "$$checkout" = "/" ] || [ -z "$$manifest" ]; then \
		echo "invalid check workspace: $$checkout" >&2; \
		exit 1; \
	fi; \
	if ! git ls-files --cached --others --exclude-standard -z > "$$manifest"; then \
		echo "failed to create release-test file manifest" >&2; \
		exit 1; \
	fi; \
	if [ ! -s "$$manifest" ]; then \
		echo "release-test file manifest is empty" >&2; \
		exit 1; \
	fi; \
	rsync -a --delete --from0 --files-from="$$manifest" ./ "$$checkout"/; \
	if [ -d node_modules ]; then \
		ln -s "$$(pwd)/node_modules" "$$checkout/node_modules"; \
	fi; \
	RELEASE_TEST_FILE_MANIFEST="$$manifest" $(MAKE) -C "$$checkout" check-local

check-local: format-check lint test build ui-e2e release-test

dev-server:
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go run ./cmd/gitops-dashboard -config examples/config.dev.yaml

dev-ui:
	VITE_API_TARGET=$(VITE_API_TARGET) npm run dev

format:
	gofmt -w cmd internal
	npm run format

format-check:
	@set -eu; \
	files="$$(mktemp "$${TMPDIR:-/tmp}/gitops-dashboard-gofmt.XXXXXX")"; \
	trap 'rm -f "$$files"' EXIT; \
	gofmt -l cmd internal > "$$files"; \
	if [ -s "$$files" ]; then \
		cat "$$files"; \
		exit 1; \
	fi
	npm run format

lint: ui-build ui-lint
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...

test: go-test ui-test

go-test: ui-build
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...

release-test:
	bash scripts/release_test.sh

ui-build:
	npm run build

ui-lint:
	npm run lint

ui-test:
	npm test

ui-e2e: build
	npm run test:e2e
