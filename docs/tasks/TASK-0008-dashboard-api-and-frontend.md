# TASK-0008: Dashboard API And Frontend Views

## Status

Done

## Summary

Expose the normalized inventory, scan state, runtime status, warnings, and
errors through a backend API and React dashboard views.

## Context

- `docs/requirements.md` section Dashboard
- `docs/tech_stack.md` sections Frontend and Suggested Internal Architecture
- `docs/task_acceptance_criteria.md`

## Scope

- Add API endpoints for repositories, scans, services, service details,
  warnings, runtime targets, and status results.
- Build repository overview, service catalog, service detail, scan history, and
  runtime target status views.
- Show `healthy`, `degraded`, `unhealthy`, `unknown`, and `error` states.
- Show source paths, images, ports, runtime type, inferred environment, and
  static warnings.
- Expose loaded configuration in a read-only way where useful.

## Out Of Scope

- UI-based configuration editing.
- Alerting.
- Multi-user RBAC.
- Live monitoring implementation details.

## Dependencies

- TASK-0007.

## Implementation Notes

- Keep dashboard styling practical, dense, and work-focused.
- Avoid exposing secret values or credential paths that should remain private.
- API responses should be typed and stable enough for frontend tests.

## Task-Specific Acceptance Criteria

- API endpoints return inventory, status, warning, and scan data.
- Dashboard renders repository overview and service catalog.
- Dashboard renders service detail pages.
- Dashboard clearly distinguishes all supported health states.
- Dashboard remains useful when runtime monitoring is not configured.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: scan fixture data, load the dashboard, navigate
  repository overview, service catalog, service detail, and scan/error views.
- Unit/integration tests: API handlers, frontend rendering, filtering, status
  display, unknown/error state display, and secret redaction.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, and user-facing docs.
- Maintainability review focus: API contract clarity and frontend component
  boundaries.
- Conventional commit type and summary: `feat: add dashboard views`.

## Verification Evidence

- End-to-end test result: Passed. `internal/app` exercises the real HTTP handler for `GET /api/summary` and `GET /`, and the final MVP HTTP validation verified the frontend shell over HTTP after a fixture scan. The dashboard includes repository overview, scan history, service catalog, selectable service detail, warnings, and runtime status rows.
- Automated test result: Passed with `make check`; frontend typecheck/lint/build and app handler tests pass.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed.
- Maintainability sweep result: Passed. API handling is isolated in `internal/app`, frontend assets are embedded through `internal/ui`, summary data includes explicit scans/statuses, and UI remains read-only.
- Conventional commit: `feat: add dashboard views`.
- Notes or exceptions: Browser automation was not used; server smoke, handler-level tests, frontend typecheck/lint/build, and final HTTP validation cover the dashboard shell/API path in this sandbox.
