# TASK-0047: CI publish wedge hotfix

## Status

In Review

## Summary

Prevent the UI dependency-install stage from running under target-platform QEMU
emulation, bound CI job duration, and make the workflow guard verify exact
Docker variable references.

## Context

Incident date: 2026-07-10. Two publish runs wedged while executing the UI
stage's `npm ci` for `linux/arm64` under QEMU: each first reported `SIGILL` and
then remained in an indefinite BuildKit hang. Their observed durations were 70
minutes and 5 hours 21 minutes. Incident forensics ruled out the source commit,
runner, and registry as the cause.

## Scope

- Pin only the Dockerfile `ui` stage to native `$BUILDPLATFORM`.
- Set workflow job timeouts to 15 minutes for `test` and 45 minutes each for
  `build` and `publish`.
- Harden workflow tests to identify Docker variable references lexically:
  `$name` and `${name...}` are compared by exact variable identity.
- Record the incident, verification evidence, and the operational E2E boundary.

## Out Of Scope

- Per-platform build cache changes.
- Splitting builds by platform.
- Pull-request arm64 smoke coverage; this is external T-042.

## Dependencies

- GitHub Actions CI workflow and Docker Buildx execution environment.
- Landing the change so GitHub Actions can exercise both published platforms.

## Task-Specific Acceptance Criteria

- The `ui` stage uses exactly `$BUILDPLATFORM` for its platform expression.
- No non-UI Docker stage has a platform expression that references
  `BUILDPLATFORM`, including braced forms and parameter modifiers.
- A UI expression containing `$BUILDPLATFORM_OVERRIDE` fails the guard.
- A UI expression `${TARGETPLATFORM:-$BUILDPLATFORM}` fails the guard.
- A non-UI expression `${BUILDPLATFORM:-$TARGETPLATFORM}` fails the guard.
- CI has job-level `timeout-minutes` values of 15, 45, and 45 for `test`,
  `build`, and `publish`, respectively.

## E2E Plan and Limitation

The workflow tests validate Dockerfile-stage and workflow configuration locally.
The landing runs are the E2E test: GitHub Actions will run CI on both target
platforms after this change lands. This sandbox cannot run multi-architecture
Docker builds, so it cannot perform that platform-level E2E path locally.

## Verification Evidence

On 2026-07-10, the following commands were run from the repository root. The
output below is recorded as observed.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./internal/...
ok  \tgithub.com/example/gitops-dashboard/internal/ci\t0.010s
ok  \tgithub.com/example/gitops-dashboard/internal/agent\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/app\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/auth\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/config\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/core\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/dockerapi\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/environment\t(cached)
?   \tgithub.com/example/gitops-dashboard/internal/hostinventory\t[no test files]
ok  \tgithub.com/example/gitops-dashboard/internal/monitor\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/parser\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/routetarget\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/sanitizer\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/scanner\t(cached)
ok  \tgithub.com/example/gitops-dashboard/internal/storage\t(cached)
?   \tgithub.com/example/gitops-dashboard/internal/ui\t[no test files]
?   \tgithub.com/example/gitops-dashboard/internal/version\t[no test files]
```

```text
$ make build
npm run build

> gitops-dashboard@1.0.0 build
> vite build

