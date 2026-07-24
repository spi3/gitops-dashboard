# TASK-0062: Bound Kubernetes monitor cycles

## Status

Proposed

## Scope

`internal/monitor`, production and test code only. This is the homelab
replacement for the permanent-stall-prevention portion of external T-035
(homelab re-triage 2026-07-12; T-035 is superseded). Batching, informers,
selector indexes, and enrichment optimization remain cancelled and out of
scope.

## Dependencies

None.

## Task-specific acceptance criteria

- R-3: every Kubernetes API request and target check is bounded so a request
  or phase deadline aborts the current check, persists defined failure state
  through a separate bounded write phase, and cannot prevent a later
  scheduled attempt.
- N-1: practical bounds — a per-request `rest.Config.Timeout`, a derived
  check-phase budget, and a separate bounded failure-write phase.
- N-4: exact focused, E2E, repository, documentation, and commit evidence
  (see Verification, E2E plan, Documentation sweep, Commit evidence).

(R-3/N-1/N-4 are a task-local re-triage namespace defined inline in this
record, distinct from `docs/requirements.md`'s ID space, per the same
convention T-060/T-061 used — see spec review 2026-07-24.)

Concretely:

- [x] `rest.Config.Timeout` is set to exactly `10 * time.Second` before
      `dynamic.NewForConfig`, via the private `kubernetesRESTConfig` helper
      (`internal/monitor/kubernetes_bounds.go`), tested with a temporary
      kubeconfig (`TestKubernetesRESTConfigTimeout`).
- [x] The applicable-service definition, request maximum, clamp, margin,
      zero-service behavior, and cycle formula live in one documented
      private helper, `computeKubernetesCycleBudget`
      (`internal/monitor/kubernetes_bounds.go`), covering zero, one,
      multiple, unsupported-only, below-five-second, in-range,
      above-two-minute, and cap-limited inputs (`TestKubernetesCycleBudget`).
