.PHONY: build check format lint test ui-build ui-lint ui-test ui-e2e go-test

build: ui-build
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false ./cmd/gitops-dashboard

check: format lint test build ui-e2e

format:
	gofmt -w cmd internal
	npm run format

lint: ui-lint
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...

test: go-test ui-test

go-test:
	GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...

ui-build:
	npm run build

ui-lint:
	npm run lint

ui-test:
	npm test

ui-e2e: build
	npm run test:e2e