(node:20) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
vite v8.1.0 building client environment for production...
✓ 24 modules transformed.
✓ built in 263ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-712edacc91d3 -X github.com/example/gitops-dashboard/internal/version.Commit=712edacc91d3 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T16:21:04Z" ./cmd/gitops-dashboard
```

```text
$ make check
make[1]: Entering directory '/tmp/gitops-dashboard-check.mbGFK5/checkout'
npm run format
npm run build
vite v8.1.0 building client environment for production...
✓ 24 modules transformed.
✓ built in 264ms
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
ok  \tgithub.com/example/gitops-dashboard/cmd/gitops-dashboard\t0.015s
ok  \tgithub.com/example/gitops-dashboard/cmd/release\t2.341s
ok  \tgithub.com/example/gitops-dashboard/cmd/version-allocator\t0.657s
ok  \tgithub.com/example/gitops-dashboard/internal/agent\t0.012s
ok  \tgithub.com/example/gitops-dashboard/internal/app\t0.606s
ok  \tgithub.com/example/gitops-dashboard/internal/auth\t0.012s
ok  \tgithub.com/example/gitops-dashboard/internal/ci\t0.014s
ok  \tgithub.com/example/gitops-dashboard/internal/config\t0.017s
ok  \tgithub.com/example/gitops-dashboard/internal/core\t0.007s
ok  \tgithub.com/example/gitops-dashboard/internal/dockerapi\t0.010s
ok  \tgithub.com/example/gitops-dashboard/internal/environment\t0.005s
?   \tgithub.com/example/gitops-dashboard/internal/hostinventory\t[no test files]
ok  \tgithub.com/example/gitops-dashboard/internal/monitor\t5.671s
ok  \tgithub.com/example/gitops-dashboard/internal/parser\t0.018s
ok  \tgithub.com/example/gitops-dashboard/internal/routetarget\t0.016s
ok  \tgithub.com/example/gitops-dashboard/internal/sanitizer\t0.007s
ok  \tgithub.com/example/gitops-dashboard/internal/scanner\t1.087s
ok  \tgithub.com/example/gitops-dashboard/internal/storage\t0.366s
?   \tgithub.com/example/gitops-dashboard/internal/ui\t[no test files]
?   \tgithub.com/example/gitops-dashboard/internal/version\t[no test files]
npm test
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-unknown -X github.com/example/gitops-dashboard/internal/version.Commit=unknown -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T16:21:14Z" ./cmd/gitops-dashboard
```

The historical `make check` excerpt above is not a complete transcript and is
superseded by the round-4 evidence below. In particular, it did not include
the Playwright or release-test outcomes; it must not be used as evidence that
either target ran.

```text
$ git diff --check
```

`git diff --check` exited 0 and produced no output.

### Round 4 verification (2026-07-10)

The following commands were run from the repository root after the UI guard
was changed to require the entire normalized platform expression to be either
`$BUILDPLATFORM` or `${BUILDPLATFORM}`. Each command exited 0.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./internal/...
ok  	github.com/example/gitops-dashboard/internal/ci	0.028s
ok  	github.com/example/gitops-dashboard/internal/agent	(cached)
ok  	github.com/example/gitops-dashboard/internal/app	(cached)
ok  	github.com/example/gitops-dashboard/internal/auth	(cached)
ok  	github.com/example/gitops-dashboard/internal/config	(cached)
ok  	github.com/example/gitops-dashboard/internal/core	(cached)
ok  	github.com/example/gitops-dashboard/internal/dockerapi	(cached)
ok  	github.com/example/gitops-dashboard/internal/environment	(cached)
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	(cached)
ok  	github.com/example/gitops-dashboard/internal/parser	(cached)
ok  	github.com/example/gitops-dashboard/internal/routetarget	(cached)
ok  	github.com/example/gitops-dashboard/internal/sanitizer	(cached)
ok  	github.com/example/gitops-dashboard/internal/scanner	(cached)
ok  	github.com/example/gitops-dashboard/internal/storage	(cached)
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]

$ make build
✓ built in 287ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-712edacc91d3 -X github.com/example/gitops-dashboard/internal/version.Commit=712edacc91d3 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T16:33:02Z" ./cmd/gitops-dashboard

$ make check
make[1]: Entering directory '/tmp/gitops-dashboard-check.IFVAq5/checkout'
npm run format

> gitops-dashboard@1.0.0 format
> node scripts/check-format.mjs

npm run build

> gitops-dashboard@1.0.0 build
> vite build

(node:69) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
(Use `node --trace-warnings ...` to show where the warning was created)
vite v8.1.0 building client environment for production...
transforming...✓ 24 modules transformed.
rendering chunks...
computing gzip size...
internal/ui/dist/index.html                                                          0.40 kB │ gzip:  0.27 kB
internal/ui/dist/assets/jetbrains-mono-vietnamese-wght-normal-Bt-aOZkq.woff2         7.50 kB
internal/ui/dist/assets/bricolage-grotesque-vietnamese-wght-normal-BUzh504Q.woff2    8.60 kB
internal/ui/dist/assets/jetbrains-mono-greek-wght-normal-Bw9x6K1M.woff2              9.00 kB
internal/ui/dist/assets/jetbrains-mono-cyrillic-wght-normal-D73BlboJ.woff2          12.10 kB
internal/ui/dist/assets/jetbrains-mono-latin-ext-wght-normal-DBQx-q_a.woff2         15.19 kB
internal/ui/dist/assets/bricolage-grotesque-latin-ext-wght-normal-CcLUaPy7.woff2    18.66 kB
internal/ui/dist/assets/jetbrains-mono-latin-wght-normal-B9CIFXIH.woff2             40.40 kB
internal/ui/dist/assets/bricolage-grotesque-latin-wght-normal-DLoelf7F.woff2        41.34 kB
internal/ui/dist/assets/index-4UJhDLCM.css                                          21.47 kB │ gzip:  6.66 kB
internal/ui/dist/assets/index-CK07UTUG.js                                          220.11 kB │ gzip: 68.54 kB

✓ built in 340ms
npm run lint

> gitops-dashboard@1.0.0 lint
> eslint web --ext .ts,.tsx

(node:106) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
(Use `node --trace-warnings ...` to show where the warning was created)
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
ok  	github.com/example/gitops-dashboard/cmd/gitops-dashboard	0.010s
ok  	github.com/example/gitops-dashboard/cmd/release	2.976s
ok  	github.com/example/gitops-dashboard/cmd/version-allocator	0.789s
ok  	github.com/example/gitops-dashboard/internal/agent	0.016s
ok  	github.com/example/gitops-dashboard/internal/app	0.650s
ok  	github.com/example/gitops-dashboard/internal/auth	0.010s
ok  	github.com/example/gitops-dashboard/internal/ci	0.010s
ok  	github.com/example/gitops-dashboard/internal/config	0.026s
ok  	github.com/example/gitops-dashboard/internal/core	0.015s
ok  	github.com/example/gitops-dashboard/internal/dockerapi	0.013s
ok  	github.com/example/gitops-dashboard/internal/environment	0.008s
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	5.694s
ok  	github.com/example/gitops-dashboard/internal/parser	0.027s
ok  	github.com/example/gitops-dashboard/internal/routetarget	0.017s
ok  	github.com/example/gitops-dashboard/internal/sanitizer	0.015s
ok  	github.com/example/gitops-dashboard/internal/scanner	1.146s
ok  	github.com/example/gitops-dashboard/internal/storage	0.411s
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]
npm test

> gitops-dashboard@1.0.0 test
> npm run typecheck && npm run lint


> gitops-dashboard@1.0.0 typecheck
> tsc --noEmit


> gitops-dashboard@1.0.0 lint
> eslint web --ext .ts,.tsx

(node:2058) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
(Use `node --trace-warnings ...` to show where the warning was created)
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-unknown -X github.com/example/gitops-dashboard/internal/version.Commit=unknown -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T16:42:47Z" ./cmd/gitops-dashboard
npm run test:e2e

> gitops-dashboard@1.0.0 test:e2e
> playwright test

(node:2172) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
(Use `node --trace-warnings ...` to show where the warning was created)

Running 16 tests using 2 workers

(node:2183) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
(Use `node --trace-warnings ...` to show where the warning was created)
(node:2184) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
(Use `node --trace-warnings ...` to show where the warning was created)
  ✓   1 [chromium] › tests/ui/agents.spec.ts:182:1 › agents tab shows connected and never-connected agents without losing the services tab (2.3s)
  ✓   3 [chromium] › tests/ui/agents.spec.ts:261:1 › the #/agents deep link opens directly on the agents tab from a fresh page load (282ms)
  ✓   2 [chromium] › tests/ui/dashboard.spec.ts:83:1 › verifies the full dashboard workflow against the real server (5.3s)
  ✓   4 [chromium] › tests/ui/dashboard.spec.ts:183:1 › renders every supported health state in the browser (422ms)
  ✓   5 [chromium] › tests/ui/dashboard.spec.ts:210:1 › renders image version states in tiles and details (461ms)
  ✓   6 [chromium] › tests/ui/dashboard.spec.ts:241:1 › renders uptime history and drawer details from the summary (684ms)
  ✓   7 [chromium] › tests/ui/dashboard.spec.ts:266:1 › aggregates multi-target uptime on service tiles (350ms)
  ✓   8 [chromium] › tests/ui/dashboard.spec.ts:284:1 › can mark individual route monitor targets not applicable from the drawer (841ms)
  ✓   9 [chromium] › tests/ui/dashboard.spec.ts:364:1 › keeps all routes controls after re-enabling the all routes monitor (584ms)
  ✓  10 [chromium] › tests/ui/dashboard.spec.ts:409:1 › keeps a literal routes monitor target out of the route override UI (668ms)
  ✓  11 [chromium] › tests/ui/dashboard.spec.ts:447:1 › hides the all routes override when only stale route rows remain (399ms)
  ✓  12 [chromium] › tests/ui/dashboard.spec.ts:470:1 › keys route controls from backend canonical monitor routes (363ms)
  ✓  13 [chromium] › tests/ui/dashboard.spec.ts:492:1 › strips route userinfo from drawer links and monitor targets (403ms)
  ✓  14 [chromium] › tests/ui/dashboard.spec.ts:515:1 › shows policy-blocked route statuses in the drawer (356ms)
  ✓  15 [chromium] › tests/ui/dashboard.spec.ts:536:1 › keeps slash-distinct route controls and override targets (723ms)
  ✓  16 [chromium] › tests/ui/dashboard.spec.ts:714:3 › agent report-driven compose service health › updates service tile state as agent reports arrive (2.7s)

  16 passed (17.4s)
bash scripts/release_test.sh
ok  	github.com/example/gitops-dashboard/cmd/release	(cached)
release binary clean-clone invocation passed
make[1]: Leaving directory '/tmp/gitops-dashboard-check.IFVAq5/checkout'
```

