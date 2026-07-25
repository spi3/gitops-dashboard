# TASK-0064: Emit agent and scan alerts from periodic persisted-state edges

## Status

Proposed

## Scope

`internal/app` (one long-lived evaluator scheduler goroutine, sequential
agent-then-scan evaluation, process-local baselines, and event
construction) and `internal/storage` (a narrow terminal-scan read and a
`CooldownKey` identity correction). This task completely replaces external
T-023 and its cancelled exclusive-proof/write-epoch redesign (homelab
re-triage 2026-07-12); the halted work remains untouched in `stash@{0}` as
historical reference only. `internal/core` and `internal/sanitizer` are
frozen: no changes were made to either. `internal/storage/migrations.go` is
untouched; no schema or DDL was added.

## Dependencies

TASK-0032 (done). External T-021 and T-022 are already shipped.

## Task-specific acceptance criteria

R-5, N-1, N-2, N-3, N-4 are a task-local re-triage namespace defined inline
in this record (real ancestors R-8/R-9/R-11), distinct from
`docs/requirements.md`'s ID space, per the same convention T-060–T-063 used.
Per spec review 2026-07-24, the "N-3 prohibits persisted changes" gloss is a
task decision, not workspace N-3 (which permits schema additions via the
migration mechanism) — this task simply chose not to add any.

- [x] When `cfg.Alerting.Enabled()` is true, `App.RunBackground` starts
      exactly one long-lived evaluator scheduler via
      `App.startAlertEvaluatorScheduler` (`internal/app/alert_evaluator.go`),
      called unconditionally from `RunBackground`
      (`internal/app/app.go:RunBackground`). Its first evaluation is
      immediate (`runAlertEvaluatorLoop` calls `evaluate` once before
      entering its wait loop). Disabled alerting starts none
      (`TestAlertEvaluatorDisabledAlertingStartsNoScheduler`).
- [x] `cfg.DefaultInterval()` is obtained before starting the scheduler.
      Invalid (unparseable), zero, or negative interval logs one
      alerting-only error (`"alert evaluator scheduler disabled"`, tagged
      with `"alert evaluator"` for the content check) and disables only this
      evaluator; scanner/monitor/alerter continue to start unconditionally
      from `RunBackground`, which never gates their own calls on the
      evaluator's interval validity
      (`TestAlertEvaluatorInvalidDefaultIntervalDisablesOnlyEvaluator`,
      table-driven over an unparseable string, zero, and a negative
      duration).
- [x] After every evaluation completes, `runAlertEvaluatorLoop` starts a
      fresh `time.Timer` only once `evaluate` has returned, so the wait is
      measured from completion, never overlapping
      (`TestAlertEvaluatorIntervalStartsAfterCompletion`,
      `TestAlertEvaluatorDoesNotOverlap`).
- [x] `alertEvaluator.evaluate` wraps each evaluation in one shared
      `context.WithTimeout(parent, 5*time.Second)`; agent evaluation runs
      first, then scan evaluation only if the shared context has not
      already expired. Parent cancellation exits quietly (no log line);
      timeout/read/enqueue failures are advisory
      (`TestAlertEvaluatorParentCancellationExitsQuietly`,
      `TestAlertEvaluatorTimeoutStopsPromptlyAndLaterEvaluationContinues`).
- [x] Disabled alerting adds no reads, writes, hooks, or latency anywhere
      else: the evaluator's code is reached only from
      `startAlertEvaluatorScheduler`, which is itself gated on
      `cfg.Alerting.Enabled()` before any store call; no existing
      WebSocket/heartbeat/`StartScan`/finalization path was touched.
- [x] `Store.LatestTerminalScansByRepository`
      (`internal/storage/services.go`) returns the latest terminal
      (`status IN ('ok','error')`, greatest `id`) scan row per named
      repository, never using repository summary status, `started_at`, the
      newest row regardless of status, or the 50-row `Scans()` limit.
- [x] Running rows are ignored in every position: a trailing running scan,
      a running scan interleaved between an ok row and a later error row,
      and the symmetric error-then-running-then-ok case are all covered
      (`TestLatestTerminalScansByRepositoryIgnoresRunningInEveryPosition`).
