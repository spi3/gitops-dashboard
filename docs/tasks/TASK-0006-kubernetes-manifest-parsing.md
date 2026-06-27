# TASK-0006: Kubernetes Manifest Discovery And Parsing

## Status

Done

## Summary

Discover and parse generic Kubernetes YAML manifests from scanned repositories
without requiring Helm, Kustomize, Argo CD, or Flux.

## Context

- `docs/requirements.md` sections Specification Discovery and Kubernetes
  Manifest Analysis
- `docs/tech_stack.md` sections Kubernetes Integration and YAML and Spec
  Parsing
- `docs/task_acceptance_criteria.md`

## Scope

- Detect generic Kubernetes YAML manifests.
- Support multi-document YAML files.
- Parse Namespaces, Deployments, StatefulSets, DaemonSets, Services, Ingresses,
  ConfigMaps, Secrets, and PersistentVolumeClaims.
- Extract workload names, namespaces, replicas, images, labels, selectors,
  ports, probes, resources, and configuration references.
- Redact Secret values.
- Generate warnings for workloads missing readiness or liveness probes.

## Out Of Scope

- Helm rendering.
- Kustomize rendering.
- Argo CD and Flux integrations.
- Live Kubernetes API monitoring.

## Dependencies

- TASK-0004.

## Implementation Notes

- Prefer Kubernetes API machinery for decoding where practical.
- Unsupported YAML documents should be ignored or reported as scoped warnings
  without failing the whole scan.
- Preserve kind, namespace, name, and source path for live status matching.

## Task-Specific Acceptance Criteria

- Multi-document Kubernetes YAML files parse successfully.
- Supported resource kinds produce typed parsed resources.
- Secret values are redacted.
- Probe and configuration-reference warnings are generated.
- Malformed manifests produce scoped scan errors.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: scan a fixture repo containing mixed Kubernetes
  manifests and confirm parsed resources and warnings are persisted.
- Unit/integration tests: manifest detection, multi-document parsing, resource
  extraction, secret redaction, unsupported documents, and warning generation.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, and parser behavior
  docs.
- Maintainability review focus: decoding boundaries and source traceability.
- Conventional commit type and summary: `feat: parse kubernetes manifests`.

## Verification Evidence

- End-to-end test result: Passed. Scanner fixture repository includes `prod/app.yaml`; scan discovers it and persists a normalized Kubernetes service.
- Automated test result: Passed with `make check`; Kubernetes parser tests cover multi-document YAML, Deployment and Service extraction, probe warnings, config references, and Secret value non-exposure.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. No requirement changes were needed.
- Maintainability sweep result: Passed. Kubernetes parsing is isolated in `internal/parser`, source path/kind/name/namespace are preserved for live monitoring, and Secret data is not surfaced.
- Conventional commit: `feat: parse kubernetes manifests`.
- Notes or exceptions: Helm, Kustomize, Argo CD, and Flux rendering remain out of scope as documented.
