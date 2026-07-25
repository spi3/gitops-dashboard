# TASK-0065: Startup scan immediacy + alerter dispatch lifecycle truthfulness + recovery event-kind rename

## Status

Proposed

## Scope

`internal/scanner` (immediate first scheduled scan tick), `internal/config`
(sink timeout vs. delivery lease invariant), `internal/alerter` (skipped
dispatch status, lease derived from the shared config constant),
`internal/storage` (skipped as a terminal dispatch status folded into
event-status derivation, dedupe/cooldown continuity, and duplicate-dispatch
merge parity), and `internal/app` (TASK-0064 evaluator kind rename). No
schema or DDL change: `alert_dispatches.status` and `alert_events.kind` are
plain `TEXT` columns with no `CHECK` constraint, so the new `skipped` status
and the renamed dot-convention kinds are additive data, not a migration.

This task bundles four independent fixes from two survey rounds (2026-07-24
IR-1/IR-2, 2026-07-25 IR-1); see the spec's Context section for full
file:line detail.

## Dependencies

TASK-0064 (ordering only: both modify `internal/storage/alerts.go` lifecycle
code; no functional dependency).

## Requirements

Verbatim from the spec's two requirement sections (both mandatory):

- [x] After server startup with at least one repository configured with a
      nonzero scan interval, a scheduled scan attempt for every such
      repository is recorded promptly (first loop tick immediate — same
      phase as monitor loops), not one full interval later. Subsequent
      scans keep the configured interval spacing.
      `Scanner.runRepoLoop`'s timer changed from `time.NewTimer(interval)`
      to `time.NewTimer(0)` (`internal/scanner/scanner.go`), matching the
      existing `runTargetLoop` monitor-loop pattern
      (`internal/monitor/monitor.go:330`).
      (`TestRunScheduledScansPromptlyAtStartup`,
      `internal/scanner/scanner_test.go`.)
- [x] Existing scan coalescing behavior is unchanged: a startup scheduled
      scan and a concurrent manual `POST /api/scan` for the same repository
      still coalesce to one underlying scan. Untouched: both paths already
      shared `repoScanFlights` before this task; the timer change does not
      touch `scanOne`/`repoScanFlights` at all.
      (`TestRunScheduledCoalescesWithConcurrentManualScan`,
      `internal/scanner/scanner_test.go`.)
- [x] For any configuration accepted at load time, the alerter's delivery
      lease strictly exceeds every enabled sink's configured timeout.
      Implementer's choice taken: reject the offending configuration at
      validation, naming the sink and field. `config.AlertDeliveryLeaseDuration`
      (`internal/config/config.go`) is the single source of truth; each
      sink's `TimeoutDuration()` (webhook/discord/homeAssistant) now routes
      through `requiredSinkTimeout`, which rejects a timeout `>=` the lease.
      `internal/alerter/alerter.go`'s `defaultLeaseDuration` is derived from
      the same constant rather than duplicated, so the two can never drift.
      (`TestLoadConfigRejectsSinkTimeoutAtOrAboveTheAlertDeliveryLease`,
      `internal/config/config_test.go`.)
- [x] A delivery that completes successfully within its sink timeout is
      recorded exactly once; no accepted configuration can produce
      duplicate delivery of one dispatch through lease expiry during a
      still-running send. Follows structurally from the invariant above
      (lease strictly exceeds every accepted timeout, so a send that
      finishes within its own timeout budget also finishes within the
      lease). Existing `TestStallingSinkDoesNotBlockOtherDeliveriesOrTheStore`
      and `TestWorkerIdempotentRedeliveryAfterRestart` continue to pass with
      an in-bound timeout.
- [x] A dispatch skipped because its sink is disabled or the event fails
      the sink's filters is persisted with terminal status `skipped` (exact
      persisted value), not `delivered`. New `storage.AlertDispatchStatusSkipped
      = "skipped"` (`internal/storage/alerts.go`); `Worker.deliverOne`'s
      skip branch now calls `w.complete(ctx, delivery,
      storage.AlertDispatchStatusSkipped, "")` instead of `...Delivered...`
      (`internal/alerter/alerter.go`).
      (`TestWorkerRoutesOnlyToMatchingSinksAndSkipsExcluded`, strengthened
      to assert the dispatch-level status directly,
      `internal/alerter/alerter_test.go`.)
