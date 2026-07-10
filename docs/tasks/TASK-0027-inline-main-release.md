# TASK-0027: Inline auto-patch release on main

## Scope

Publish a serialized, immutable multi-architecture release inline for every
push to `main`. The same release queue accepts manual major/minor dispatches.
External strict-SemVer tag pushes retain their legacy SemVer-channel-only path.

GitHub Actions supports only one pending run for a concurrency group and may
replace it with a later pending run. Its workflow schema rejects `queue: max`;
the checked-in workflow documents this platform bound and preserves
`cancel-in-progress: false`. An unbounded durable release queue would require
an external broker.

## Out of Scope

- Stale-run guards for moving channels and release documentation improvements
  (T-028).
- Dashboard product behavior.

## E2E Plan

1. Run `make check` from a clean checkout.
2. On a `main` push, the release job waits for `test`, allocates/reuses an
   exact SemVer tag, and publishes one linux/amd64+linux/arm64 digest under the
   exact, minor, major, `latest`, and short-SHA tags.
3. Re-run the same commit: inspect the existing exact image's version and
   revision OCI labels; reuse its digest only when both match, otherwise fail.
4. Confirm the GitHub Release contains the digest, full `git rev-parse HEAD`
   commit, timestamp, and image references.

## Observed Results

No remote release was dispatched from this task checkout.

Hermetic `cmd/release` coverage drives `r.run(..., local=false, ...)` through
the checked-in workflow capability content, a bare Git remote, and a fake
GitHub API. It covers a successful returned run, delayed visibility correlated
by dispatch token, an ambiguous accepted POST reconciled without a second
dispatch, a failed `release` job conclusion with its run URL, an in-progress run
that times out with its run reference, and SIGINT-style cancellation during
observation with its run reference. Polling uses injected sleeps and the
timeout case uses an injected observation budget; no test adds wall-clock
sleeps. Remote dispatch is serialized by the workflow concurrency group and
does not acquire the local fallback's Git ref lock; cancellation verifies that
no local lock is left held.

## Verification

Observed on 2026-07-10 from the repository root:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/internal/ci (cached)
ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard (cached)
ok   github.com/example/gitops-dashboard/cmd/release (cached)
ok   github.com/example/gitops-dashboard/cmd/version-allocator 0.602s
ok   github.com/example/gitops-dashboard/internal/agent (cached)
ok   github.com/example/gitops-dashboard/internal/app 0.524s
ok   github.com/example/gitops-dashboard/internal/auth (cached)
ok   github.com/example/gitops-dashboard/internal/config (cached)
ok   github.com/example/gitops-dashboard/internal/core (cached)
ok   github.com/example/gitops-dashboard/internal/dockerapi (cached)
ok   github.com/example/gitops-dashboard/internal/environment (cached)
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/monitor 5.704s
ok   github.com/example/gitops-dashboard/internal/parser (cached)
ok   github.com/example/gitops-dashboard/internal/routetarget (cached)
ok   github.com/example/gitops-dashboard/internal/sanitizer (cached)
ok   github.com/example/gitops-dashboard/internal/scanner 0.991s
ok   github.com/example/gitops-dashboard/internal/storage (cached)
?    github.com/example/gitops-dashboard/internal/ui [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]

$ make build
npm run build
vite v8.1.0 building client environment for production...
✓ 24 modules transformed.
✓ built in 268ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-38140912da8b -X github.com/example/gitops-dashboard/internal/version.Commit=38140912da8b -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T17:24:04Z" ./cmd/gitops-dashboard

$ make check
make[1]: Entering directory '/tmp/gitops-dashboard-check.vbo92j/checkout'
npm run format
npm run build
vite v8.1.0 building client environment for production...
✓ 24 modules transformed.
✓ built in 269ms
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/cmd/release 2.498s
ok   github.com/example/gitops-dashboard/internal/ci 0.013s
ok   github.com/example/gitops-dashboard/internal/monitor 5.752s
npm test
> gitops-dashboard@1.0.0 test
> npm run typecheck && npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-unknown -X github.com/example/gitops-dashboard/internal/version.Commit=unknown -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T17:24:12Z" ./cmd/gitops-dashboard

$ actionlint .github/workflows/ci.yml
/bin/sh: 1: actionlint: not found

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