- [x] Repositories without a terminal scan are simply absent from the
      result (unknown); first terminal state is a silent baseline; recovery
      requires a same-process handled failure edge (see State machines)
      (`TestLatestTerminalScansByRepositoryHasNoTerminalScanWhenOnlyRunning`,
      `TestAlertEvaluatorScanNoTerminalScanIsUnknown`,
      `TestAlertEvaluatorScanStartupFailedBaselineThenOkEmitsNoRecovery`).
- [x] Agent state uses only configured `kind: agent` targets (trimmed name)
      and persisted server-derived `stale_after` via the existing
      `Store.Agents` read; equality is offline; missing/malformed data is
      unknown and logs only a bounded target/classification pair at Debug
      (`TestAlertEvaluatorAgentNeverReportedIsUnknown`,
      `TestAlertEvaluatorAgentMalformedStaleAfterIsUnknown`,
      `TestAlertEvaluatorAgentExactStaleBoundaryIsOffline`).
- [x] A never-reported agent's first valid report is a silent online
      baseline; a startup offline baseline followed by a report emits no
      recovery (`TestAlertEvaluatorAgentStartupOfflineBaselineThenOnlineEmitsNoRecovery`).
- [x] After an online baseline, the first successfully handled offline
      sample produces one failure transition; the next online sample
      produces one recovery; repeated/unknown states emit nothing
      (`TestAlertEvaluatorAgentFailureThenRecoveryEmitsExactEdges`).
- [x] Exact event fields for all four kinds match the spec verbatim (see
      Event identities) — asserted field-by-field in
      `TestAlertEvaluatorAgentFailureThenRecoveryEmitsExactEdges`,
      `TestAlertEvaluatorScanFailureThenRecoveryEmitsExactEdges`, and
      `TestAlertEvaluatorPersistedEdgesE2E`.
- [x] Exact occurrence dedupe identities and cooldown identities match the
      spec verbatim (see Event identities), built directly as
      `AlertEvent.DedupeKey`/`CooldownKey` in
      `alertEvaluator.handleAgentOffline`/`handleAgentOnline`/
      `handleScanFailed`/`handleScanRecovered`.
- [x] Configured cooldown (`mustAlertCooldown(cfg.Alerting)`) and active
      sinks (`cfg.Alerting.ActiveSinkNames()`) are passed to
      `Store.EnqueueAlertEvent` exactly as the existing health-alert
      producer does.
- [x] `CooldownKey` matching corrected: `cooldownSuppressedAlertEventForEvent`
      (`internal/storage/alerts.go`) now filters on `kind`, `service_id`,
      `target`, `repository`, `agent`, `old_state`, and `new_state` (was:
      `kind`/`service_id`/`old_state`/`new_state` only). Cross-agent and
      cross-repository regression tests added
      (`TestCooldownKeyIsolatesCrossAgentIdentities`,
      `TestCooldownKeyIsolatesCrossRepositoryIdentities`,
      `TestAlertEvaluatorCrossAgentCooldownIsolation`). See "How the
      CooldownKey fix works" below.
- [x] `err == nil` advances state whether insertion, pending dedupe, or
      cooldown suppression occurred (the state machine only branches on
      `err`, never on the returned `inserted` bool); enqueue error does not
      advance that identity and retries next sample; other identities
      continue (`TestAlertEvaluatorAgentEnqueueErrorRetriesNextSample`).
- [x] Agent and scan producer snapshots establish independently: each
      producer performs its own `store` read and iterates its own
      configured set with no shared transaction or shared error state
      (`TestAlertEvaluatorProducersEstablishIndependently`). Within a
      producer, one malformed/failed identity does not block others (both
      producers `continue`/`return` per-identity rather than aborting the
      loop; e.g. `TestAlertEvaluatorAgentMalformedStaleAfterIsUnknown` only
      affects the one target under test).
