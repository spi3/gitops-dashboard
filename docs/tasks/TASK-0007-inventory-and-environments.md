# TASK-0007: Normalized Inventory And Environment Inference

## Status

Done

## Summary

Normalize Compose and Kubernetes analysis results into the shared service model
and infer environments from repository folder names.

## Context

- `docs/requirements.md` section Service Inventory
- `docs/tech_stack.md` modules `inventory` and `environment`
- `docs/task_acceptance_criteria.md`

## Scope

- Define the normalized service model.
- Merge Compose and Kubernetes parsed resources into service records.
- Infer environments from folder names in repository paths.
- Support aliases `prod`, `production`, `stage`, `staging`, `dev`,
  `development`, `test`, `testing`, `homelab`, `lab`, and `local`.
- Persist inventory snapshots and static analysis warnings.
- Mark health as `unknown` when no live signal is configured.

## Out Of Scope

- Live Docker status.
- Live Kubernetes status.
- UI rendering.
- User-authored environment overrides.

## Dependencies

- TASK-0005.
- TASK-0006.

## Implementation Notes

- Keep environment inference deterministic and testable.
- Preserve source repository, source commit, and source file path on every
  service record.
- Keep static analysis results separate from live monitoring results.

## Task-Specific Acceptance Criteria

- Compose and Kubernetes resources normalize into the shared service model.
- Folder-name environment inference handles all v1 aliases.
- Inferred environments are marked as inferred.
- Services without live status are marked `unknown`.
- Inventory snapshots are persisted with source commit and source path.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: scan fixture repos containing Compose and Kubernetes
  files in environment folders and verify the generated inventory.
- Unit/integration tests: service normalization, environment alias matching,
  unknown health status, source traceability, and persistence.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, and inventory model
  docs.
- Maintainability review focus: service model cohesion and inference logic.
- Conventional commit type and summary: `feat: build normalized inventory`.

## Verification Evidence

- End-to-end test result: Passed. Scanner fixture repository produces both Compose and Kubernetes service records from files under `prod/`, and the persisted inventory shows runtime type, source path, source commit, inferred environment, images, warnings, and `unknown` health.
- Automated test result: Passed with `make check`; environment tests cover all initial folder aliases and scanner/storage tests cover normalized inventory persistence.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed.
- Maintainability sweep result: Passed. Inventory assembly happens in scanner normalization helpers, environment inference is isolated in `internal/environment`, and live status remains separate from static inventory.
- Conventional commit: `feat: build normalized inventory`.
- Notes or exceptions: User-authored environment overrides remain out of scope for v1.
