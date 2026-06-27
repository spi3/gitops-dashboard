# TASK-0004: Repository Access And Scan Orchestration

## Status

Done

## Summary

Implement repository clone/fetch support and scan lifecycle orchestration for
GitHub PAT repositories, public generic Git URLs, and private SSH Git URLs.

## Context

- `docs/requirements.md` sections Repository Configuration and Repository
  Scanning
- `docs/tech_stack.md` section Repository Access
- `docs/task_acceptance_criteria.md`

## Scope

- Implement a narrow internal Git interface backed by the system `git` binary.
- Support explicit GitHub repository configuration with PAT authentication.
- Support public generic Git clone URLs.
- Support SSH Git URLs using configured private keys and known hosts.
- Maintain a managed repository cache.
- Record scan start, finish, commit SHA, errors, and status.
- Ensure one repository failure does not stop other scans.

## Out Of Scope

- GitHub organization/user discovery.
- Webhook-triggered scans.
- Compose or Kubernetes parsing.
- Runtime monitoring.

## Dependencies

- TASK-0002.
- TASK-0003.

## Implementation Notes

- Keep Git commands behind a small interface for testing.
- Avoid logging PATs, SSH key paths with sensitive content, or command output
  containing credentials.
- Time-bound clone and fetch operations.

## Task-Specific Acceptance Criteria

- Configured GitHub repos can be fetched with a PAT.
- Public generic Git repos can be fetched.
- Private SSH Git repos can be fetched with configured keys.
- Scan records include commit SHA and source ref.
- Per-repository scan errors are visible and isolated.
- Existing cached repositories are fetched rather than recloned when possible.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: configure fixture repositories for GitHub-style,
  public Git, and SSH Git access, run scans, and verify persisted scan state.
- Unit/integration tests: Git interface, auth setup, cache behavior, timeout
  handling, error isolation, and secret redaction.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, configuration
  examples, and operational docs.
- Maintainability review focus: Git abstraction, command safety, and scan state
  transitions.
- Conventional commit type and summary: `feat: add repository scanning`.

## Verification Evidence

- End-to-end test result: Passed. `internal/scanner` creates a real temporary Git repository, commits Compose and Kubernetes manifests, scans it through the configured repository flow, and verifies persisted repository commit and service inventory.
- Automated test result: Passed with `make check`; scanner tests cover local Git clone/fetch, token URL handling, SSH command environment setup, scan lifecycle, and parsed inventory persistence.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed.
- Maintainability sweep result: Passed. Git operations are behind scanner helpers, repository failures are scoped to scan records, and credential-sensitive paths avoid logging token contents.
- Conventional commit: `feat: add repository scanning`.
- Notes or exceptions: External GitHub PAT and SSH server E2E were not run against live services; token/SSH behavior is covered by deterministic tests and uses the same clone orchestration path.
