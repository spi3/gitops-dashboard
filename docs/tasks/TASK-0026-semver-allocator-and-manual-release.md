# TASK-0026: SemVer allocator and manual release

## Scope

Provide the fail-closed SemVer allocation and guarded manual release tooling.
The release tool may create annotated release tags and its allocator lock ref
in this repository only.

## Out of Scope

- Dashboard, scanner, or monitor repository mutation.
- Release-workflow dispatch implementation.
- Operator invocation, stranded-lock recovery, and version-burn runbook
  material; these belong to T-028.

## Dependencies

- TASK-0015 CI versioning process.
- GitHub Actions CI workflow for the selected main commit.

## Acceptance Criteria

- Configuration inspection is fail-closed, including valueless entries, and
  fetches cannot prune tags or recurse into submodules.
- CI approval selects the newest traversed run and refetches that exact run.
- Local tags are created only by atomic compare-and-create and cleaned up only
  when owned by the release process.
- Publication is a create-only tag push bound to the reconciled tag object.

## E2E Evidence

The production entrypoint, `(*release).run`, is exercised by
`TestReleasePublishEndToEndWithBareRemote`. It creates a temporary local bare
remote and uses an in-process fake GitHub API (the test's `RoundTripper`; no
listener is required). It asserts these outcomes against that remote:

- a successful allocation creates an annotated tag object, whose direct remote
  ref is type `tag` and peels to `HEAD`, writes exactly `released <tag>`, and
  removes the lock;
- a tag collision introduced after allocation aborts without claiming it: its
  direct remote OID is unchanged and the owned local candidate tag is removed;
- a push whose response is ambiguous is reconciled only when the remote direct
  ref equals the captured local tag-object OID;
- cancellation after lock acquisition and an in-process closed release-output
  writer both return an error and release the lock; the latter also removes its
  owned local candidate tag; and
- each failure retains the pre-existing local `keep-local` tag and leaves no
  remote allocator lock.

## Verification

On 2026-07-10, the following commands were run from the repository root. The
output below is recorded as observed; `make check` is the top-level,
metadata-free-checkout regression.

```sh
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/sanitizer ./internal/ci/... ./cmd/... ./internal/...
# exit 0
# ok   github.com/example/gitops-dashboard/internal/sanitizer (cached)
# ok   github.com/example/gitops-dashboard/internal/ci (cached)
# ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard (cached)
# ok   github.com/example/gitops-dashboard/cmd/release (cached)
# ok   github.com/example/gitops-dashboard/cmd/version-allocator (cached)
# ok   github.com/example/gitops-dashboard/internal/agent (cached)
# ok   github.com/example/gitops-dashboard/internal/app (cached)
# ok   github.com/example/gitops-dashboard/internal/auth (cached)
# ok   github.com/example/gitops-dashboard/internal/config (cached)
# ok   github.com/example/gitops-dashboard/internal/core (cached)
# ok   github.com/example/gitops-dashboard/internal/dockerapi (cached)
# ok   github.com/example/gitops-dashboard/internal/environment (cached)
# ?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
# ok   github.com/example/gitops-dashboard/internal/monitor (cached)
# ok   github.com/example/gitops-dashboard/internal/parser (cached)
# ok   github.com/example/gitops-dashboard/internal/routetarget (cached)
# ok   github.com/example/gitops-dashboard/internal/scanner (cached)
# ok   github.com/example/gitops-dashboard/internal/storage (cached)
# ?    github.com/example/gitops-dashboard/internal/ui [no test files]
# ?    github.com/example/gitops-dashboard/internal/version [no test files]

make build
# exit 0
# vite v8.1.0 building client environment for production...
# ✓ 24 modules transformed.
# ✓ built in 260ms
# GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-a98dd6a27b35 -X github.com/example/gitops-dashboard/internal/version.Commit=a98dd6a27b35 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T08:37:44Z" ./cmd/gitops-dashboard

make check
# exit 0
# tail of output:
#   16 passed (17.3s)
# bash scripts/release_test.sh
# ok   github.com/example/gitops-dashboard/cmd/release (cached)
# release binary clean-clone invocation passed
# make[1]: Leaving directory '/tmp/gitops-dashboard-check.GwzyP4/checkout'

make release-test
# exit 0
# bash scripts/release_test.sh
# ok   github.com/example/gitops-dashboard/cmd/release (cached)
# release binary clean-clone invocation passed

git diff --check
# exit 0
# (no output)
```

`make release-test` runs the same `cmd/release` suite plus the release-wrapper
guard checks. The test uses `httptest`-style in-process HTTP transport only, so
it remains runnable in CI and local development even where a review sandbox
cannot bind a loopback listener.

Its fixture regression creates ignored `.env`, key, `data/` credential, and
build-artifact inputs in the source checkout, then asserts those paths and
`data/github_pat.txt` are absent from the fixture. `make check` creates a
NUL-delimited file manifest with `git ls-files --cached --others
--exclude-standard -z` before copying the isolated checkout, uses that manifest
as the rsync file list, and passes it to the fixture. The fixture validates that
the manifest exists and is non-empty before copying; direct `release-test`
falls back to creating the same manifest only when its root is a Git checkout.

## Documentation and Maintainability Sweep

Documentation review, with a verdict for every shared-criteria target:

- `docs/vision.md`: reviewed; no change. The release tooling does not alter the
  dashboard's read-only product direction.
- `docs/requirements.md`: updated to permit only this repository's release
  tooling and CI release workflow to create release tags and their lock refs.
- `docs/tech_stack.md`: reviewed; no change. Go, Git CLI, and shell wrapper
  usage follow the documented backend/tooling choices.
- `docs/implementation_plan.md`: reviewed; no change. This task has a task
  record and tracker entry consistent with the documented workflow.
- `docs/task_acceptance_criteria.md`: reviewed; no change. Its required sweep,
  maintainability, evidence, and tracker items are recorded here.
- `docs/tasks/TASK-0026-semver-allocator-and-manual-release.md`: updated with
  the release E2E evidence, this complete documentation/maintainability sweep,
  regression coverage, verification evidence, and proposed commit subject.
- `docs/tasks/TASK-0015-ci-versioning-process.md`: reviewed as the related
  versioning/release task; no change. Its CI tag-publication scope remains
  compatible with this guarded local release path.
- `docs/tasks/tracker.md`: updated to record TASK-0026 and reconcile the
  externally reserved task-number range.
- `docs/versioning.md`: reviewed as related release documentation; no change.
  It already defines immutable full-SemVer tags and the release-tag CI outcome.

Operator invocation, stranded-lock recovery, and version-burn runbook material
are explicitly deferred to external documentation task T-028.

Maintainability assessment: `internal/ci` keeps SemVer selection and CI-run
approval cohesive; `cmd/version-allocator` exposes allocator behavior without
duplicating release publication; `cmd/release` owns guarded lock, tag, and
create-only publication orchestration; `scripts/release.sh` remains a small
environment-cleaning, symlink-resolving wrapper; and `scripts/release_test.sh`
integrates a tracked/non-ignored-only fixture plus wrapper regressions. The
allocator/release E2E tests use explicit captured OIDs and a local bare remote,
keeping collision and ambiguous-push assertions readable and bounded. No
unrelated refactor was introduced.

Proposed Conventional Commit subject: `fix(check): preserve release fixture manifest (T-026)`.
