# Repository Guidelines

## Project Structure & Module Organization

GitOps Dashboard is one Go binary with an embedded React UI. `-mode` selects
its role: the **server** scans Git repositories for infrastructure definitions,
normalizes them into services in SQLite, monitors runtime health, and serves the
dashboard; the **agent** is the same binary dialing out over WebSocket to report
Docker container state from a remote host. Changes must keep both roles working.

- `cmd/gitops-dashboard/`: entry point, mode selection, config load.
- `cmd/release/`, `cmd/version-allocator/`: release entrypoint and SemVer
  allocation used by CI. `cmd/t032-e2e/` is an end-to-end helper.
- `internal/app/`: wiring, HTTP API, auth middleware, agent WebSocket endpoint,
  embedded asset serving, alert evaluation.
- `internal/scanner/`, `internal/parser/`, `internal/hostinventory/`: repository
  sync and discovery; Compose, Kubernetes, Traefik, and Ansible inventory
  parsing into `internal/core` service models.
- `internal/monitor/`, `internal/dockerapi/`, `internal/routetarget/`: runtime
  checks (Docker, Kubernetes, HTTP routes, ping) and status/history writes.
- `internal/storage/`: SQLite schema, ordered migrations, and queries.
- `internal/agent/`, `internal/agentprotocol/`: agent-mode collection and the
  server/agent wire protocol.
- `internal/alerter/`, `internal/config/`, `internal/auth/`,
  `internal/sanitizer/`, `internal/version/`, `internal/ci/`: alert delivery,
  YAML config, basic auth, secret redaction, build metadata, CI checks.
- `internal/ui/`: `embed.go` plus the `dist/` output that `npm run build`
  produces; the binary serves the UI from here, so a UI change is only visible
  after a rebuild.
- `web/src/`: React 19 + TypeScript dashboard. `tests/ui/`: Playwright specs.
- `docs/`: product and operational docs (`requirements.md`, `discovery.md`,
  `deployment.md`, `versioning.md`, `tech_stack.md`) plus per-task
  implementation records in `docs/tasks/`.
- `examples/`: runnable configuration samples. `scripts/`: release, formatting,
  and build-context audit helpers.

## Scope & Operating Model

Read `docs/requirements.md` before proposing anything ambitious. This is a
read-only, single-container dashboard for one homelab operator, not
production-grade infrastructure software:

- One active dashboard process per SQLite database. Overlapping restart is the
  only supported concurrency window; continuous multi-replica operation is not
  supported and correctness arguments that require live peers do not apply.
- Configuration is file based and operator controlled. The UI never edits
  configuration.
- Prefer the smallest change that makes ordinary use correct. Hardening against
  hostile local administration, hand-drifted database schemas, or contrived
  sub-second interleavings is out of proportion here.

## Build, Test, and Development Commands

One-time setup: `npm install && npm run test:e2e:install`.

- `make build`: build the UI, then the binary with version ldflags.
- `make test`: Go tests plus `npm test` (TypeScript typecheck and ESLint).
- `make ui-e2e`: build the binary and run Playwright against the real server.
- `make check`: the full CI gate — format check, lint, tests, build, UI e2e, and
  release test. It is **non-mutating**: it copies the tracked-and-untracked,
  non-ignored file manifest into a temporary directory and runs there, so
  uncommitted work is checked and ignored files (`data/`, `.env*`, keys) never
  are. Use `make check-local` to run the same gates in place when iterating.
- `make format`: the only mutating command — `gofmt -w` plus the whitespace
  check. Run it before `make check`, never instead of it.
- `make dev-server` / `make dev-ui`: backend on `:18080`, Vite dev server on
  `:5173` proxying to it.

Run the built binary directly with `./gitops-dashboard -config
examples/config.dev.yaml` (no auth) or `./gitops-dashboard -mode agent -config
examples/compose-config/agent.yaml`. `-version` prints build metadata.