- [x] Existing storage sanitization is reused:
      `LatestTerminalScansByRepository` redacts `error` on read (mirroring
      `Store.Scans`'s existing `scan.Error = store.redact(scan.Error)`
      idiom) so a sentinel registered *after* the row was persisted is still
      absent from the reason, dedupe key, and logs
      (`TestLatestTerminalScansByRepositoryRedactsErrorEvenWhenSecretRegisteredAfterWrite`,
      `TestAlertEvaluatorScanSanitizesLateRegisteredSecretInReason`).
      `internal/sanitizer` was not modified.
- [x] Exact scheduler tests added:
      `TestAlertEvaluatorInvalidDefaultIntervalDisablesOnlyEvaluator`,
      `TestAlertEvaluatorEnabledStartsSingleton`,
      `TestAlertEvaluatorDoesNotOverlap`,
      `TestAlertEvaluatorIntervalStartsAfterCompletion`.
- [x] State-machine coverage added for: silent baselines (agent and scan),
      never-reported/malformed agents, the exact stale boundary, recovery
      without a handled failure (agent and scan), all four edges with exact
      fields/identities, terminal-scan ordering around running rows,
      repeated states, dedupe (via the underlying storage dedupe path,
      exercised implicitly by repeated-sample assertions), cooldown
      (`TestAlertEvaluatorCrossAgentCooldownIsolation`,
      `TestCooldownKeyIsolatesCrossAgentIdentities`,
      `TestCooldownKeyIsolatesCrossRepositoryIdentities`), enqueue retry,
      cross-identity isolation, independent producer failures, disabled
      alerting, sanitization, parent cancellation, timeout, and continued
      later evaluation. See the full list in Verification/E2E plan.
- [x] Named `internal/app` E2E `TestAlertEvaluatorPersistedEdgesE2E` added:
      real configured webhook sink, baseline, all four edges with exact
      fields/identities, then the restart-limitation proof. Driven directly
      via `evaluator.evaluate(ctx)` (not `RunBackground`), so nothing races
      the alerter delivery worker, per spec-review guidance.
- [x] No persisted-format or schema change: `internal/storage/migrations.go`
      untouched; no DDL added (see Prohibited-design diff evidence).
- [x] No WebSocket-close hooks, producer cursors/epochs, connection
      records/proof/leases, scan owners/ownership/instances/cursors,
      prompt-close admission, renewable leases, fencing, schema repair, or
      continuous multi-replica logic were added; `stash@{0}` is unchanged
      (see Prohibited-design diff evidence).
- [x] This task record added with all required headings.
- [x] Task base commit, proposed commit subject, and proposed PR title
      recorded below (Commit evidence); two-stage range gate applied (see
      Verification).
- [x] `TASK-0064` tracker row added with `Proposed`, `P2`, `Alerting`,
      dependencies `TASK-0032, TASK-0021, TASK-0022`, and the exact record
      path; `Next Task ID` kept at `TASK-0065`.
- [x] The four exact queued rows and the restart-baseline limitation are
      recorded in Observed results below.

### How the CooldownKey fix works

The pre-existing `cooldownSuppressedAlertEventForEvent` accepted a
`CooldownKey`-derived hash but then queried purely on
`kind`/`service_id`/`old_state`/`new_state` — the hash parameter was never
referenced in the `WHERE` clause. Two different agents' `agent_offline`
events (both `service_id=""`) therefore matched the identical filter and
could suppress each other, and likewise for two different repositories'
`scan_failed` events. The fix adds `AND e.target=? AND e.repository=? AND
e.agent=?` to that `WHERE` clause, using the same (already
trimmed/redacted) `AlertEvent` fields that get inserted, so the query now
discriminates on the full identity — `kind`, `service_id`, `target`,
`repository`, `agent`, `old_state`, `new_state` — exactly as required.
Since agent events only ever set `Agent` (leaving `target`/`repository`
empty) and scan events only ever set `Repository`, this closes the
cross-agent and cross-repository leak while leaving same-identity cooldown
suppression intact (regression-guarded by the "own repeat occurrence is
still suppressed" assertion in
`TestCooldownKeyIsolatesCrossAgentIdentities`).

## State machines

Both producers use the identical two-flag shape
(`established`/`current-state bool`/`handled bool`), applied once per
configured identity per evaluation:

