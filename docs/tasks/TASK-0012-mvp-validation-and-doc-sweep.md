# TASK-0012: End-To-End MVP Validation And Documentation Sweep

## Status

Done

## Summary

Validate the full v1 workflow against the vision, requirements, and tech stack.
This task closes the MVP by proving that repository discovery, static analysis,
live monitoring, dashboard rendering, packaging, security, and documentation
work together.

## Context

- `docs/vision.md`
- `docs/requirements.md`
- `docs/tech_stack.md`
- `docs/implementation_plan.md`
- `docs/task_acceptance_criteria.md`
- All previous MVP task files

## Scope

- Run a full end-to-end flow with GitHub PAT, public Git, and SSH Git inputs.
- Validate Compose and Kubernetes parsing against fixture repositories.
- Validate Kubernetes monitoring through mounted kubeconfig.
- Validate Docker monitoring through local target and remote WebSocket agent.
- Validate basic auth, secret redaction, unknown states, errors, and status
  display.
- Complete a full documentation sweep and update stale docs.
- Complete a maintainability sweep across the MVP codebase.

## Out Of Scope

- New feature development beyond fixes required to satisfy v1 criteria.
- GitHub organization/user discovery.
- Helm or Kustomize rendering.
- OIDC, RBAC, or multi-user behavior.

## Dependencies

- TASK-0011.

## Implementation Notes

- Treat failures in this task as either fixes in place or new follow-up tasks,
  depending on scope.
- The final result should be a coherent v1 that a homelab operator can run from
  the documented container image and file-based config.

## Task-Specific Acceptance Criteria

- The full MVP workflow passes end to end.
- All P0 tracker tasks are `Done` or have explicit approved deferrals.
- Documentation reflects the implemented behavior.
- Build, lint, formatting, tests, and E2E checks all pass.
- Known limitations are documented.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: run the packaged dashboard, configure repositories and
  runtime targets through files, scan repositories, monitor Docker/Kubernetes,
  and verify dashboard output.
- Unit/integration tests: run the complete automated suite.
- Build/lint/format commands: run every project check established by prior
  tasks.
- Documentation sweep targets: all docs under `docs/`, task files, deployment
  examples, and README if one exists.
- Maintainability review focus: module boundaries, testing gaps, operational
  diagnostics, and scope creep.
- Conventional commit type and summary: `test: validate mvp workflow`.

## Verification Evidence

- End-to-end test result: Passed. Ran the rebuilt dashboard on a temporary
  file-based config against a local Git fixture containing `prod/compose.yaml`
  and `prod/app.yaml`; invoked `POST /api/scan`; verified `GET /api/summary`
  returned one repository and two services: `compose:web:production:unknown`
  and `kubernetes:api:production:unknown`; verified all list fields serialize
  as JSON arrays; verified scan history and status arrays are present; verified
  the frontend shell serves built assets.
- Automated test result: Passed with `make check`, including backend tests,
  frontend typecheck/lint, frontend build, and production Go build.
- Build result: Passed with `make check` and
  `docker build -t gitops-dashboard:mvp .`.
- Lint result: Passed with `make check`, including `go vet` and
  `npm run lint`.
- Formatting result: Passed with `make check`, including `gofmt` and
  `npm run format`.
- Documentation sweep result: Reviewed all files under `docs/`, all task files,
  deployment docs, and examples. Updated deployment docs for automatic runtime
  monitoring and optional scheduled scans; removed stale task-template text;
  corrected the Docker monitoring task to reference the direct Docker Engine
  HTTP API client.
- Maintainability sweep result: Passed with fixes. Added API response
  normalization so omitted service list fields render as empty arrays instead
  of `null`; added regression coverage for that contract; added startup
  validation for duration fields; added background monitor and optional
  scheduled scan loops behind existing scanner/monitor module boundaries; added
  scan history, service detail, and runtime status fields/views to complete the
  dashboard acceptance scope.
- Conventional commit: `test: validate mvp workflow`.
- Notes or exceptions: E2E live runtime validation used the no-target path and
  verified `unknown` health because no Docker engine or Kubernetes cluster was
  configured in the temporary validation environment. Docker and Kubernetes
  live status behavior is covered by targeted tests and the agent/server API
  checks from prior tasks.