- [x] The Kubernetes check phase is wrapped in the derived budget in both
      `runKubernetesLoop` (via `runTargetLoop`'s `checkTimeout` hook) and
      `CheckAll` (via `runCheckWithTimeout`), both computed from the same
      `Monitor.kubernetesCycleBudget` seam
      (`TestCheckAllUsesKubernetesCycleBudget`).
- [x] Any workload `Get` or pod `List` error satisfying
      `errors.Is(err, context.DeadlineExceeded)` (or `context.Canceled`, for
      the parent-ending case below) aborts `checkKubernetesWithClient`
      immediately via the shared `isContextTerminal` check, returning the
      error unconverted rather than writing an ordinary per-service
      `HealthError` or a "metadata unavailable" message
      (`TestKubernetesGetDeadlineAbortsTargetCheck`,
      `TestKubernetesPodListDeadlineAbortsTargetCheck`).
- [x] Immediate fake-client deadline regressions cover both the workload
      `Get` and the pod `List` path; both reactors return
      `context.DeadlineExceeded` synchronously, so neither test waits for
      the production ten-second request timeout.
- [x] `TestKubernetesRESTRequestTimeoutAbortsTargetCheck` uses a real dynamic
      client against a stalling `httptest.Server`, with a 50ms test-only
      `rest.Config.Timeout` and a 5-second injected context deadline
      (standing in for a much longer phase budget). It proves the request
      timeout — not the injected deadline — aborts the check by asserting
      the abort lands in well under a second.
- [x] On an explicit phase or request-level deadline while the parent
      context remains active, `handleKubernetesCheckFailure` routes to
      `recordKubernetesTargetFailure`, which writes `HealthError` /
      exactly `monitor target check failed` for every applicable service
      through a separate `context.WithTimeout(context.WithoutCancel(parent),
      2*time.Second)` context, overwriting any earlier result from the same
      failed attempt (`TestKubernetesPhaseDeadlinePersistsCoveredFailures`).
- [x] `recordKubernetesTargetFailure` returns immediately, writing nothing,
      when `parent.Err() != nil` — covering both parent
      `context.Canceled` and parent `context.DeadlineExceeded` identically —
      including the case where the parent ends mid-request while a fake
      client reactor is blocked on it
      (`TestKubernetesParentCancellationSkipsFailurePersistence`).
- [x] Unsupported Kubernetes kinds remain `HealthNotApplicable`, issue zero
      API requests, and are excluded from
      `recordKubernetesTargetFailure`'s applicable-service filter, so they
      are never overwritten by a generic failure row
      (`TestKubernetesRequestCounts/zero_for_unsupported_kinds`).
- [x] A stalled earlier supported service aborts the whole per-target check
      before any later service (including unsupported ones) is reached, so
      a previously seeded unsupported row is left untouched and an unseeded
      unsupported service gets no row at all
      (`TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched`).
- [x] Actual dynamic-client action counts are asserted directly via
      `client.Actions()`: two requests for usable pod-selection metadata,
      one when pod selection is unusable, zero for unsupported kinds, and
      exactly two per applicable service (never more) across multiple
      services (`TestKubernetesRequestCounts`).
- [x] After a first scheduled timeout, `runTargetLoop`'s existing
      `failures`/`nextInterval` bookkeeping (unchanged) increments the
      failure count and schedules the second attempt after one configured
      interval (`nextInterval(interval, 1) == interval`) measured from
      completion of the failed attempt, including its bounded failure-write
      phase; a successful second attempt resets `failures` to 0 via the same
      unchanged path (`TestKubernetesScheduledCycleRecoversAfterTimeout`).
- [x] `Monitor.kubernetesCycleBudget` is a private field seam
      (`kubernetesCycleBudgetFunc`) initialized in `New` to
      `computeKubernetesCycleBudget`; timing tests substitute millisecond
      budgets without touching the production `kubernetesRequestTimeout`
      constant or the formula itself
      (used by `TestCheckAllUsesKubernetesCycleBudget`).
- [x] Added the eleven exact tests listed in Verification, all in
      `internal/monitor/kubernetes_bounds_test.go` except
      `TestRunTargetLoopWithoutCheckTimeoutDoesNotAddDefaultDeadline`'s call
      site update in `internal/monitor/monitor_test.go` (signature-only).
- [x] Timing tests use deterministic channels (`attempted`, `getStarted`,
      buffered result channels) and injected short budgets/timeouts (30ms,
      50ms), each with a `select`/`time.After` wall-clock outer guard of at
      least two seconds; none sleeps for ten seconds.
- [x] No config fields, Kubernetes writes, batching, concurrency fan-out,
      informers, watches, prefetching, selector indexes, or scanner
      enrichment changes were added. The two new `Monitor` fields
      (`kubernetesCycleBudget`, `kubernetesClient`) are test/production
      seams, not configuration — mirroring the existing `ping pingFunc`
      field — and add zero new capability surface.
- [x] No schema or persisted-format change; no upgrade fixture added.
- [x] Added this task record with the required headings.
- [x] Recorded the base commit, a proposed T-062 commit subject, and a
      proposed PR title, and applied the two-stage commit-range gate (see
      Commit evidence).
- [x] Added the `TASK-0062` tracker row and kept `Next Task ID` at
      `TASK-0065`.
- [x] Recorded a system-facing E2E through production `kubernetesRESTConfig`
      construction and `CheckAll`, showing bounded failure rows and a later
      successful check (see E2E plan/Observed results).

## Out of scope

Kubernetes batching, fan-out, informer/watch caches, new configuration,
selector indexes, scanner enrichment, schema changes, and budget
optimization beyond the pinned homelab formula.

## Cycle budget

Definitions (all pinned by the spec, not tunable):

- An applicable service has `Runtime == "kubernetes"` and a kind accepted by
  `gvrForKind` (`Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob`).
- Each applicable service issues at most two Kubernetes API requests per
  attempt: one workload `Get`, followed only when pod-selection metadata is
  usable (non-empty `matchLabels`/`matchExpressions`) by one pod `List`.
- Per-request timeout: exactly `10 * time.Second` (`kubernetesRequestTimeout`,
  set via `rest.Config.Timeout`).
- Margin: exactly `2 * time.Second` (`kubernetesCycleMargin`).
- `intervalCap = clamp(configuredTargetInterval, 5*time.Second, 2*time.Minute)`
  (`kubernetesIntervalFloor`/`kubernetesIntervalCeiling`).
- For `n > 0` applicable services:
  `cycleBudget = min(intervalCap, 2*time.Second + n*2*10*time.Second)`.
- For `n == 0`: `cycleBudget = 2*time.Second`.
- The configured interval is `target.CheckInterval(cfg.DefaultInterval())`;
  the formula applies identically to scheduled Kubernetes loops
  (`runKubernetesLoop`) and `CheckAll`.
- The failure-write phase (`kubernetesFailureWriteTimeout = 2 * time.Second`,
  derived from `context.WithoutCancel(parent)`) is outside this budget, per
  the spec's "deadline-to-return timing includes generic failure-row
  writes" rule.

All five constants and the formula live in
`internal/monitor/kubernetes_bounds.go` (`computeKubernetesCycleBudget`),
exercised by `TestKubernetesCycleBudget`'s eight input classes.

### Shared-vs-Kubernetes-specific failure-writer decision

Today's generic `recordTargetFailure` (`internal/monitor/monitor.go`) is
shared across docker/http/ping/kubernetes: it skips only parent
`context.Canceled`, and otherwise writes through the existing 30-second
`statusWriteContext` (which itself derives a `context.WithoutCancel` context
only on parent `context.DeadlineExceeded`). The spec's Kubernetes-only rules
diverge from that in two ways: a distinct two-second (not 30-second) write
context, and a no-write rule on parent `context.DeadlineExceeded` in addition
to parent `context.Canceled`.

Per the spec-review note recommending a Kubernetes-specific change: this task
added a separate `recordKubernetesTargetFailure` /
`handleKubernetesCheckFailure` pair in `kubernetes_bounds.go`, used only by
the Kubernetes loop and `CheckAll` branch. `recordTargetFailure` itself is
untouched, and docker/http/ping keep their exact prior failure-persistence
behavior — verified by the full pre-existing `internal/monitor` suite passing
unchanged (see Verification). `runTargetLoop`'s failure-handling slot changed
from a `covered ...func([]core.Service) []core.Service` list (which only ever
fed the shared writer) to a single `onFailure func(context.Context, error,
[]core.Service)` callback, so each runtime can supply its own policy; docker,
http, and ping pass `genericFailureHandler(...)`, an extracted closure with
byte-for-byte the same `errors.Is(err, context.Canceled)` skip and
`recordTargetFailure` call the inline code previously had, and Kubernetes
passes `handleKubernetesCheckFailure` instead.

Kubernetes errors that are neither `context.DeadlineExceeded` nor
`context.Canceled` (for example an invalid kubeconfig) still fall back to the
shared `recordTargetFailure` inside `handleKubernetesCheckFailure`, so that
pre-existing behavior for non-deadline Kubernetes failures is unchanged.

## E2E plan

1. **Production REST-config path**: write a temporary kubeconfig file to
   disk; call the private `kubernetesRESTConfig` helper (the same helper
   `checkKubernetes`'s production `kubernetesClient` seam calls) with that
   path; assert the returned `*rest.Config.Timeout` is exactly
   `10 * time.Second` and `Host` matches the configured cluster server.
   (`TestKubernetesRESTConfigTimeout`.)
2. **Bounded failure rows through `CheckAll`**: seed two applicable services
   (one with a prior healthy status) plus, in a second run, an unsupported
   service (one pre-seeded not-applicable, one never seeded) after the
   applicable one. Inject a fake dynamic client (via the `kubernetesClient`
   seam) whose `Get` reactor returns `context.DeadlineExceeded`
   synchronously. Run `monitor.CheckAll(ctx)` with a live parent context.
   Expect: `CheckAll` returns an error satisfying
   `errors.Is(err, context.DeadlineExceeded)`; every applicable service
   (including the one with a stale healthy row) now reads `HealthError` /
   `monitor target check failed`; the pre-seeded unsupported row and the
   never-seeded unsupported service are both untouched.
   (`TestKubernetesPhaseDeadlinePersistsCoveredFailures`,
   `TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched`.)
3. **Parent cancellation mid-request**: seed one applicable service; inject
   a fake client whose `Get` reactor blocks on the same cancelable context
   `CheckAll` was called with, then returns `ctx.Err()`. Run `CheckAll` in a
   goroutine, wait for the reactor to signal it has started, then cancel the
   context. Expect: `CheckAll` returns promptly with
   `errors.Is(err, context.Canceled)`, and the store has zero status rows
   for that service. (`TestKubernetesParentCancellationSkipsFailurePersistence`.)
4. **Scheduled recovery**: run `runKubernetesLoop` against a fake client
   whose reactor fails the first `Get` with `context.DeadlineExceeded` and
   succeeds afterward, with a 30ms configured interval. Expect: after the
   first attempt, the store shows the Kubernetes failure row; after the
   second, scheduled one interval later, it shows `HealthHealthy`.
   (`TestKubernetesScheduledCycleRecoversAfterTimeout`.)
5. **Real request timeout vs. phase budget**: two runs against a stalling
   `httptest.Server` behind a real dynamic client. One pairs a 50ms
   `rest.Config.Timeout` with a 5-second context deadline
   (`TestKubernetesRESTRequestTimeoutAbortsTargetCheck`); the other pairs a
   5-second `rest.Config.Timeout` with a 50ms `CheckAll`-level
   `kubernetesCycleBudget` seam override
   (`TestCheckAllUsesKubernetesCycleBudget`). Expect both: the check aborts
   with `errors.Is(err, context.DeadlineExceeded)` in well under a second,
   proving each bound fires independently of the other.

## Observed results

- `kubernetesRESTConfig("<temp kubeconfig>", "")` returned `Timeout =
  10s`, `Host = https://example.invalid:6443`
  (`TestKubernetesRESTConfigTimeout`).
- `computeKubernetesCycleBudget` matched the formula across all eight input
  classes, including the floor (2s interval → 5s cap), the ceiling (7
  applicable services, 10m interval → 2m cap), and cap-limited-within-range
  (10 applicable services, 45s interval → 45s) cases (`TestKubernetesCycleBudget`).
- Fake-client action counts: 2 for usable pod-selection metadata, 1 for
  unusable selection, 0 for an unsupported kind, 4 total (2 each, never
  more) across two applicable services (`TestKubernetesRequestCounts`).
- An immediate `context.DeadlineExceeded` fake reactor on `Get` and,
  separately, on pod `List` each aborted `checkKubernetesWithClient` in
  under 100ms with no per-service status row written
  (`TestKubernetesGetDeadlineAbortsTargetCheck`,
  `TestKubernetesPodListDeadlineAbortsTargetCheck`).
- Against a stalling `httptest.Server`, a 50ms `rest.Config.Timeout` aborted
  the check in ~120ms despite a 5-second context deadline
  (`TestKubernetesRESTRequestTimeoutAbortsTargetCheck`); with the timeout
  and budget roles reversed via the `CheckAll`-level seam, a 50ms phase
  budget aborted `CheckAll` in ~130ms despite a 5-second
  `rest.Config.Timeout` (`TestCheckAllUsesKubernetesCycleBudget`).
- A phase-deadline `CheckAll` run overwrote a pre-existing healthy "api" row
  and wrote a fresh "db" failure row, both `HealthError` /
  `monitor target check failed`
  (`TestKubernetesPhaseDeadlinePersistsCoveredFailures`).
- In the same kind of run reordered so an applicable service stalls before
  two unsupported services, the pre-seeded unsupported row stayed
  `HealthNotApplicable` / "prior not applicable" and the unseeded
  unsupported service had no status row at all
  (`TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched`).
- Canceling the parent context while a fake `Get` reactor was blocked on it
  made `CheckAll` return `context.Canceled` promptly with zero status rows
  persisted (`TestKubernetesParentCancellationSkipsFailurePersistence`).
- A scheduled Kubernetes loop with a 30ms interval showed a failure row after
  attempt one and a healthy row after attempt two, without any fixed sleep
  (`TestKubernetesScheduledCycleRecoversAfterTimeout`).

## Verification

All commands were run from the worktree.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/monitor -list 'Test(KubernetesRESTConfigTimeout|KubernetesCycleBudget|KubernetesRequestCounts|KubernetesGetDeadlineAbortsTargetCheck|KubernetesPodListDeadlineAbortsTargetCheck|KubernetesRESTRequestTimeoutAbortsTargetCheck|KubernetesPhaseDeadlinePersistsCoveredFailures|KubernetesDeadlineLeavesLaterUnsupportedServicesUntouched|KubernetesScheduledCycleRecoversAfterTimeout|KubernetesParentCancellationSkipsFailurePersistence|CheckAllUsesKubernetesCycleBudget)'
TestKubernetesRESTConfigTimeout
TestKubernetesCycleBudget
TestKubernetesRequestCounts
TestKubernetesGetDeadlineAbortsTargetCheck
TestKubernetesPodListDeadlineAbortsTargetCheck
TestKubernetesRESTRequestTimeoutAbortsTargetCheck
TestKubernetesPhaseDeadlinePersistsCoveredFailures
TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched
TestKubernetesParentCancellationSkipsFailurePersistence
TestKubernetesScheduledCycleRecoversAfterTimeout
TestCheckAllUsesKubernetesCycleBudget
ok  	github.com/example/gitops-dashboard/internal/monitor	0.014s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/monitor -run "^(Test(KubernetesRESTConfigTimeout|KubernetesCycleBudget|KubernetesRequestCounts|KubernetesGetDeadlineAbortsTargetCheck|KubernetesPodListDeadlineAbortsTargetCheck|KubernetesRESTRequestTimeoutAbortsTargetCheck|KubernetesPhaseDeadlinePersistsCoveredFailures|KubernetesDeadlineLeavesLaterUnsupportedServicesUntouched|KubernetesScheduledCycleRecoversAfterTimeout|KubernetesParentCancellationSkipsFailurePersistence|CheckAllUsesKubernetesCycleBudget))$" -count=1 -v
--- PASS: TestKubernetesRESTConfigTimeout (0.00s)
--- PASS: TestKubernetesPodListDeadlineAbortsTargetCheck (0.07s)
--- PASS: TestKubernetesPhaseDeadlinePersistsCoveredFailures (0.07s)
--- PASS: TestKubernetesGetDeadlineAbortsTargetCheck (0.07s)
--- PASS: TestKubernetesRESTRequestTimeoutAbortsTargetCheck (0.12s)
--- PASS: TestKubernetesParentCancellationSkipsFailurePersistence (0.06s)
--- PASS: TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched (0.04s)
--- PASS: TestKubernetesScheduledCycleRecoversAfterTimeout (0.09s)
--- PASS: TestCheckAllUsesKubernetesCycleBudget (0.11s)
    --- PASS: TestKubernetesCycleBudget/zero_applicable_services (0.00s)
    --- PASS: TestKubernetesCycleBudget/unsupported-only (0.00s)
    --- PASS: TestKubernetesCycleBudget/one_applicable,_in_range (0.00s)
    --- PASS: TestKubernetesCycleBudget/multiple_applicable,_uncapped (0.00s)
    --- PASS: TestKubernetesCycleBudget/below_five_seconds_clamps_to_floor (0.00s)
    --- PASS: TestKubernetesCycleBudget/above_two_minutes_clamps_to_ceiling (0.00s)
    --- PASS: TestKubernetesCycleBudget/cap-limited_within_range (0.00s)
--- PASS: TestKubernetesCycleBudget (0.00s)
    --- PASS: TestKubernetesRequestCounts/two_requests_for_usable_pod-selection_metadata (0.06s)
    --- PASS: TestKubernetesRequestCounts/one_request_when_pod_selection_is_unusable (0.05s)
    --- PASS: TestKubernetesRequestCounts/zero_for_unsupported_kinds (0.05s)
    --- PASS: TestKubernetesRequestCounts/never_more_than_two_per_applicable_service_across_multiple_services (0.05s)
--- PASS: TestKubernetesRequestCounts (0.00s)
PASS
ok  	github.com/example/gitops-dashboard/internal/monitor	0.247s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/monitor -count=1
ok  	github.com/example/gitops-dashboard/internal/monitor	5.783s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.026s
ok  	github.com/example/gitops-dashboard/internal/alerter	0.324s
ok  	github.com/example/gitops-dashboard/internal/app	1.237s
ok  	github.com/example/gitops-dashboard/internal/auth	0.014s
ok  	github.com/example/gitops-dashboard/internal/ci	2.418s
ok  	github.com/example/gitops-dashboard/internal/config	0.031s
ok  	github.com/example/gitops-dashboard/internal/core	0.006s
ok  	github.com/example/gitops-dashboard/internal/dockerapi	0.020s
ok  	github.com/example/gitops-dashboard/internal/environment	0.017s
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	5.939s
ok  	github.com/example/gitops-dashboard/internal/parser	0.028s
ok  	github.com/example/gitops-dashboard/internal/routetarget	0.014s
ok  	github.com/example/gitops-dashboard/internal/sanitizer	0.004s
ok  	github.com/example/gitops-dashboard/internal/scanner	3.081s
ok  	github.com/example/gitops-dashboard/internal/storage	7.078s
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]
```

Also confirmed clean with the race detector:
`GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/monitor -race -count=1` passed with no data races.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local make check
...
  16 passed (18.4s)
bash scripts/release_test.sh
ok  	github.com/example/gitops-dashboard/cmd/release	(cached)
release binary clean-clone invocation passed
```

`make check` ran gofmt, the UI build/lint/typecheck, `go vet`, the full Go
test suite (including `internal/monitor`), Playwright (16/16), and
`scripts/release_test.sh`, all against a clean clone checkout, all green.

```text
$ git diff --check
```

No output; exited 0.

## Documentation sweep

- `docs/vision.md`: reviewed; no change. Product vision is unaffected by
  monitor-internal deadline bounding.
- `docs/requirements.md`: reviewed; no change. R-3/N-1/N-4 are task-local
  IDs defined in this record, distinct from the project ID space, per the
  same convention T-060/T-061 used.
- `docs/tech_stack.md`, `docs/implementation_plan.md`,
  `docs/task_acceptance_criteria.md`, `docs/deployment.md`,
  `docs/versioning.md`, `docs/discovery.md`: reviewed; no change.
- `docs/tasks/tracker.md`: updated to add the `TASK-0062` row and keep
  `Next Task ID` at `TASK-0065` (the spec-review-approved value; not
  advanced by this task).

## Maintainability sweep

- All new Kubernetes bounding logic (constants, `computeKubernetesCycleBudget`,
  `kubernetesRESTConfig`, `productionKubernetesClient`,
  `recordKubernetesTargetFailure`, `handleKubernetesCheckFailure`,
  `isContextTerminal`, `isApplicableKubernetesService`/
  `applicableKubernetesServices`/`countApplicableKubernetesServices`) lives
  in one new, focused file, `internal/monitor/kubernetes_bounds.go`, mirroring
  the existing per-feature file split (`ping.go`, `http_routes.go`) rather
  than growing the already-large `monitor.go` further.
- `runTargetLoop`'s failure-handling parameter changed from a
  `covered ...func([]core.Service) []core.Service` list (which only ever fed
  one hardcoded `recordTargetFailure` call) to a single injected
  `onFailure func(context.Context, error, []core.Service)` callback. This is
  a strict generalization: `genericFailureHandler` reproduces the prior
  docker/http/ping behavior byte-for-byte as an extracted closure, while
  Kubernetes supplies its own policy — removing the special-casing that
  would otherwise have been needed to give one runtime different
  failure-write semantics inside a function shared by four.
- `checkKubernetesWithClient`'s two deadline call sites (`Get`, and the pod
  `List` inside `observedKubernetesImages`) both route through the same
  `isContextTerminal` predicate rather than duplicating the
  `errors.Is(err, context.DeadlineExceeded) ||
  errors.Is(err, context.Canceled)` check.
- The two new `Monitor` fields (`kubernetesCycleBudget`, `kubernetesClient`)
  follow the existing `ping pingFunc` seam pattern already on `Monitor`,
  rather than introducing a new mechanism for substituting test doubles.
- No unrelated refactors were made beyond what the bounding requirements and
  the shared/Kubernetes-specific failure-writer split required.

## Commit evidence

Task base commit: `8e76c90ed8c8c0f0c8c8f1f085b5a6e0f3a05500`

Proposed commit subject: `feat(monitor): bound Kubernetes check cycles against permanent stalls (T-062)`

Proposed PR title: `feat(monitor): bound Kubernetes check cycles against permanent stalls (T-062)`