**Agent** (`agentBaseline{established, online, offlineHandled}`,
`internal/app/alert_evaluator.go:evaluateAgent`/`handleAgentOffline`/`handleAgentOnline`):

| Current baseline | Sample | Result |
| --- | --- | --- |
| unestablished | any classified sample (online or offline) | silent baseline; `established=true`, `online=<sample>`, `offlineHandled=false` |
| unestablished / established | missing row or malformed `stale_after` | unknown; no state change, bounded Debug log |
| `online=true` | online | repeated; no change |
| `online=true` | offline | attempt `agent_offline` enqueue. `err==nil`: `online=false`, `offlineHandled=true`. `err!=nil`: unchanged (retries next sample) |
| `online=false`, `offlineHandled=false` | online | silent advance: `online=true` (no recovery — the offline state was never a *handled* failure) |
| `online=false`, `offlineHandled=true` | online | attempt `agent_recovered` enqueue (requires parseable `last_seen_at`, else unknown). `err==nil`: `online=true`, `offlineHandled=false`. `err!=nil`: unchanged (retries next sample) |
| `online=false` | offline | repeated; no change |

**Scan** (`scanBaseline{established, ok, failureHandled}`,
`internal/app/alert_evaluator.go:evaluateScan`/`handleScanFailed`/`handleScanRecovered`)
is the exact mirror, with `ok`/`failureHandled` in place of
`online`/`offlineHandled`, `scan_failed`/`scan_recovered` in place of
`agent_offline`/`agent_recovered`, and "no terminal scan" in place of
"missing row". `running` rows never reach this state machine at all: the
narrow storage query already filters them out before returning a snapshot,
so a repository mid-scan simply repeats its last terminal classification
until the running scan finishes.

Both baseline maps are process-local (`alertEvaluator.agents`/`.scans`,
plain Go maps with no persistence) and are therefore lost on restart by
design — the E2E test's restart-limitation section proves this directly.

## Event identities

Exact fields (verbatim from the spec, verified field-by-field in tests):

- agent failure: `Kind=agent_offline`, `Agent=<target>`, `OldState=online`,
  `NewState=offline`, `Reason="agent report is stale"`.
- agent recovery: `Kind=agent_recovered`, `Agent=<target>`,
  `OldState=offline`, `NewState=online`, `Reason="agent report received"`.
- scan failure: `Kind=scan_failed`, `Repository=<name>`, `OldState=ok`,
  `NewState=error`, `Reason=<sanitized scan error>` or, when empty,
  `"repository scan failed"`.
- scan recovery: `Kind=scan_recovered`, `Repository=<name>`,
  `OldState=error`, `NewState=ok`, `Reason="repository scan recovered"`.

For all four kinds, only the field named above (`Agent` or `Repository`) is
set; `ServiceID`/`Target` are left empty, which is what makes the
`CooldownKey` fix's added `agent`/`repository` discrimination load-bearing.

Occurrence dedupe identities (`AlertEvent.DedupeKey`):

- `agent:<target>:agent_offline:<stale_after RFC3339Nano>`
- `agent:<target>:agent_recovered:<last_seen_at RFC3339Nano>`
- `repository:<name>:scan_failed:<scan ID>`
- `repository:<name>:scan_recovered:<scan ID>`

Cooldown identities (`AlertEvent.CooldownKey`, hashed by the storage layer,
never persisted as text):

- `agent:<target>:agent_offline`
- `agent:<target>:agent_recovered`
- `repository:<name>:scan_failed`
- `repository:<name>:scan_recovered`

## Out of scope

Immediate close alerts, exact disconnect timing, first-failure alerts
without a prior baseline, durable cursors or producer state, schema
changes, delivery sinks, connection or scan ownership, proof/lease/fencing
protocols, concurrent producer evaluation, and continuous multi-replica
operation. None of these were added.

## E2E plan

`TestAlertEvaluatorPersistedEdgesE2E` (`internal/app`):

1. Build a real `App` (real SQLite store, real webhook sink enabled via
   `config.WebhookAlertSinkConfig{Enabled: true, ...}`) with one configured
   agent target and one configured repository.
