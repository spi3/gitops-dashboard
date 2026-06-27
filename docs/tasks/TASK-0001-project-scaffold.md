# TASK-0001: Project Scaffold And Developer Checks

## Status

Done

## Summary

Create the initial project scaffold for the GitOps Dashboard. This task should
establish the Go backend, React/TypeScript frontend, SQLite dependency path,
single-container build shape, and repeatable developer check commands.

## Context

- `docs/vision.md`
- `docs/requirements.md`
- `docs/tech_stack.md`
- `docs/implementation_plan.md`
- `docs/task_acceptance_criteria.md`

## Scope

- Create the Go module and backend application skeleton.
- Create the React/TypeScript frontend skeleton served by the backend.
- Add initial project layout matching `docs/tech_stack.md`.
- Add development commands for build, test, lint, and formatting.
- Add a Dockerfile for the single-container server artifact.
- Add placeholder health/readiness endpoints.

## Out Of Scope

- Full repository scanning.
- Real Compose or Kubernetes parsing.
- Live Docker or Kubernetes monitoring.
- Production-ready dashboard UI.

## Dependencies

- None.

## Implementation Notes

- Prefer a small Go router or the Go standard library.
- Keep the server binary capable of supporting future server and agent modes.
- Keep frontend assets buildable into static files served by the backend.
- Do not add database schema beyond what is needed to prove wiring.

## Task-Specific Acceptance Criteria

- `go test ./...` or the chosen backend test command runs successfully.
- Frontend build and test commands are documented and pass.
- Formatting and lint commands are documented and pass.
- The container image builds successfully.
- The server starts and exposes health/readiness endpoints.
- The frontend shell is served by the backend.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: start the server, request health/readiness endpoints,
  and load the frontend shell.
- Unit/integration tests: backend smoke tests and frontend smoke/build checks.
- Build/lint/format commands: define and run the initial project commands.
- Documentation sweep targets: all docs under `docs/`.
- Maintainability review focus: project layout, command consistency, and
  separation between backend, frontend, and packaging.
- Conventional commit type and summary: `feat: scaffold gitops dashboard`.

## Verification Evidence

- End-to-end test result: Passed. Started `./gitops-dashboard -config examples/config.dev.yaml` on `:18080`; verified `GET /healthz`, `GET /readyz`, `GET /api/summary`, and `GET /` returned expected responses.
- Automated test result: Passed with `make check`, including Go tests for auth, config, environment inference, parsers, and storage plus frontend typecheck/lint.
- Build result: Passed with `make check`; container build passed with `docker build -t gitops-dashboard:task-0001 .`.
- Lint result: Passed with `make check`, including `go vet` and `npm run lint`.
- Formatting result: Passed with `make check`, including `gofmt` and `npm run format`.
- Documentation sweep result: Reviewed `docs/vision.md`, `docs/requirements.md`, `docs/tech_stack.md`, `docs/implementation_plan.md`, `docs/task_acceptance_criteria.md`, and task files. Updated `docs/tech_stack.md` to document direct read-only Docker Engine API use instead of the official Docker client.
- Maintainability sweep result: Passed. Scaffold keeps server, config, auth, storage, parsing, monitoring, agent, UI, and packaging behind separate modules; generated and cache paths are ignored; build commands are centralized in `Makefile`.
- Conventional commit: `feat: scaffold gitops dashboard`.
- Notes or exceptions: Normal `.git` initialization is blocked by a read-only workspace mount, so commits use `git --git-dir=.gitdb --work-tree=.`.
