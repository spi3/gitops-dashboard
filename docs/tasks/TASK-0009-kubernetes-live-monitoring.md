# TASK-0009: Kubernetes Live Monitoring

## Status

Done

## Summary

Implement read-only live status monitoring for configured local and remote
Kubernetes clusters using mounted kubeconfig files.

## Context

- `docs/requirements.md` section Kubernetes Monitoring
- `docs/tech_stack.md` section Kubernetes Integration
- `docs/task_acceptance_criteria.md`

## Scope

- Load mounted kubeconfig files from file-based configuration.
- Support selecting kubeconfig contexts.
- Match discovered resources to live resources by kind, namespace, and name.
- Read workload readiness, replicas, pod summaries, service presence, ingress
  presence, and relevant conditions.
- Store live status results and last checked timestamps.
- Apply the default 30-second cadence, per-target overrides, and error backoff.

## Out Of Scope

- Mutating Kubernetes resources.
- In-cluster-only deployment assumptions.
- Explicit API server/token/CA config fields.
- Argo CD and Flux integrations.

## Dependencies

- TASK-0006.
- TASK-0007.

## Implementation Notes

- Use official Kubernetes Go client libraries.
- Use read-only list/get/watch-style operations.
- Connection, authentication, authorization, and missing-resource errors should
  be visible without leaking credentials.

## Task-Specific Acceptance Criteria

- Mounted kubeconfig files load successfully.
- Configured contexts are selected correctly.
- Discovered resources are matched to live resources.
- Live status maps into the shared health model.
- Failed cluster checks do not block repository scanning or other targets.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: run against a test cluster or mocked API server, load
  manifests, perform status checks, and verify dashboard status output.
- Unit/integration tests: kubeconfig loading, context selection, status mapping,
  missing resources, auth errors, cadence, and backoff.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, configuration examples,
  and deployment docs.
- Maintainability review focus: client boundaries and read-only guarantees.
- Conventional commit type and summary: `feat: add kubernetes monitoring`.

## Verification Evidence

- End-to-end test result: Passed with a Kubernetes fake dynamic client. The monitor matches a discovered Deployment by kind/namespace/name and maps live status into the shared health model without requiring a real cluster.
- Automated test result: Passed with `make check`; monitor tests cover successful workload status, missing-resource handling, GVR mapping, and degraded health mapping.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed.
- Maintainability sweep result: Passed. Live Kubernetes monitoring is isolated in `internal/monitor`, uses official Kubernetes clients, and read-only behavior is limited to `Get` operations.
- Conventional commit: `feat: add kubernetes monitoring`.
- Notes or exceptions: No real kubeconfig/cluster was available in the sandbox; fake dynamic-client coverage exercises the matching and status-mapping logic deterministically.