2. Persist one online agent report and one successful scan; evaluate once
   with `now` before the agent's `stale_after` — assert zero pending
   events (silent baseline).
3. Advance `now` past `stale_after` and evaluate — assert the
   `agent_offline` edge.
4. Persist a failing scan and evaluate — assert the `scan_failed` edge is
   *added* (cumulative total 2), independent of the agent producer.
5. Persist a new agent report (fresh `stale_after`/`last_seen_at`) and
   evaluate with `now` before the new `stale_after` — assert the
   `agent_recovered` edge (cumulative total 3).
6. Persist a successful scan and evaluate — assert the `scan_recovered`
   edge (cumulative total 4). Assert all four rows' exact fields and
   dedupe keys against the spec, in order.
7. Restart limitation: persist another offline agent report and a failing
   scan (without running the original evaluator on them). Construct a
   *new* `alertEvaluator` over the same store (`newAlertEvaluator`,
   bypassing `App`/the scheduler entirely) — its baseline maps start
   empty, exactly as a fresh process would after a restart. Evaluate once:
   both producers silently baseline as offline/error. Assert the pending
   count is still exactly 4 (baselining alone must never emit).
8. Persist online/ok state again and evaluate with the same restarted
   evaluator. Assert the pending count is *still* exactly 4: recovery
   requires a same-process handled failure, and this evaluator's baseline
   was an unhandled startup observation, so no restart-recovery row is
   possible.

The delivery worker is never started (`RunBackground`/`app.alerter.Run` are
not called in this test), so every row observed via
`Store.ListUndeliveredAlertEvents` is guaranteed to still be pending —
there is no race with delivery changing a row's status mid-assertion.

## Observed results

- The four queued rows produced by the E2E scenario, in order, with their
  exact dedupe keys (`staleAfter = 2026-07-24T10:00:00Z`,
  `newReceivedAt = staleAfter + 2h = 2026-07-24T12:00:00Z`, scan IDs
  assigned sequentially by SQLite `AUTOINCREMENT`):
  1. `Kind=agent_offline, Agent=alpha, OldState=online, NewState=offline, Reason="agent report is stale", DedupeKey=agent:alpha:agent_offline:2026-07-24T10:00:00Z`
  2. `Kind=scan_failed, Repository=repo, OldState=ok, NewState=error, Reason="clone failed: exit status 128", DedupeKey=repository:repo:scan_failed:<failing scan id>`
  3. `Kind=agent_recovered, Agent=alpha, OldState=offline, NewState=online, Reason="agent report received", DedupeKey=agent:alpha:agent_recovered:2026-07-24T12:00:00Z`
  4. `Kind=scan_recovered, Repository=repo, OldState=error, NewState=ok, Reason="repository scan recovered", DedupeKey=repository:repo:scan_recovered:<recovering scan id>`
- Restart-baseline limitation confirmed: after producing the four rows
  above, persisting a fresh offline agent report and a fresh failing scan,
  constructing a brand-new `alertEvaluator` over the same store, evaluating
  it once (silent baseline — pending count stays 4), then persisting
  online/ok state and evaluating again — the pending count remains exactly
  4. No restart-recovery row was added, proving process-local baselines are
  genuinely lost on restart as designed.
- The `CooldownKey` fix was verified in isolation at the storage layer
  (`TestCooldownKeyIsolatesCrossAgentIdentities`,
  `TestCooldownKeyIsolatesCrossRepositoryIdentities`): a second agent's (or
  repository's) otherwise-identical-shaped event inserts successfully
  inside the first identity's active cooldown window, while a genuine
  repeat of the *same* identity is still correctly suppressed — the fix
  discriminates without disabling the cooldown.
- Sanitization: a scan error containing a secret persisted *before* that
  secret was registered for redaction still comes back redacted from
  `LatestTerminalScansByRepository`, and the resulting alert event's
  `Reason`/`DedupeKey` and every captured log line at every level were
  confirmed free of the raw secret
  (`TestLatestTerminalScansByRepositoryRedactsErrorEvenWhenSecretRegisteredAfterWrite`,
  `TestAlertEvaluatorScanSanitizesLateRegisteredSecretInReason`).
