# TASK-0005: Docker Compose Discovery And Parsing

## Status

Done

## Summary

Discover Docker Compose files in scanned repositories and extract service facts
needed for the normalized inventory and dashboard.

## Context

- `docs/requirements.md` sections Specification Discovery and Docker Compose
  Analysis
- `docs/tech_stack.md` sections YAML and Spec Parsing and Testing Strategy
- `docs/task_acceptance_criteria.md`

## Scope

- Detect `compose.yaml`, `compose.yml`, `docker-compose.yaml`, and
  `docker-compose.yml`.
- Parse Compose services from YAML.
- Extract service names, images, build contexts, ports, volumes, networks,
  healthchecks, restart policies, and dependencies.
- Record environment variable names without values.
- Produce static analysis warnings such as missing healthchecks.
- Track source file paths and parse errors.

## Out Of Scope

- Docker runtime monitoring.
- Compose command execution.
- Secret value inspection.
- Full Compose project simulation.

## Dependencies

- TASK-0004.

## Implementation Notes

- Use structured YAML parsing.
- Preserve enough source context to explain parser warnings.
- Treat malformed Compose files as file/repository scan errors without failing
  unrelated repositories.

## Task-Specific Acceptance Criteria

- Supported Compose filenames are detected.
- Services and core service fields are parsed into typed results.
- Environment values are not stored or displayed.
- Missing-healthcheck warnings are generated.
- Malformed Compose files produce scoped scan errors.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: scan a fixture repo with multiple Compose files and
  confirm parsed service facts and warnings are persisted.
- Unit/integration tests: filename detection, YAML parsing, service extraction,
  warning generation, and secret redaction.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, and parser behavior
  docs.
- Maintainability review focus: typed parser structure and source-path
  traceability.
- Conventional commit type and summary: `feat: parse docker compose specs`.

## Verification Evidence

- End-to-end test result: Passed. Scanner fixture repository includes `prod/compose.yaml`; scan discovers it and persists a normalized Compose service.
- Automated test result: Passed with `make check`; Compose parser tests cover supported filename parsing, service extraction, dependency extraction, missing healthcheck warnings, and environment variable name redaction.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed.
- Maintainability sweep result: Passed. Compose parsing is isolated in `internal/parser`, source paths are preserved by scanner, and config references store names without values.
- Conventional commit: `feat: parse docker compose specs`.
- Notes or exceptions: Compose runtime behavior is handled by TASK-0010.