- [x] Alert events whose dispatches terminate in any mix of
      `skipped`/`delivered` reach a terminal event status, and
      dedupe/cooldown behavior for subsequent identical events is unchanged
      relative to today's behavior when those dispatches were recorded as
      `delivered`. `syncAlertEventStatus` folds `skipped` into the same
      "closed" bucket as `delivered` for event-status derivation
      (`internal/storage/alerts.go`). Two more call sites needed the same
      fold to avoid regressing today's behavior (both previously
      special-cased `delivered` only, because a skip used to *be* persisted
      as delivered): the every-startup SQL reconciliation pass
      `reconcileLegacyAlertEventStatuses` (`internal/storage/migrations.go`
      — see "Dedupe/cooldown continuity" below, this is the one with real
      operational bite) and the duplicate-dispatch dedupe-hash merge helpers
      `alertDispatchMergeBetter`/`mergeDuplicateAlertDispatches`
      (`internal/storage/migrations.go`) plus
      `mergeRouteAlertCollisionDispatches` (`internal/storage/route_targets.go`),
      all now routed through a shared `closedWithoutRetryAlertDispatchStatus`
      predicate instead of an `== AlertDispatchStatusDelivered` literal.
      (`TestSkippedDispatchStatusFixtureOpensAndOperatesNormally`'s final
      section proves a fresh skip closes an event exactly like a delivery;
      `internal/storage/storage_test.go`.)
- [x] Pre-change fixture (standing invariant 3): a database created by the
      current release containing dispatch rows in `pending`, `delivered`,
      and `dead_lettered` states opens and operates normally after this
      change — existing rows keep their statuses, claiming and completion
      continue to work.
      (`TestSkippedDispatchStatusFixtureOpensAndOperatesNormally`,
      `internal/storage/storage_test.go` — built from the exact current
      schema DDL constants via raw `INSERT`, bypassing the Go API, then
      reopened.)
- [x] Behavior elsewhere unchanged: full Go test suite green (see
      Verification).

### Additional requirements (survey 2026-07-25, IR-1)

- [x] The evaluator emits exactly `agent.offline`, `agent.recovery`,
      `scan.failure`, `scan.recovery`, replacing `agent_offline`,
      `agent_recovered`, `scan_failed`, `scan_recovered`
      (`internal/app/alert_evaluator.go`, new
      `alertKindAgentOffline`/`alertKindAgentRecovery`/`alertKindScanFailure`/
      `alertKindScanRecovery` constants). Dedupe/cooldown key *shapes* are
      unchanged — same `"agent:" + target + ":" + kind + ...` /
      `"repository:" + name + ":" + kind + ...` construction — only the
      embedded kind substring changed, now built from the same constant
      instead of a duplicated literal.
      (`internal/app/alert_evaluator_test.go`, updated to assert the new
      literals.)
- [x] Every agent/scan recovery event is rendered as a recovery by all
      sinks: `IsRecovery()` true, Discord green-check icon, `Summary()`
      "recovered" phrasing. `IsRecovery()` itself is unchanged (still
      `strings.HasSuffix(kind, ".recovery")`,
      `internal/alerter/payload.go`) — no widening.
      (`TestEventPayloadRendersAlertEvaluatorKindsAsExpected`, new,
      `internal/alerter/payload_recovery_test.go`, asserting `IsRecovery()`
      and `Summary()` directly for all four literal kinds; two new Discord
      table cases in `TestDiscordSinkFormatsReadableMessagesForEventKinds`
      assert the rendered checkmark icon end to end,
      `internal/alerter/alerter_test.go`.)
- [x] Non-test Go code contains no occurrence of `agent_offline`,
      `agent_recovered`, `scan_failed`, or `scan_recovered` (comments
      included); test fixtures for the pre-change requirement below may
      still contain them. Verified by the spec's mandated grep (see
      Verification).
- [x] One-time continuity effects documented and accepted (see
      "Dedupe/cooldown continuity" below).
- [x] Pre-change fixture (standing invariant 3): a database created by the
      current release containing `alert_events` rows with the underscore
      kinds — at least one pending event with undelivered dispatches and
      one terminal event — opens and operates normally after this change;
      the pending dispatches still deliver.
      (`TestUnderscoreAlertKindFixtureOpensAndOperatesNormally`,
      `internal/storage/storage_test.go`.)
- [x] In-repo docs no longer present the underscore kinds as current:
      `docs/tasks/TASK-0064-agent-and-scan-alert-evaluator.md` gained a
      dated note after its kind/key enumerations; `docs/tasks/tracker.md`
      reflects the new TASK-0065 row (this file).

## Dedupe/cooldown key continuity

Both changes that touch persisted key material were reviewed against the
same question: does an already-persisted row stop matching a freshly
computed key, and if so, is that accepted?

- **Skipped status (first requirements section).** `skipped` is not
  embedded in any dedupe/cooldown key — those keys never included dispatch
  status, only event identity fields (kind, service/target/repository/agent,
  old/new state) and, for dedupe, a timestamp or scan ID. No key changes
  shape or value. The only behavior change is what a dispatch's *own*
  status column reads after completion, and (per the requirement above)
  what that status contributes to event-status derivation — already proven
  unchanged relative to `delivered`.