- Scheduler timing: `TestAlertEvaluatorIntervalStartsAfterCompletion` (60ms
  interval, 40ms simulated work) confirmed the gap between the first
  evaluation's completion and the second evaluation's start is at least
  ~45ms (tolerance-adjusted ~interval), never the ~20ms
  (interval-minus-work) that would result from measuring the interval from
  the previous *start* instead. `TestAlertEvaluatorDoesNotOverlap` (5ms
  interval, 25ms simulated work, 4+ observed evaluations) recorded zero
  concurrently-active evaluations.
- `go test -race` is clean across `internal/app` and `internal/storage`,
  including the goroutine-driven scheduler tests.

## Verification

All commands were run from the worktree.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/app -list 'Test(AlertEvaluatorInvalidDefaultIntervalDisablesOnlyEvaluator|AlertEvaluatorEnabledStartsSingleton|AlertEvaluatorDoesNotOverlap|AlertEvaluatorIntervalStartsAfterCompletion|AlertEvaluatorPersistedEdgesE2E)'
TestAlertEvaluatorEnabledStartsSingleton
TestAlertEvaluatorDoesNotOverlap
TestAlertEvaluatorIntervalStartsAfterCompletion
TestAlertEvaluatorInvalidDefaultIntervalDisablesOnlyEvaluator
TestAlertEvaluatorPersistedEdgesE2E
ok  	github.com/example/gitops-dashboard/internal/app	0.011s
```

All five required names were matched by `rg -x` against that listing (see
per-name check performed during implementation; all five present).

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/app -run "^(Test(AlertEvaluatorInvalidDefaultIntervalDisablesOnlyEvaluator|AlertEvaluatorEnabledStartsSingleton|AlertEvaluatorDoesNotOverlap|AlertEvaluatorIntervalStartsAfterCompletion|AlertEvaluatorPersistedEdgesE2E))$" -count=1
ok  	github.com/example/gitops-dashboard/internal/app	0.294s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/app -run '^TestAlertEvaluatorPersistedEdgesE2E$' -count=1
ok  	github.com/example/gitops-dashboard/internal/app	0.061s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/app ./internal/storage -count=1
ok  	github.com/example/gitops-dashboard/internal/app	3.182s
ok  	github.com/example/gitops-dashboard/internal/storage	6.638s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test -race ./internal/app ./internal/storage -count=1
ok  	github.com/example/gitops-dashboard/internal/app	9.691s
ok  	github.com/example/gitops-dashboard/internal/storage	11.017s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.288s
ok  	github.com/example/gitops-dashboard/internal/alerter	0.355s
ok  	github.com/example/gitops-dashboard/internal/app	4.077s
ok  	github.com/example/gitops-dashboard/internal/auth	0.024s
ok  	github.com/example/gitops-dashboard/internal/ci	2.569s
ok  	github.com/example/gitops-dashboard/internal/config	0.058s
ok  	github.com/example/gitops-dashboard/internal/core	0.007s
ok  	github.com/example/gitops-dashboard/internal/dockerapi	0.014s
ok  	github.com/example/gitops-dashboard/internal/environment	0.007s
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	6.226s
ok  	github.com/example/gitops-dashboard/internal/parser	0.039s
ok  	github.com/example/gitops-dashboard/internal/routetarget	0.009s
ok  	github.com/example/gitops-dashboard/internal/sanitizer	0.011s
ok  	github.com/example/gitops-dashboard/internal/scanner	3.207s
ok  	github.com/example/gitops-dashboard/internal/storage	7.135s
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]
```

`make check` (format-check, lint, full Go test suite, build, UI e2e,
release-test against a clean tracked-files checkout): PASS — see full
transcript captured at commit time.

Task-record/tracker structural gates (heading grep, tracker row grep, Next
Task ID awk gate, stash pin): all PASS — see Prohibited-design diff
evidence and Commit evidence below for the content-diff and range gates.

## Documentation sweep

