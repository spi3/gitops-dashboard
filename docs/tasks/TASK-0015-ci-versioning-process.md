# TASK-0015: CI Versioning Process

## Status

Done

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

- End-to-end test result: `npm run test:e2e -- tests/ui/dashboard.spec.ts`
  passed on 2026-07-08 with 11 Chromium tests passing. `make check` also
  passed with all 13 Playwright tests passing.
- Automated test result:
  `GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/...`
  passed; `make check` also ran `go test ./cmd/... ./internal/...`.
- Build result: `make build` passed, including Vite production build and Go
  binary build with ldflags. `./gitops-dashboard -version` reported
  `gitops-dashboard dev-381872caca7b (commit 381872caca7b, built 2026-07-09T01:12:07Z)`.
- Lint result: `npm test` passed TypeScript typecheck and ESLint; `make check`
  passed `go vet ./cmd/... ./internal/...`.
- Formatting result: `make check` passed `gofmt -w cmd internal` and
  `npm run format`.
- Documentation sweep result: reviewed `README.md`, `docs/deployment.md`,
  `docs/versioning.md`, `docs/requirements.md`, `docs/tech_stack.md`,
  `docs/implementation_plan.md`, `docs/task_acceptance_criteria.md`, this task
  file, and `docs/tasks/tracker.md`; updated the docs that changed behavior or
  operator-facing usage.
- Maintainability sweep result: build metadata is centralized in
  `internal/version`; image parsing and comparison logic is centralized in
  `internal/core`; scanner, monitor, storage, API, and UI changes stay within
  existing module boundaries.
- Conventional commit: `feat(release): add ci versioning process (T-001)`
- Notes or exceptions: No release tag was pushed from this sandbox. The workflow
  now defines the SemVer release path, release-tag ancestry check, OCI labels,
  disables automatic `latest` tagging, and writes a workflow summary; live GHCR
  publication will be exercised when a reviewed `vMAJOR.MINOR.PATCH` tag is
  pushed. Round-1 reviewer fixes covered release-tag `latest` behavior,
  registry-aware image matching, Kubernetes status-only observed images, and
  exact desired-to-observed image pairing. Round-2 reviewer fixes moved
  Kubernetes observations to matching Pod status data and changed image
  comparison to exact-match, repository-match, then unknown phases with no
  cross-repository fallback. Round-3 reviewer fixes preserved workload health
  when optional Pod image lookup fails and deduplicated desired image references
  before comparison. Round-4 reviewer fixes exclude not-applicable targets from
  image version comparisons, clear override-row observed images, and inspect
  Docker image metadata so digest-pinned desired images can match observed
  repo digests from both direct Docker checks and agent reports. Round-5
  reviewer fixes display observed repo digests in the drawer and include the
  UTC build timestamp in the footer build label. Round-6 reviewer fixes treat
  moving release-channel tags and unknown non-SemVer tags as mutable while
  keeping full SemVer tags and digest-pinned references immutable. Round-7
  reviewer fixes derive workflow commit metadata from the checked-out commit
  instead of raw `GITHUB_SHA`, preserving source traceability for annotated
  release tags. Round-8 reviewer fixes limit Docker image observations to live
  containers so stopped stale Compose containers do not create false image
  drift. Round-9 reviewer fixes limit Kubernetes image observations to live Pods
  so deleting or terminal stale Pods do not create false image drift.
