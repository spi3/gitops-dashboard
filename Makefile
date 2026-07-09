VITE_API_TARGET ?= http://127.0.0.1:18080
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
VERSION ?= dev-$(COMMIT)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/example/gitops-dashboard/internal/version.Version=$(VERSION) -X github.com/example/gitops-dashboard/internal/version.Commit=$(COMMIT) -X github.com/example/gitops-dashboard/internal/version.BuildDate=$(BUILD_DATE)

.PHONY: build check dev-server dev-ui format lint test ui-build ui-lint ui-test ui-e2e go-test

build: ui-build
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "$(LDFLAGS)" ./cmd/gitops-dashboard

check: format lint test build ui-e2e

dev-server:
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go run ./cmd/gitops-dashboard -config examples/config.dev.yaml

dev-ui:
	VITE_API_TARGET=$(VITE_API_TARGET) npm run dev

format:
	gofmt -w cmd internal
	npm run format

lint: ui-build ui-lint
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...

test: go-test ui-test

go-test: ui-build
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...

ui-build:
	npm run build

ui-lint:
	npm run lint

ui-test:
	npm test

ui-e2e: build
	npm run test:e2e
