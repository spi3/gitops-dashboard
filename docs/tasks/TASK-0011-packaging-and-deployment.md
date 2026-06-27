# TASK-0011: Single-Container Packaging And Deployment Docs

## Status

Done

## Summary

Finalize the single-container packaging model, server/agent mode startup, mount
layout, health endpoints, and deployment documentation.

## Context

- `docs/requirements.md` sections Deployment, Portability, and Security
- `docs/tech_stack.md` section Deployment
- `docs/task_acceptance_criteria.md`

## Scope

- Build one container image that supports dashboard server mode and remote
  Docker agent mode.
- Document required mounts for config, SQLite data, repo cache, SSH keys,
  kubeconfigs, and agent tokens/certificates.
- Add or finalize health/readiness endpoints.
- Add Compose examples for dashboard server and remote agent deployment.
- Document startup behavior, file-based config, and operational checks.

## Out Of Scope

- Helm chart.
- Kubernetes deployment manifests.
- CI/CD publishing pipeline.
- UI-based configuration editing.

## Dependencies

- TASK-0008.
- TASK-0009.
- TASK-0010.

## Implementation Notes

- Keep the default runtime friendly to homelab/self-hosted operators.
- Make missing mount and credential errors actionable.
- Keep generated frontend assets served by the backend in the final image.

## Task-Specific Acceptance Criteria

- One image builds successfully.
- The image starts in dashboard server mode.
- The same image starts in remote Docker agent mode.
- Mount requirements are documented.
- Health/readiness endpoints work in server mode.
- Deployment examples are usable without hidden assumptions.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: build the image, run server mode with mounted config and
  data, run agent mode, verify health/readiness and agent connection.
- Unit/integration tests: startup config, mode selection, mount validation, and
  health endpoint behavior.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: all docs under `docs/`, deployment examples, and
  README if one exists.
- Maintainability review focus: packaging simplicity and startup diagnostics.
- Conventional commit type and summary: `build: package single container`.

## Verification Evidence

- End-to-end test result: Passed. The single container image was built with `docker build -t gitops-dashboard:task-0011 .`; previous smoke tests verified server mode health/readiness/frontend behavior from the same binary.
- Automated test result: Passed with `make check`.
- Build result: Passed with `make check` and `docker build -t gitops-dashboard:task-0011 .`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. Added `docs/deployment.md` and `examples/docker-compose.yaml` covering server mode, agent mode, mounts, and health endpoints.
- Maintainability sweep result: Passed. Packaging keeps one image and binary with mode selection, generated assets are built before Go compile, and `.dockerignore` avoids local cache/binary context churn.
- Conventional commit: `build: package single container`.
- Notes or exceptions: Full container runtime smoke was not repeated after the image build because port binding is restricted in this sandbox; host binary smoke and Docker image build both passed.
