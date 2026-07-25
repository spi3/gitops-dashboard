# TASK-0066: Storage duplication consolidation + redaction file split

## Status

Proposed

## Scope

`internal/storage` (service-replacement skeleton, status-table delete
statements, prune-tail sharing, route-canonicalization family) and
`internal/config` (alert-redaction machinery moved to its own file). Pure
behavior-preserving refactor: no exported API, schema, or query-result
change.

Satisfies survey 2026-07-24 IR-3 (F4+F5), IR-5 (F7+F8), IR-6 (F9 only; F10
closed as evidence-refuted at spec review 2026-07-24 — the surveyed
migrations.go split was dropped from this task).

## Dependencies

None.

## Task base commit

`6e1feab8662e111f873c086bc87adad170e0c36a` (main, after TASK-0065/v0.0.16).

## Requirements

Verbatim from the spec, each mapped to what/where:

- [x] Behavior-preserving throughout: full existing Go test suite passes
      unmodified; no exported API, schema, or query-result change. No test
      file was touched — every renamed/moved symbol stayed within
      `internal/storage`/`internal/config`, so no test needed to change its
      import or reference.
- [x] The service-replacement sequence is one shared implementation
      (`replaceServiceRows`, `internal/storage/services.go`) parameterized by
      the caller's current-IDs query/args and delete query/args; the three
      call sites (`ReplaceConfiguredServices`, `ReplaceRuntimeServices`, the
      success branch of `FinishScanWithRouteTargetChanges`) delegate to it
      inside their own already-open transaction. The repository-row
      upsert/update step (INSERT...ON CONFLICT DO UPDATE vs DO NOTHING vs
      FinishScan's plain UPDATE) is left in each caller, since its SQL shape
      genuinely differs per site and isn't part of the duplicated
      cleanup/canonicalization sequence itself.
      **Preserved divergence:** `replaceServiceRows`'s `pruneOrphanedStatus`
      parameter is `true` for `ReplaceConfiguredServices`/
      `ReplaceRuntimeServices` and `false` for
      `FinishScanWithRouteTargetChanges` — the latter still does not delete
      `status_results`/`status_history` for service IDs dropped from a
      successful scan's result. This is documented at the `false` call site
      (`internal/storage/services.go`, `FinishScanWithRouteTargetChanges`)
      and in `replaceServiceRows`'s doc comment. Per the spec's Out of scope,
      this divergence was not "fixed" — the refactor did not surface new
      evidence that it is a live leak, so no follow-up intake is warranted at
      this time.
- [x] Every `DELETE FROM status_results` / `DELETE FROM status_history` in
      `services.go` and `status.go` now goes through shared helpers in
      `internal/storage/sqlutil.go`:
      - Whole-service pair: `deleteStatusForService` — replaces the three
        copies (two `Replace*` orphan-cleanup loops via `replaceServiceRows`,
        and `PruneRuntimeServices`'s `removeIDs` loop).
      - By service+targets pair: the pre-existing `deleteByServiceAndTargets`
        — already used by `PruneStatusTargetsFromKnown`; `PruneStatusTargets`
        now also calls it (twice, once per table) instead of looping
        per-target single-row deletes, since it already collects the full
        `removeTargets` slice before deleting.
      - By service+target-prefix pair: `deleteStatusForServiceTargetPrefix`
        — replaces `SetMonitorNotApplicable`'s synthetic-route-parent branch.
      - Lone single-table helpers: `deleteStatusHistoryForServiceTarget`
        (the identical service+target statement duplicated in
        `SetMonitorNotApplicable` and `UpsertStatus`) and
        `deleteStatusHistoryBefore` (the age-cutoff prune in
        `PruneStatusHistory`, which still runs outside a transaction — its
        helper takes a `sqlExecutor` interface satisfied by both `*sql.Tx`
        and `*sql.DB`).
      `rg -n 'DELETE FROM status_(results|history)' internal/storage/services.go internal/storage/status.go`
      prints nothing (verified below).
- [x] `PruneStatusTargets` and `PruneStatusTargetsFromKnown` share one
      removal/commit/alert tail, `commitStatusTargetPrune`
      (`internal/storage/status.go`): delete via `deleteByServiceAndTargets`
      (both tables) within the caller's already-open transaction, commit,
      then `observeHealthAlert`. Each function's own gather logic (exclusion
      consultation + `keepExact` for `PruneStatusTargetsFromKnown`; plain
      per-target/prefix scan for `PruneStatusTargets`) and its own
      transaction-open point are unchanged — only the post-gather
      delete/commit/alert sequence moved into the shared tail.
- [x] The canonicalize* triplet and migrateRoute* twins
      (`internal/storage/route_targets.go`) are driven by shared
      implementations parameterized by table/columns/merge rule:
      - `canonicalizeRouteAliasRows[T any]` is the shared gather core for
        `canonicalizeMonitorOverrides`, `canonicalizeStatusResults`, and
        `canonicalizeStatusHistory` — select rows matching the target
        prefix, keep only those whose `routetarget.CanonicalTargetForName`
        differs, hand each surviving alias to the caller's `apply` closure.
      - `applyMonitorOverrideMigration` and `applyStatusResultMigration` are
        each the single update-or-merge-then-delete rule for their table,
        shared by *both* the migrate twin and the canonicalize triplet
        member for that table (`migrateRouteOverride`/
        `canonicalizeMonitorOverrides` share `applyMonitorOverrideMigration`;
        `migrateRouteStatus`/`canonicalizeStatusResults` share
        `applyStatusResultMigration`) — a tighter reuse than the spec
        required (it asked for the triplet and the twins to each have shared
        implementation(s); this shares the actual per-row merge rule across
        both families since they turned out to be the same operation
        applied to a single explicit pair vs. a gathered batch).
      - `status_history` has no uniqueness constraint on
        (service_id, target), so its canonicalize path stays a plain
        rename-only `UPDATE` (no merge-then-delete needed) — this asymmetry
        with the other two tables is inherent to the schema, not something
        to normalize away.
      Per-table merge semantics are unchanged (max-of-both-fields for
      `monitor_overrides`; `shouldReplaceCanonicalStatus` for
      `status_results`, unchanged); existing route_targets tests (T-031
      continuity) pass unmodified.
- [x] The full twelve-function alert-redaction family enumerated in the
      spec's Context item 5 moved from `internal/config/config.go` to
      `internal/config/alert_redaction.go`: `appendAlertRedactValues`,
      `appendAlertURLRedactValues`, `appendAlertHeaderRedactValues`,
      `AlertingRedactionValues`, `alertURLRedactionValues`,
      `appendAlertURLTrailingPathSecretValues`,
      `appendAlertDeclaredURLSecretValues`, `alertEscapedPathVariants`,
      `appendAlertURLQueryRedactValues`, `alertEscapedSecretVariants`,
      `alertHeaderRedactionValues`, `isAlertSecretParameterName`. Pure move:
      no signature or behavior change. `resolveAlertingSecrets` stayed in
      `config.go` and still calls the moved functions (unqualified — same
      package). `internal/app/app.go`'s call to the exported
      `config.AlertingRedactionValues` is unaffected. `net/url`,
      `net/textproto`, and `strings` stayed in `config.go` (used elsewhere in
      the file); the `internal/sanitizer` import moved to the new file, since
      after the move it was only referenced from
      `alertURLRedactionValues`.
- [x] Net non-test Go line count in `internal/storage` decreased relative to
      the task base commit: **before=8420, after=8389 (delta -31)**. Measured
      by the spec's exact Verification commands (see below). Per-file deltas:
      `services.go` -27, `route_targets.go` -39, `status.go` -8,
      `sqlutil.go` +43 (five new required helpers), all other files
      unchanged.

## Out of scope

Not attempted, per the spec: fixing the `FinishScanWithRouteTargetChanges`
orphaned-status divergence (documented above, not changed — the refactor did
not surface evidence it's a live leak); schema/migration changes; the
alerter dispatch path (TASK-0065); the docker inspection refactor
(TASK-0046, external T-046); performance changes. The
`internal/storage/migrations.go` split proposed by the original survey
finding (F10) was dropped from this task at spec review 2026-07-24 — its
repair functions (`repairAlertEventsSchema`, `repairAlertDispatchesSchema`,
`ensureAlertIndexes`, `rebuildAlertTables`) are migration-step
implementations reached only through the migration framework at store open,
not runtime self-healing code, so `migrations.go` was left untouched.

## Verification

```sh
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build ./...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage -count=1
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1

# no inline status-table deletes outside the sqlutil.go helpers (pass = prints nothing)
rg -n 'DELETE FROM status_(results|history)' internal/storage/services.go internal/storage/status.go && exit 1 || true

# redaction-family DEFINITIONS fully out of config.go (pass = prints nothing)
rg -n 'func (AlertingRedactionValues|appendAlertRedactValues|appendAlertURLRedactValues|appendAlertHeaderRedactValues|alertURLRedactionValues|appendAlertURLTrailingPathSecretValues|appendAlertDeclaredURLSecretValues|appendAlertURLQueryRedactValues|alertEscapedPathVariants|alertEscapedSecretVariants|alertHeaderRedactionValues|isAlertSecretParameterName)\(' internal/config/config.go && exit 1 || true
rg -l 'func AlertingRedactionValues' internal/config

# net non-test Go lines in internal/storage decreased vs the task base commit
base=6e1feab8662e111f873c086bc87adad170e0c36a
before=$(git ls-tree -r --name-only "$base" internal/storage | rg '\.go$' | rg -v '_test\.go$' | while read -r f; do git show "$base:$f"; done | wc -l)
after=$(find internal/storage -name '*.go' ! -name '*_test.go' -exec cat {} + | wc -l)
echo "storage non-test lines: before=$before after=$after"
test "$after" -lt "$before"
```

All commands pass. Also run and green: `make check` (format, `go vet`, full
Go test suite including `go test ./cmd/... ./internal/...`, UI
typecheck/lint/build, 16/16 Playwright e2e, release test).

## Maintainability sweep

- `applyMonitorOverrideMigration`/`applyStatusResultMigration` are now the
  single source of truth for their table's rename-or-merge-then-delete rule,
  shared by both the explicit-pair migrate path and the bulk canonicalize
  path — a bug fix in one no longer risks silently missing the other, which
  was exactly the survey's original concern (Context item 1/4).
- `sqlExecutor` (an interface satisfied by both `*sql.Tx` and `*sql.DB`)
  lets `deleteStatusHistoryBefore` serve `PruneStatusHistory`'s
  outside-a-transaction call without a separate non-transactional variant.
- No unrelated refactors: `internal/storage/migrations.go`,
  `internal/storage/alerts.go`, `internal/storage/storage.go`, and
  `internal/storage/redaction.go` (an unrelated storage-layer output
  redaction registry, not the config alert-redaction machinery this task
  moved) were not touched.

## Commit evidence

Task base commit: `6e1feab8662e111f873c086bc87adad170e0c36a`

Proposed commit subjects:
- `refactor(storage): share the service-replacement skeleton across scan and sync paths (T-066)`
- `refactor(storage): route status-table deletes through shared helpers (T-066)`
- `refactor(storage): share the route-canonicalization and migration merge rules (T-066)`
- `refactor(config): move alert redaction machinery to its own file (T-066)`
- `docs(tasks): add TASK-0066 record (T-066)`
