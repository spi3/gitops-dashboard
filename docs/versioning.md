# Versioning Process

## Purpose

This document defines how GitOps Dashboard versions its own container image and
how it should track versions of the services it discovers. The process keeps
GitHub Actions responsible for repeatable build metadata and image publication,
while keeping GitOps deployment repositories responsible for choosing which
version is deployed.

## Version Types

- Source revision: the Git commit SHA that produced a build or service scan.
- Product release version: a SemVer tag in the form `vMAJOR.MINOR.PATCH`.
- Container image version: the GHCR image tag and immutable digest produced by
  CI.
- Desired service version: the image references declared in scanned Compose and
  Kubernetes manifests.
- Observed service version: the image reference, image ID, or digest reported by
  a live Docker or Kubernetes target.

The source revision and image digest are the immutable identifiers. Human
readable tags are convenience handles for operators and automation.

## Tag Policy

GitHub Actions should publish these tags:

| Trigger | Tags | Purpose |
| --- | --- | --- |
| Pull request | None | Validate the code and image build without publishing. |
| Push to `main` | `latest`, `sha-<short-sha>` | Provide a fast-moving integration image and an immutable commit image. |
| Push tag `vMAJOR.MINOR.PATCH` | `vMAJOR.MINOR.PATCH`, `vMAJOR.MINOR`, `vMAJOR` | Publish a stable release line for GitOps deployment updates. |

Rules:

- Release tags are immutable. Never delete and recreate a release tag after it
  has published an image.
- GitOps deployments should pin either an image digest or a SemVer tag plus
  digest. They should not pin `latest`.
- `latest` is only an integration channel for local testing and compatibility
  with the current workflow.
- `sha-<short-sha>` is the best tag for reproducing a specific `main` build.
- Major and minor moving tags are convenience release channels. The exact image
  digest in the release notes remains the audit target.

## CI Responsibilities

### Pull Requests

PR CI should:

- Run the full project check target.
- Build the container image without pushing it.
- Verify the image can report its build metadata.
- Fail if version metadata is missing or malformed.

PRs must not publish packages. This keeps unreviewed code out of GHCR and makes
the package registry reflect accepted repository state only.

### Main Branch

Pushes to `main` should:

- Run the same checks as PR CI.
- Publish the image to `ghcr.io/spi3/gitops-dashboard`.
- Tag the image as `latest` and `sha-<short-sha>`.
- Add OCI labels for source repository, commit revision, build timestamp, and
  version.
- Emit the pushed image digest in the workflow summary.

The current workflow already publishes `latest` and short SHA tags from `main`.
The next improvement is to make the metadata visible from the running binary.

### Release Tags

Pushing a SemVer tag such as `v1.4.2` should:

- Confirm the tag points at a commit reachable from `main`.
- Run the full project checks.
- Build and publish the image once.
- Publish SemVer tags from the same digest:
  - `v1.4.2`
  - `v1.4`
  - `v1`
- Create or update a GitHub Release with the image digest, commit SHA, and
  upgrade notes.

Release CI should not mutate deployment repositories directly. Downstream
GitOps repositories should consume the new version through Renovate or a
reviewed pull request.

## Build Metadata

The binary and image should expose the same build metadata:

- Version: SemVer tag for release builds, otherwise a development version based
  on the commit SHA.
- Commit SHA.
- Build timestamp.
- Image digest when available after publication.

Recommended surfaces:

- `gitops-dashboard -version`
- `GET /api/version`
- Dashboard footer and per-service image version detail.
- OCI labels on the container image.
- GitHub Actions workflow summary.

This metadata lets an operator compare what is deployed, what was built, and
what Git commit produced it without inspecting the registry manually.

## Service Version Inventory

For discovered services, the dashboard should treat Git as the source of the
desired version and runtime targets as the source of the observed version.

Desired service version should come from scanned manifests:

- Compose `services[].image`
- Kubernetes workload container and init-container images
- The scan commit SHA and source path that declared the image

The scanner should normalize each image reference into:

- Registry
- Repository
- Tag, when present
- Digest, when present
- Original image string

Observed service version should come from live targets:

- Docker image reference, image ID, and repo digest when available.
- Kubernetes container image, container image ID, and pod status image ID when
  available.

Image observations should come only from live runtime objects. Docker excludes
stopped containers. Kubernetes excludes deleting Pods and terminal
`Succeeded`/`Failed` Pods; it includes `Running` Pods and `Pending` Pods only
when Pod status already reports container image metadata.

The dashboard can then show:

- Desired version from Git.
- Observed version from the runtime.
- Whether desired and observed appear to match.
- Whether the service is using a mutable tag such as `latest`.
- The source commit that last declared the desired version.

Mutable or unpinned service image references should be warnings, not hard
failures. The dashboard is read-only and should surface the risk without
blocking scans.

Digest-pinned references are immutable. Full SemVer tags such as `v1.2.3` are
treated as immutable release references, while partial SemVer channel tags such
as `v1` and `v1.2`, empty tags, `latest`, and unknown non-SemVer tag schemes
are treated as mutable because they can move without a manifest change.

## GitOps Update Flow

The release workflow publishes images; it does not deploy them.

The deployment update flow should be:

1. GitHub Actions publishes a release image and digest.
2. Renovate or a human opens a pull request in the GitOps deployment repo.
3. The pull request updates image tags or digests in Compose or Kubernetes
   manifests.
4. CI in the deployment repo validates the manifest change.
5. The GitOps controller or deployment process applies the merged change.
6. GitOps Dashboard scans the repository and reports the new desired version.
7. Runtime monitoring reports the observed version after rollout.

This keeps the product build pipeline separate from environment rollout policy.

## Rollback And Hotfixes

Rollbacks should be GitOps changes, not registry mutation:

- Revert the deployment repository to a previously known good tag or digest.
- Keep the bad release tag in GHCR for auditability.
- Publish a new patch release for fixes, for example `v1.4.3` after `v1.4.2`.

Hotfixes should branch from the relevant release commit when needed, publish a
new patch tag, then merge the fix back to `main`.

## Implementation Sketch

The existing workflow can evolve into three jobs:

- `test`: run `make check`.
- `build`: build the image on PRs without pushing.
- `publish`: push GHCR tags on `main` and `v*` tag events.

The metadata action tag configuration should include the existing `latest` and
short SHA tags for `main`, plus SemVer tags for release events:

```yaml
tags: |
  type=raw,value=latest,enable=${{ github.ref == 'refs/heads/main' }}
  type=sha,format=short
  type=semver,pattern=v{{version}}
  type=semver,pattern=v{{major}}.{{minor}}
  type=semver,pattern=v{{major}}
```

The `v` prefix is intentional because the metadata action's SemVer variables
are the numeric version components.

When release deployment policy is mature, the project can decide whether
`latest` should continue following `main` or move to the latest stable release.
Until then, production GitOps pins should avoid `latest`.