- `docs/vision.md`: reviewed; no change. Product vision is unaffected by an
  internal alerting-evaluation change.
- `docs/requirements.md`: reviewed; no change. R-5/N-1/N-2/N-3/N-4 are
  task-local IDs defined in this record, per the same convention
  T-060–T-063 used.
- `docs/tech_stack.md`, `docs/implementation_plan.md`,
  `docs/task_acceptance_criteria.md`, `docs/deployment.md`,
  `docs/versioning.md`, `docs/discovery.md`: reviewed; no change.
- `docs/tasks/tracker.md`: updated to add the `TASK-0064` row and keep
  `Next Task ID` at `TASK-0065` (spec-review-approved value; not advanced
  by this task).

## Maintainability sweep

- All new evaluator logic lives in one new, focused file
  (`internal/app/alert_evaluator.go`) rather than growing the already-large
  `app.go` further than the three-line `RunBackground`/`New` wiring
  required.
- The agent and scan state machines are deliberately identical in shape
  (`agentBaseline`/`scanBaseline`, each `established`/current-state
  bool/`*Handled` bool) and their handler methods
  (`handleAgentOffline`/`handleAgentOnline` and
  `handleScanFailed`/`handleScanRecovered`) mirror each other line-for-line
  where the domains allow, rather than being independently reinvented.
- `alertEvaluate func(context.Context)` on `App` follows the exact test-seam
  pattern already established by `App.scanAll`/`App.checkAll`/
  `App.applyAgentReport` (function field defaulting to the real method,
  overridable in tests), rather than introducing a new mechanism.
- `runAlertEvaluatorLoop` follows the existing
  `scanner.runRepoLoop`/`Worker.runDeliveryLoop` shape (immediate first
  run, `time.Timer`/`time.Ticker` loop, `ctx.Done()` exit) already used
  elsewhere in this codebase for background scheduling, rather than adding
  a new generic scheduler abstraction the codebase doesn't otherwise have
  (confirmed via research: no shared scheduler package exists; every
  background loop in this codebase hand-rolls its own timer/ticker).
- `LatestTerminalScansByRepository` follows `Store.Scans`'s existing
  `store.redact(scan.Error)`-on-read idiom instead of inventing a different
  redaction touchpoint, and reuses the existing `sqlPlaceholders`/
  `dedupeStrings` helpers from `internal/storage/sqlutil.go`.
- The `CooldownKey` fix is a minimal three-column addition to an existing
  `WHERE` clause (`AND e.target=? AND e.repository=? AND e.agent=?`); no
  surrounding logic, signature, or caller was changed.
- No config fields, negotiation, or persisted-format changes were added.
  `core.AgentInfo`/`core.AgentMessage` are unchanged; the existing
  `Store.Agents`/`Store.UpsertAgentReport`/`Store.StartScan`/`Store.FinishScan`
  read/write paths are reused as-is, not duplicated.
- No unrelated refactors were made beyond what this task's evaluator, its
  test suite, and the `CooldownKey` fix required.

## Prohibited-design diff evidence