Round 2 verification observed on 2026-07-10 (after the fixes in this record):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/internal/ci (cached)
ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard (cached)
ok   github.com/example/gitops-dashboard/cmd/release (cached)
ok   github.com/example/gitops-dashboard/cmd/version-allocator 0.602s
ok   github.com/example/gitops-dashboard/internal/agent (cached)
ok   github.com/example/gitops-dashboard/internal/app 0.519s
ok   github.com/example/gitops-dashboard/internal/auth (cached)
ok   github.com/example/gitops-dashboard/internal/config (cached)
ok   github.com/example/gitops-dashboard/internal/core (cached)
ok   github.com/example/gitops-dashboard/internal/dockerapi (cached)
ok   github.com/example/gitops-dashboard/internal/environment (cached)
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/monitor 5.691s
ok   github.com/example/gitops-dashboard/internal/parser (cached)
ok   github.com/example/gitops-dashboard/internal/routetarget (cached)
ok   github.com/example/gitops-dashboard/internal/sanitizer (cached)
ok   github.com/example/gitops-dashboard/internal/scanner 1.034s
ok   github.com/example/gitops-dashboard/internal/storage (cached)
?    github.com/example/gitops-dashboard/internal/ui [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]

$ make build
npm run build
vite v8.1.0 building client environment for production...
✓ 24 modules transformed.
✓ built in 285ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false ... ./cmd/gitops-dashboard

$ make check
make[1]: Entering directory '/tmp/gitops-dashboard-check.esSOGg/checkout'
npm run format
npm run build
vite v8.1.0 building client environment for production...
✓ 24 modules transformed.
✓ built in 300ms
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard 0.016s
ok   github.com/example/gitops-dashboard/cmd/release 2.791s
ok   github.com/example/gitops-dashboard/cmd/version-allocator 0.802s
ok   github.com/example/gitops-dashboard/internal/ci 0.036s
ok   github.com/example/gitops-dashboard/internal/monitor 5.709s
npm test
> gitops-dashboard@1.0.0 test
> npm run typecheck && npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false ... ./cmd/gitops-dashboard

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

Round 3 verification observed on 2026-07-10 (after the display-name,
rate-limit, and exact generated-block fixes):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/internal/ci (cached)
ok   github.com/example/gitops-dashboard/cmd/release (cached)

$ make build
✓ built in 274ms

$ make check
make[1]: Entering directory '/tmp/gitops-dashboard-check.9PCITw/checkout'
ok   github.com/example/gitops-dashboard/cmd/release 4.082s
ok   github.com/example/gitops-dashboard/internal/ci 1.033s

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

Round 4 verification observed on 2026-07-10 (after the generated-release
image-reference format and rerun-idempotency fix):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/internal/ci 1.098s
ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard (cached)
ok   github.com/example/gitops-dashboard/cmd/release (cached)
ok   github.com/example/gitops-dashboard/cmd/version-allocator (cached)

$ make build
✓ built in 311ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-38140912da8b -X github.com/example/gitops-dashboard/internal/version.Commit=38140912da8b -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T18:43:38Z" ./cmd/gitops-dashboard

$ make check
make[1]: Entering directory '/tmp/gitops-dashboard-check.1A8MDF/checkout'
ok   github.com/example/gitops-dashboard/cmd/release 4.072s
ok   github.com/example/gitops-dashboard/internal/ci 1.312s

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

The dispatch observer derives the Actions job display name from the remote
workflow's `jobs.release.name`; the fake Actions API uses the same parsed
workflow content. Its success, failed, and skipped-job fixtures therefore use
the real `Release Container` display name. Only exhausted 403/429 responses
derive a rate-limit delay. The generated release-block helpers now emit and
validate the same bullet-line image-reference format, including `latest` and
short-SHA references for main releases. The positive fixture invokes the
extracted production generator, the independent missing/corrupt-field cases
run against that generated value, and a rerun of a self-generated block
follows the no-append branch.

## Documentation and Maintainability Sweep

- `docs/versioning.md`: reviewed; no change in this task. T-028 owns the
  corresponding channel/stale-run documentation updates.
- `docs/requirements.md`: reviewed; the existing authorization boundary for
  the CI release workflow remains applicable.
- `docs/tasks/TASK-0027-inline-main-release.md`: added as this task record.
- `docs/tasks/tracker.md`: updated with the task index and next-ID counter.

The workflow keeps the build to one exact-tag multi-architecture push; moving
channels are manifest aliases of that digest. `cmd/release` delegates to the
same workflow queue after a structural capability probe, and reconciliation by
dispatch token prevents an ambiguous accepted request from being duplicated.

Proposed Conventional Commit subject: `ci(release): publish patch releases inline on main (T-027)`.