Scope the gate to the change: a Go-only change needs `make test` while
iterating; a UI change needs `make ui-e2e` because the UI is embedded; a
docs-only change still needs `npm run format`. Run `make check` before handing
work off, whatever the change.

Direct `go` invocations should match the Makefile's environment:
`GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/...`.

## Coding Style & Naming Conventions

Go 1.24 formatted with `gofmt`; `go vet ./cmd/... ./internal/...` must stay
clean. Internal packages are short, lowercase, single-word names owning one
concern — keep parsing, monitoring, storage, and HTTP wiring on their existing
side of those boundaries rather than reaching across.

TypeScript is `strict` with `@typescript-eslint/no-explicit-any` set to `error`;
`any` will not pass lint. React components live in `web/src/components/` in
PascalCase files; shared helpers stay in `selectors.ts`, `api.ts`, `types.ts`.

`npm run format` is not Prettier — it runs `scripts/check-format.mjs`, which
fails on trailing whitespace in Go, TypeScript, CSS, HTML, JSON, YAML, Markdown,
`Dockerfile`, and `Makefile`. This file is checked by it too.

Code should be self-documenting. Comments explain a decision or a non-obvious
constraint, not what the next line does.

## Testing Guidelines

Go tests sit beside their source as `*_test.go`; fixtures live in `testdata/`.
Playwright specs live in `tests/ui/*.spec.ts`.

- Default tests must not require a live Docker daemon, Kubernetes cluster,
  network access, or real credentials. Use fakes and fixtures.
- Cover the normal success path plus the realistic failure that motivated the
  change. Exhaustive adversarial matrices are only warranted for genuinely
  remote or untrusted input.
- A change to a persisted format ships one representative pre-change fixture
  proving a database from a supported release still opens, migrates, and keeps
  its data.
- User-visible UI behavior needs Playwright coverage for the affected workflow.
- Record the exact commands run and their results in the task record.

`docs/task_acceptance_criteria.md` is the full bar and applies to every change.

## Commit & Pull Request Guidelines

Conventional Commits with a subsystem scope, matching recent history:
`feat(storage): index and retire scan rows past a 7-day terminal horizon
(T-068)`. The subject ends with the task ID when the work belongs to a tracked
task. No co-author trailers.

Every implementation keeps an in-repo record: `docs/tasks/TASK-NNNN-<slug>.md`
with scope, verification commands, and observed evidence, plus its row in
`docs/tasks/tracker.md`. That mirror exists for evidence only — an external
orchestration workspace owns task status, priority, and assignment, so do not
invent or edit status there beyond what the work you did demonstrates.

Do not commit until the work is approved, and treat CI as the completion gate.
Every push to `main` runs `make check` and, on success, allocates and publishes
a release automatically, so `main` must stay releasable at all times. A pull
request should state scope, the linked task record, the verification evidence,
and any residual risk.

## Security & Configuration Tips

- Stay read-only toward everything monitored: Git repositories, Docker hosts,
  and Kubernetes clusters are observed, never mutated.
- Secrets arrive through environment variables or mounted files. Route
  redaction through `internal/sanitizer`; consume it rather than widening it.
  Secret values must never reach logs, API responses, SQLite text, error
  strings, Git remotes, or subprocess arguments.
- Never add credentials, `data/`, kubeconfigs, or key material to the Docker
  build context. `.dockerignore` filters it and `scripts/docker-context-audit.sh`
  fails the image build if a forbidden path class survives.
- Release tags and published images are immutable. Never delete, recreate, or
  retarget an exact `vX.Y.Z` tag or its image; see `docs/versioning.md` for how
  the channel tags and `latest` converge.
- Keep the module path, endpoint contracts (`/api/summary`, `/api/scan`,
  `/api/monitor`, `/api/monitor-overrides`, `/api/agents/connect`), and CSRF
  behavior (`X-GitOps-Dashboard-CSRF: 1` on state-changing calls) intact unless
  a task says otherwise.