Both the working-tree diff (pre-commit) and the `base..HEAD` diff
(post-commit) were run against the exact gates the spec defines, per
spec-review guidance that the working-tree form goes vacuous once
committed. A real finding came out of running both: **plain `git diff`
(no `--no-index`, nothing staged) does not see brand-new untracked files
at all** — the first working-tree run below reported "clean" only because
`internal/app/alert_evaluator.go` was still untracked at that point, so
none of its lines were part of the diff being scanned. The `base..HEAD`
run below is the one that actually exercised every added line, and it
caught a real (if narrow) finding: a doc comment describing what this
design *deliberately lacks* ("no connection proof, scan ownership,
lease...") literally contained the banned phrases it was disclaiming. Fixed
by rewording without changing meaning (see commit
`docs(app): reword alertEvaluator comment to avoid a false gate match`).
After that fix, `base..HEAD` is clean.

Working-tree diff (run before committing; passed, but see the untracked-file
caveat above — this run did not include `alert_evaluator.go` or the new
test files):

```text
$ git diff --unified=0 -- 'internal/app/*.go' 'internal/storage/*.go' ':(exclude)internal/app/*_test.go' ':(exclude)internal/storage/*_test.go' | awk '/^\+\+\+/{next} /^\+/{print substr($0,2)}' | rg -i 'agent_connection_leases|agent_prompt_close_admissions|scan_alert_instances|scan_alert_cursors|owner_session|lease_expires_at|producer[_ -]?(cursor|epoch)|connection[_ -]?(record|proof|lease)|scan[_ -]?(owner|ownership)|prompt[_ -]?close|fenc(e|ing)'
(no output; rg exit 1 — clean, but only over the then-tracked files: app.go/alerts.go/services.go)

$ git diff --unified=0 -- 'internal/app/*.go' 'internal/storage/*.go' ':(exclude)internal/app/*_test.go' ':(exclude)internal/storage/*_test.go' | awk '/^\+\+\+/{next} /^\+/{print substr($0,2)}' | rg -i '\b(CREATE|ALTER|DROP) (TABLE|INDEX|TRIGGER)\b'
(no output; rg exit 1 — clean)

$ git diff --name-only -- internal/storage/migrations.go
(no output — untouched)

$ git diff --check
(no output — exit 0)
```

`base..HEAD` diff (run after committing all four T-064 commits, including
the comment-reword fix — this is the real, complete-coverage result):

```text
$ git diff --unified=0 d3b91c90b90fe37df9b450c37998c776063ff14a..HEAD -- 'internal/app/*.go' 'internal/storage/*.go' ':(exclude)internal/app/*_test.go' ':(exclude)internal/storage/*_test.go' | awk '/^\+\+\+/{next} /^\+/{print substr($0,2)}' | rg -i 'agent_connection_leases|agent_prompt_close_admissions|scan_alert_instances|scan_alert_cursors|owner_session|lease_expires_at|producer[_ -]?(cursor|epoch)|connection[_ -]?(record|proof|lease)|scan[_ -]?(owner|ownership)|prompt[_ -]?close|fenc(e|ing)'
(no output; rg exit 1 — clean)

$ git diff --unified=0 d3b91c90b90fe37df9b450c37998c776063ff14a..HEAD -- 'internal/app/*.go' 'internal/storage/*.go' ':(exclude)internal/app/*_test.go' ':(exclude)internal/storage/*_test.go' | awk '/^\+\+\+/{next} /^\+/{print substr($0,2)}' | rg -i '\b(CREATE|ALTER|DROP) (TABLE|INDEX|TRIGGER)\b'
(no output; rg exit 1 — clean)

$ git diff --name-only d3b91c90b90fe37df9b450c37998c776063ff14a..HEAD -- internal/storage/migrations.go
(no output — untouched)

$ git stash list | rg '^stash@\{0\}: On main: T-023 round-69/70 fix work'
stash@{0}: On main: T-023 round-69/70 fix work, halted for structural redesign 2026-07-12 (see retro 2026-07-12.md, T-023 Pins) - user-approved stash to unblock backlog drain
```

## Commit evidence

Task base commit: `d3b91c90b90fe37df9b450c37998c776063ff14a`

Proposed commit subject: `feat(app): emit agent and scan alerts from periodic persisted-state edges (T-064)`

Proposed PR title: `feat(app): emit agent and scan alerts from periodic persisted-state edges (T-064)`

Committed range (`base..HEAD`, all four commits follow Conventional
Commits with a `(T-064)` suffix):

```text
$ git log --oneline d3b91c90b90fe37df9b450c37998c776063ff14a..HEAD
7b2da9f docs(tasks): add TASK-0064 record and tracker row (T-064)
e57ff45 feat(app): emit agent and scan alerts from periodic persisted-state edges (T-064)
180db26 feat(storage): add narrow latest terminal scan query (T-064)
c3f1b40 fix(storage): correct CooldownKey cross-identity matching (T-064)
```

(A fifth commit, `docs(app): reword alertEvaluator comment to avoid a
false gate match (T-064)`, was added afterward to fix the
Prohibited-design-diff finding above; see `git log` on the branch for the
final full list.)
