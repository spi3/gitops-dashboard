# TASK-0015: CI Versioning Process

## Status

Ready

## Summary

Implement the versioning process for the GitOps Dashboard container image and
service version inventory. The process should make GitHub Actions publish
repeatable image metadata, make release versions explicit, and let the
dashboard compare desired service versions from Git with observed versions from
runtime targets.

## Context

- `docs/versioning.md`
- `docs/requirements.md` sections Service Inventory, Docker Monitoring,
  Kubernetes Monitoring, and Deployment
- `docs/tasks/TASK-0011-packaging-and-deployment.md`
- `.github/workflows/ci.yml`

## Scope

- Extend GitHub Actions so pull requests build but do not publish the image.
- Keep `main` publishing `latest` and `sha-<short-sha>`.
- Add SemVer release publishing for `vMAJOR.MINOR.PATCH` tags.
- Add build metadata to the binary, API, image labels, and workflow summary.
- Normalize desired image references from Compose and Kubernetes manifests into
  version metadata.
- Collect observed image metadata from Docker and Kubernetes targets when
  available.
- Surface desired versus observed service version information in the dashboard.

## Out Of Scope

- Automatic deployment to any environment.
- Mutating downstream GitOps repositories from this repository's CI.
- Helm chart or Kubernetes deployment manifest generation.
- Enforcing pinned service image versions as scan failures.

## Dependencies

- `TASK-0011`

## Implementation Notes

- Keep the same single image for dashboard server mode and Docker agent mode.
- Preserve `latest` on `main` for compatibility with the existing workflow, but
  document that GitOps deployments should use SemVer tags and/or digests.
- Treat image parsing as structured data, not display-only string handling.
- Service image warnings should be additive and should not block repository
  scans.

## Task-Specific Acceptance Criteria

- PR CI runs checks and builds the image without pushing it.
- Main CI publishes `latest` and `sha-<short-sha>` image tags.
- Release tag CI publishes `vMAJOR.MINOR.PATCH`, `vMAJOR.MINOR`, and `vMAJOR`
  tags from a single image digest.
- The running app exposes version, commit SHA, and build timestamp.
- Published images include OCI labels for version, revision, source, and build
  timestamp.
- Service inventory exposes desired image tag/digest metadata from Git.
- Runtime monitoring exposes observed image tag/digest metadata when available.
- The dashboard distinguishes matching, mismatched, unknown, and mutable service
  image version states.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: create a release-tag workflow dry run or controlled test
  tag, verify GHCR tags resolve to one digest, run the image, and verify version
  output.
- Unit/integration tests: image reference parsing, version metadata API, Docker
  observed image metadata, Kubernetes observed image metadata, and service
  desired-versus-observed comparison.
- Build/lint/format commands: use `make check`.
- Documentation sweep targets: `README.md`, `docs/deployment.md`,
  `docs/versioning.md`, and related task files.
- Maintainability review focus: metadata source of truth, CI branch/tag
  conditions, and avoiding deployment-side mutation from product CI.
- Conventional commit type and summary: `build: add ci versioning process`.

## Verification Evidence

Fill this in before moving the task to `Done`.

- End-to-end test result:
- Automated test result:
- Build result:
- Lint result:
- Formatting result:
- Documentation sweep result:
- Maintainability sweep result:
- Conventional commit:
- Notes or exceptions:
