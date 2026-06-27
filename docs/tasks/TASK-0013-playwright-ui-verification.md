# TASK-0013: Playwright UI Verification

## Status

Done

## Summary

Add automated Playwright browser verification for the dashboard UI so the MVP
has end-to-end coverage of the rendered workflow, not only API and server smoke
checks.

## Context

- `docs/requirements.md` section Dashboard
- `docs/tech_stack.md` section Testing Strategy
- `docs/task_acceptance_criteria.md`
- `docs/tasks/TASK-0012-mvp-validation-and-doc-sweep.md`

## Scope

- Add Playwright as a reproducible project dev dependency.
- Add a Playwright config and browser test under `tests/ui/`.
- Have the browser test create a temporary Git fixture repository with Docker
  Compose and Kubernetes manifests.
- Have the browser test launch the built dashboard server, scan the fixture
  through the UI, and verify repository, service catalog, scan history, service
  detail, and no browser warning/error output.
- Add a Makefile target and include the Playwright check in the project check
  flow.

## Out Of Scope

- Cross-browser matrix beyond Chromium.
- Visual snapshot testing.
- CI workflow creation.

## Dependencies

- TASK-0012.

## Implementation Notes

- Keep the Playwright test self-contained by creating temporary fixture data at
  runtime.
- Use the built Go dashboard binary so the browser test validates the embedded
  production frontend assets and real API.
- Use `npm run test:e2e:install` to install the local Chromium browser bundle
  on fresh development or CI hosts before running Playwright.
- Ignore generated Playwright reports and test artifacts.

## Task-Specific Acceptance Criteria

- Playwright runs with `npm run test:e2e`.
- `make ui-e2e` builds the dashboard and runs the Playwright test.
- `make check` includes the Playwright UI verification.
- The test verifies a real browser workflow for repository scan, service
  catalog rendering, service detail rendering, scan history, and clean browser
  console output.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: run `make ui-e2e`.
- Unit/integration tests: run the existing automated suite through
  `make check`.
- Build/lint/format commands: run `make check`.
- Documentation sweep targets: task tracker, task acceptance criteria, and tech
  stack testing strategy.
- Maintainability review focus: Playwright fixture isolation, generated artifact
  ignores, and check command integration.
- Conventional commit type and summary: `test: add playwright UI verification`.

## Verification Evidence

- End-to-end test result: Passed with `make ui-e2e`; Chromium scanned a
  temporary fixture repository through the dashboard UI and verified repository,
  services, scan history, service detail, and clean browser console output.
- Automated test result: Passed with `make check`, including Go tests,
  frontend typecheck/lint, and Playwright Chromium UI verification.
- Build result: Passed with `make check`, including Vite production asset build
  and Go dashboard binary build.
- Lint result: Passed with `make check`, including `go vet` and
  `npm run lint`.
- Formatting result: Passed with `make check`, including `gofmt` and
  `npm run format`.
- Documentation sweep result: Updated task tracker, shared task acceptance
  criteria, and tech stack testing strategy.
- Maintainability sweep result: Added self-contained Playwright fixture setup,
  ignored generated reports, and wired E2E through Makefile without requiring
  committed test data.
- Conventional commit: `test: add playwright UI verification`.
- Notes or exceptions: Chromium is the initial browser target for v1.