This is the complete stdout-and-stderr transcript from the round-5 `make check`
run. ANSI color and carriage-return terminal progress control characters were
removed for Markdown readability; no substantive lines were omitted.

## Documentation and Maintainability Sweep

- `docs/vision.md`: reviewed; no change. The incident remediation does not
  change the dashboard's read-only product direction.
- `docs/requirements.md`: reviewed; no change. It does not add product-facing
  requirements.
- `docs/tech_stack.md`: reviewed; no change. Docker multi-stage builds, Go,
  and the React/Vite UI remain consistent with the stated stack.
- `docs/implementation_plan.md`: reviewed; no change. This record and tracker
  entry follow its task-file and status-board convention.
- `docs/task_acceptance_criteria.md`: reviewed; no change. The E2E limitation,
  verification, and sweep are recorded here while the task remains In Review.
- `docs/tasks/TASK-0015-ci-versioning-process.md`: reviewed; no change. Its
  CI publishing scope remains applicable.
- `docs/versioning.md`: reviewed; no change. Image tag policy is unaffected.
- `docs/tasks/TASK-0047-ci-publish-wedge-hotfix.md`: added for this incident.
- `docs/tasks/tracker.md`: updated to index TASK-0047 and advance the next ID.

Maintainability verdict: the guard remains a compact Dockerfile parser focused
only on `FROM --platform` fields. Its lexical helper recognizes exact shell
variable names and avoids substring false positives; the UI stage separately
requires the entire normalized expression to be `$BUILDPLATFORM` or
`${BUILDPLATFORM}`. The scope is limited to the UI-platform pin, CI timeouts,
their regression tests, and task traceability.

Proposed Conventional Commit subject: `fix(ci): prevent arm64 UI publish wedges (T-047)`.
