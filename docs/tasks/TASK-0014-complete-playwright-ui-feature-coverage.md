# TASK-0014: Complete Playwright UI Feature Coverage

## Status

Done

## Summary

Expand Playwright verification so every current dashboard UI feature has browser
coverage, including controls and states that were not covered by the initial
scan/detail test.

## Context

- `docs/requirements.md` section Dashboard
- `docs/task_acceptance_criteria.md`
- `docs/tasks/TASK-0013-playwright-ui-verification.md`
- `web/src/main.tsx`
- `tests/ui/dashboard.spec.ts`

## Scope

- Cover initial empty dashboard metrics and configured repository state.
- Cover the `Refresh` button in both success and error-banner paths.
- Cover the `Scan` button, repository status update, scan history, service
  catalog, service cards, warnings, and configuration references.
- Cover runtime filtering between all services, Docker Compose services, and
  Kubernetes services.
- Cover service detail content for source metadata, images, ports,
  dependencies, storage, exposure, configuration, namespace, kind, and resource
  name.
- Cover the `Check Health` button by using a deterministic fake Docker Engine
  endpoint and verifying live status rows, health badges, and metric updates.
- Cover all supported health states: `healthy`, `degraded`, `unhealthy`,
  `unknown`, and `error`.
- Fix any UI behavior that blocks truthful Playwright verification.

## Out Of Scope

- Cross-browser testing beyond Chromium.
- Visual snapshot testing.
- Real Docker socket or real Kubernetes cluster dependencies.

## Dependencies

- TASK-0013.

## Implementation Notes

- Keep the primary browser workflow against the real dashboard server and real
  scanner.
- Use a fake Docker Engine HTTP endpoint only for deterministic live Docker
  status.
- Use a controlled summary payload only for rendering all health states that
  are not all easy to produce from v1 runtime integrations in a lightweight
  browser test.

## Task-Specific Acceptance Criteria

- Playwright covers all current dashboard controls.
- Playwright covers repository overview, scan history, service catalog, service
  detail, runtime filtering, metrics, live status rows, and error banner
  behavior.
- Playwright verifies all supported health states render in metrics and badges.
- The UI detail panel respects the active runtime filter.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: run `make ui-e2e`.
- Unit/integration tests: run `make check`.
- Build/lint/format commands: run `make check`.
- Documentation sweep targets: task tracker and this task file.
- Maintainability review focus: test fixture determinism, test coverage scope,
  and UI state logic.
- Conventional commit type and summary: `test: expand playwright UI coverage`.

## Verification Evidence

- End-to-end test result: Passed with `make ui-e2e`; Chromium verified the real
  server workflow for empty state, refresh, scan, repository overview, scan
  history, service cards, runtime filtering, service detail, Check Health,
  Docker-backed live status, metric updates, and error banner behavior.
- Automated test result: Passed with `make check`, including Go tests,
  frontend typecheck/lint, production build, and two Playwright Chromium tests.
- Build result: Passed with `make check`, including Vite production asset build
  and Go dashboard binary build.
- Lint result: Passed with `make check`, including `go vet` and
  `npm run lint`.
- Formatting result: Passed with `make check`, including `gofmt` and
  `npm run format`.
- Documentation sweep result: Updated task tracker and added this task file.
- Maintainability sweep result: Passed. The real-server test uses a
  self-contained fixture repository and fake Docker endpoint; all-state
  rendering uses an isolated summary payload; the UI detail selection now
  follows the active runtime filter.
- Conventional commit: `test: expand playwright UI coverage`.
- Notes or exceptions: Degraded and error state rendering are verified with a
  controlled summary payload because v1 does not provide a lightweight local
  runtime that produces every health state without external Docker/Kubernetes
  dependencies.
