# TASK-0002: File-Based Configuration And Basic Auth

## Status

Done

## Summary

Implement file-based configuration for repositories, runtime targets, storage,
auth, and server settings. Protect the dashboard and API with basic
authentication while preserving an explicit development-mode no-auth escape
hatch.

## Context

- `docs/requirements.md` sections Repository Configuration, Runtime Target
  Configuration, Authentication, and Security
- `docs/tech_stack.md` sections Backend, Authentication, and Deployment
- `docs/task_acceptance_criteria.md`

## Scope

- Define the v1 configuration file format.
- Load configuration from mounted files with environment overrides only where
  appropriate for deployment.
- Validate repository, runtime target, credential path, auth, and cadence
  settings.
- Implement basic auth middleware for dashboard and API routes.
- Require either enabled basic auth or explicit development-mode no-auth.
- Redact secrets from logs and API responses.

## Out Of Scope

- UI-based configuration editing.
- OIDC, SSO, and RBAC.
- Repository cloning or runtime monitoring behavior.

## Dependencies

- TASK-0001.

## Implementation Notes

- Store password hashes, not plaintext passwords.
- Keep configuration parsing independent from HTTP handlers.
- Ensure validation errors identify the setting without leaking secret values.

## Task-Specific Acceptance Criteria

- Valid config files load successfully.
- Invalid config files fail with actionable errors.
- Basic auth protects dashboard and API routes.
- Explicit development-mode no-auth is required to disable auth.
- Secret values and credential file contents are not logged or returned.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: start the server with basic auth enabled, verify
  unauthenticated requests fail, authenticated requests succeed, and no-auth
  mode only works when explicitly configured.
- Unit/integration tests: config parsing, validation, redaction, auth
  middleware, and password hash verification.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, deployment notes, and
  configuration examples.
- Maintainability review focus: config schema boundaries and secret handling.
- Conventional commit type and summary: `feat: add file config and basic auth`.

## Verification Evidence

- End-to-end test result: Passed. Started `./gitops-dashboard -config examples/config.basic.yaml` on `:18081`; verified unauthenticated `GET /api/summary` returns 401, authenticated `GET /api/summary` with `admin:password` returns 200, and `GET /healthz` remains public.
- Automated test result: Passed with `make check`; auth middleware and config loading have focused Go tests.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. Added `examples/config.basic.yaml` to document file-based basic auth configuration with a bcrypt password hash.
- Maintainability sweep result: Passed. Config loading, auth middleware, and password verification are isolated from HTTP handlers; secret values are not logged or returned.
- Conventional commit: `feat: add file config and basic auth`.
- Notes or exceptions: Normal `.git` initialization is blocked by a read-only workspace mount, so commits use `git --git-dir=.gitdb --work-tree=.`.
