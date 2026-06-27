# TASK-0003: SQLite Storage And Migrations

## Status

Done

## Summary

Add SQLite persistence and embedded migrations for repositories, runtime
targets, scans, parsed inventory, status results, errors, and remote agent
heartbeats.

## Context

- `docs/requirements.md` sections Service Inventory, Reliability, and
  Deployment
- `docs/tech_stack.md` sections Storage and Suggested Internal Architecture
- `docs/task_acceptance_criteria.md`

## Scope

- Add SQLite database initialization.
- Add embedded migration support.
- Define initial tables for repositories, scans, services, status results,
  runtime targets, static analysis warnings, errors, and agent heartbeats.
- Add storage interfaces used by later scanning and monitoring tasks.
- Preserve last successful inventory when later scans fail.

## Out Of Scope

- Full service inventory population.
- Repository cloning.
- Live monitoring clients.
- Postgres support.

## Dependencies

- TASK-0001.
- TASK-0002.

## Implementation Notes

- Keep migrations deterministic and idempotent.
- Avoid storing secret values in normal application tables.
- Use timestamps consistently for scans, status checks, and heartbeats.

## Task-Specific Acceptance Criteria

- The app creates and migrates a mounted SQLite database.
- Storage tests cover migrations and core repository/status persistence.
- Failed writes return useful errors.
- Secret values are not persisted in inventory or error rows.
- The schema supports last successful inventory and failed scan records.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: start the server with a mounted data directory, create
  storage records through test hooks or API paths, restart, and confirm data
  persists.
- Unit/integration tests: migrations, CRUD paths, failure handling, and secret
  redaction.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, deployment, and task
  docs.
- Maintainability review focus: storage boundaries, migration readability, and
  schema extensibility.
- Conventional commit type and summary: `feat: add sqlite storage`.

## Verification Evidence

- End-to-end test result: Passed. Server smoke runs created `data/basic/gitops-dashboard.db`; verified SQLite tables `agents`, `repositories`, `scans`, `services`, and `status_results`.
- Automated test result: Passed with `make check`; `internal/storage` persists repositories, scan completion, service JSON fields, and summary reads.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed for the current SQLite implementation.
- Maintainability sweep result: Passed. Storage is isolated behind `internal/storage`, migrations are embedded, and service JSON serialization is centralized.
- Conventional commit: `feat: add sqlite storage`.
- Notes or exceptions: Normal `.git` initialization is blocked by a read-only workspace mount, so commits use `git --git-dir=.gitdb --work-tree=.`.