- **Kind rename (IR-1).** The dedupe/cooldown key *shapes* are unchanged —
  same concatenation pattern — but they embed the kind string itself
  (`alert_evaluator.go:158-159`, `:189-190` pre-rename; equivalent lines
  post-rename), so the rename changes the literal key value for these four
  producers only. Consequence: any `agent.offline`/`agent.recovery`/
  `scan.failure`/`scan.recovery` occurrence looks for its prior state under
  a *different* dedupe/cooldown hash than the `agent_offline`/etc. rows
  persisted while TASK-0064 was live (roughly one day of homelab rows,
  2026-07-24 to 2026-07-25). Concretely: a currently-open (pending) old-kind
  event will not be matched by `pendingAlertEventForDedupeHash` for a new
  occurrence with the same real-world identity (the new one enqueues a
  separate row instead of joining the old one's pending dispatches), and an
  old-kind event's cooldown window will not suppress a new-kind occurrence
  even if the real-world cooldown period has not yet elapsed. Both
  possible outcomes — one duplicate optional alert, or one missed
  suppression — fall within STANDARDS' "optional-alert duplication or
  omission limited to process restart" allowance, and this is explicitly a
  one-time effect: once the old-kind rows age out (naturally, through
  ordinary delivery/dead-lettering/retention pruning), all four producers'
  keys are self-consistent again under the new kind strings, permanently.
  This is accepted per the spec and documented in
  `docs/tasks/TASK-0064-agent-and-scan-alert-evaluator.md`'s dated note.
- **The every-startup reconcile pass.** `reconcileLegacyAlertEventStatuses`
  (`internal/storage/migrations.go`) runs on every process start as part of
  migration step `009_repair_alert_table_constraints` (`alertOnly: true`,
  unconditional) and re-derives `alert_events.status` from
  `alert_dispatches.status` in raw SQL, independently of the Go
  `syncAlertEventStatus` helper. Before this task, its two "delivered
  exists" checks did not know about `skipped`; left unfixed, an event whose
  dispatches were *all* skipped would fall through to its `ELSE` branch and
  be rewritten to `AlertEventStatusFailed` on every single restart — a
  standing truthfulness bug this task exists to prevent, not just avoid
  introducing. Both `EXISTS (d.status=?)` checks that named `delivered`
  became `EXISTS (d.status IN (?, ?))` naming `delivered, skipped`,
  matched to a hand-verified placeholder count (28, confirmed by direct
  inspection of every `?` against the flattened argument list) since
  `database/sql` does not validate placeholder/argument arity at compile
  time.

## Out of scope

Renaming the health producer's kinds (already follow the convention);
making the lease or poll interval operator-configurable; retry-policy
changes; exposing dispatch history via API/UI; the alerter dead-constant
advisory (`defaultSinkRequestTimeout`); `wg.Wait` cross-batch delay; any
change to sink delivery semantics beyond the lease invariant.

## Verification

```sh
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build ./...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/scanner ./internal/alerter ./internal/storage ./internal/config -count=1
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
rg -n 'AlertDispatchStatusSkipped|"skipped"' internal/storage internal/alerter
rg -in 'lease' internal/config internal/alerter --glob '*_test.go' | rg -iv 'release'
rg -n 'agent_offline|agent_recovered|scan_failed|scan_recovered' internal/ cmd/ --type go -g '!*_test.go' && exit 1 || true
rg -n 'agent\.offline|agent\.recovery|scan\.failure|scan\.recovery' internal/app/alert_evaluator.go
```

All seven commands pass (see the branch's implementation report for output
tails). Also run and green: `make check` (format, `go vet`, full Go test
suite, UI typecheck/lint/build, Playwright e2e, release test).

## Maintainability sweep

- The lease invariant lives in exactly one place
  (`config.AlertDeliveryLeaseDuration`); both the validator that rejects an
  over-long sink timeout and the worker that actually enforces the lease
  read the same constant, so they cannot drift apart the way two
  independently maintained numbers could.
- `closedWithoutRetryAlertDispatchStatus` replaces four separate
  `== AlertDispatchStatusDelivered` literals across
  `internal/storage/migrations.go` and `internal/storage/route_targets.go`
  with one named predicate, documenting in one place why `skipped` joins
  `delivered` there (parity with pre-task behavior, where a skip was itself
  persisted as delivered) and why `dead_lettered` does not.
- No unrelated refactors: the four changes touch only the files the spec
  names, plus the two additional call sites
  (`reconcileLegacyAlertEventStatuses`, the duplicate-dispatch merge
  helpers) that a careful read of the existing dispatch-status literal
  checks showed would otherwise regress "unchanged... relative to today's
  behavior when recorded as delivered".

## Commit evidence

Task base commit: `3dd9ece395d84e852edcf6271004a3c37a983756`

Proposed commit subjects:
- `feat(scanner): scan every repository immediately on startup (T-065)`
- `feat(config): reject sink timeouts that outlive the alert delivery lease (T-065)`
- `fix(alerter): persist skipped dispatches instead of delivered (T-065)`
- `fix(app): rename evaluator alert kinds to the dot recovery convention (T-065)`
- `docs(tasks): add TASK-0065 record and tracker row (T-065)`
