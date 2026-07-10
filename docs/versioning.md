# Continuous Versioning

GitOps Dashboard accepts every push to `main`, but it does not promise an
executed release job for every accepted revision. GitHub Actions serializes the
release group with one running and one pending job; a newer push replaces the
pending job. The pipeline produces immutable images and GitHub Releases;
downstream GitOps decides when and whether to deploy them.

If a delayed release observes that `origin/main` advanced beyond its checkout,
CI reconciles the displaced current head through the existing test-gated
`workflow_dispatch` release path. It waits for no queued/in-progress run for
that head, then dispatches one patch repair unless convergence is already
complete: the exact image and commit SHA image exist, `latest` points at that
release or a later one, and its GitHub Release exists. An allocated strict
SemVer tag alone never marks the head complete, because it is pushed before
all artifacts publish. The release job has `actions: write` only so this repair
can re-enter the workflow's normal test-gated front door.

A reconciliation-marked run may observe a strictly newer `main` head and
dispatch that newer head once. Thus reconciliation is an intentional bounded
per-head chain, not a blanket recursion: every link targets a descendant of
its source, no run re-dispatches its source head, and same-source/patch reuse
makes a repeated repair idempotent. Intermediate releases may still be
coalesced by the bounded queue, but the final current `main` head is
reintroduced and converges the release channels and `latest`.

## Versions and image tags

Product releases use strict SemVer with a `v` prefix: `vMAJOR.MINOR.PATCH`.
After tests pass, each executed `main` or manual-dispatch release job
serializes allocation and publishes one multi-platform digest under the
immutable exact tag, `sha-<short-sha>`, `vMAJOR.MINOR`, and `vMAJOR`. `latest`
is published only when the job's commit remains `origin/main` through its
registry write; the commit-specific SHA tag is published even for a stale
queued job.

Exact release tags and their image tags are immutable. Never delete, recreate,
or retarget them. `sha-*` is also commit-specific. The channels and `latest`
are convenience pointers, not deployment pins; deployments should use an exact
SemVer tag with its digest, or the digest alone.

CI recomputes every channel from all strict exact release tags and points it at
the highest compatible release digest. Thus a delayed release cannot move
`v1.4` back from `v1.4.3` to `v1.4.2`, and `v1` resolves to the highest
release in major line 1. `latest` is the newest successfully released `main`
commit. A delayed main run cannot move it backward: it rechecks `origin/main`
after publication and restores the prior pointer if main advanced during the
write. If no prior pointer is readable, CI advances `latest` to the advanced
main commit only when that commit already has an immutable release digest.
Otherwise it leaves `latest` at the just-published newest successful release;
the displaced-run reconciliation above restores a release path for the
advancing current-main commit and converges it forward when that release
completes. This brief state is semantically correct: a commit that never
releases successfully must not receive `latest`. CI never deletes registry
content. After a burst, the final executed current-main job converges `latest`;
its highest exact releases converge major/minor channels.

## Automatic patch releases and bootstrap

Every `main` push is an automatic patch-release event, subject to the bounded
queue above: an intermediate pending job may be replaced before execution and
therefore has no release artifact. Do not add CI skip markers such as `[skip
ci]`, `[ci skip]`, or equivalents. They bypass the test/release admission path
without the queue's explicit, observable replacement behavior and make it
unclear whether a revision was deliberately coalesced or silently skipped.

If no strict SemVer release tag exists, the allocator starts at `v0.0.1`. If an
operator first creates a manual major release, automatic pushes continue
patching that selected line. Non-strict tags are ignored. Re-running a release
for the same commit reuses its exact tag only when it already points at that
commit; a conflict fails closed.

## Manual major and minor releases

Only major and minor bumps are operator initiated. From a clean checkout at
the current `main` revision:

```sh
scripts/release.sh minor
scripts/release.sh major
```

The normal path verifies successful `main` CI, verifies `HEAD == origin/main`,
and dispatches the serialized CI release job. It needs `gh` authenticated to
GitHub with repository access and an SSH-capable Git remote for fetching.

`--local-fallback` is only for an unavailable CI dispatcher:

```sh
scripts/release.sh --local-fallback --burn-version minor
```

It is deliberately SSH-GitHub-only and requires `--burn-version`, because it
may irreversibly consume an exact version before CI publishes its image. It
uses a remote allocator lock and a create-only tag push; it never retargets an
existing version. Prefer the normal CI path.

## Recovery and GitHub Releases

Each exact release has a GitHub Release with digest, commit, timestamp, and
image references. If publication or release-note repair fails after a tag
exists, do not move or burn that tag: rerun CI for the same tag/commit so its
immutable-image and release-repair checks reconcile it. If fallback burns a
version without completing the release, preserve the tag, repair CI, and
complete that exact release; the next allocation advances instead.

Fallback removes its lock on success, error, and handled signals. After a hard
process death, inspect `refs/releases/locks/version-allocator`, confirm no
release job/operator is active, and record its remote OID. Only then may an
operator with the SSH remote delete that exact stale lock with a lease:

```sh
git push --no-follow-tags \
  --force-with-lease=refs/releases/locks/version-allocator=<observed-oid> \
  origin :refs/releases/locks/version-allocator
```

If the OID changes or deletion cannot be verified, stop: another allocator owns
the lock. Recommended GitHub policy is a `v*` tag ruleset that blocks deletion
and force updates; configuring it is outside this repository.

## Hotfixes and rollback

Fixes merge through `main` and receive the next patch release. Do not tag a
commit outside `main`: CI requires release commits to be ancestors of
`origin/main`. For urgent rollback, change downstream GitOps to a known-good
exact tag or digest; never retarget registry tags.

## Service inventory

The dashboard distinguishes source revision, product release, image digest,
desired image reference, and observed runtime image. Full SemVer tags and
digests are immutable; `latest`, `vMAJOR`, `vMAJOR.MINOR`, empty tags, and
unknown schemes are mutable warnings. Inventory remains read-only.
