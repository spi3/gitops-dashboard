# TASK-0068: Bound scans table read cost and growth: evaluator index + terminal-row retention

## Status

Proposed

## Scope

`internal/storage` (migration framework, `scans` query/prune helpers) and
`internal/scanner` (zero-config retention maintenance wiring). Satisfies
survey 2026-07-25 IR-6 (F8).

## Dependencies

TASK-0066 (ordering only: both touch `internal/storage`; no scope overlap).

## Task base commit

`bb79ad41b57502341bf4fb0a6d772f8a8e8a6a56` (main, after TASK-0067/v0.0.18).

## Requirements

Verbatim from the spec, each mapped to what/where:

- [x] The per-repository latest-terminal-scan lookup
      (`LatestTerminalScansByRepository`) is served by an index: a Go test
      asserts via `EXPLAIN QUERY PLAN` that no access to `scans` in that
      query's plan is a full-table scan.
      `TestLatestTerminalScansByRepositoryQueryPlanUsesIndex`
      (`internal/storage/scans_retention_test.go`) runs `EXPLAIN QUERY PLAN`
      against the exact SQL text
      `latestTerminalScansByRepositoryQuery` (`internal/storage/services.go`)
      builds — the same function `LatestTerminalScansByRepository` calls, so
      the test cannot drift from production SQL — and fails if any plan step
      contains `SCAN` without `USING`. Query results are unchanged: no
      existing `TestLatestTerminalScansByRepository*` test was modified, and
      all pass unmodified against the new index.
- [x] The index is added through the existing ordered-step migration
      framework, applies to both fresh and existing supported databases at
      store open, and is reconciled by name if a same-named index with a
      different definition already exists (alert-index precedent).
      `idx_scans_repository_status_id` on `scans(repository, status, id)`,
      added as migration step `020_scans_indexes`
      (`internal/storage/migrations.go`, `migrationSteps`), applying
      `store.ensureScanIndexes`. The alert-index reconciliation machinery
      (`alertIndexSpec`/`alertIndexMatchesTx`/`alertIndexFlagsTx`/
      `alertIndexColumnsTx`/`normalizeAlertIndexWhere`) was generalized in
      place (renamed to `indexSpec`/`indexSpecMatchesTx`/`indexFlagsTx`/
      `indexColumnsTx`/`normalizeIndexWhere`, with the reconcile loop
      factored into `reconcileIndexes`) rather than duplicated, so
      `scanIndexSpecs`/`ensureScanIndexes` reuse the exact same
      drop-and-recreate-by-name logic the alert indexes already use — no
      behavior change to the alert index path (`ensureAlertIndexDefinitions`
      now just calls `reconcileIndexes(ctx, alertIndexSpecs())`).
      `TestScanIndexAppliesToFreshDatabase` and
      `TestScanIndexMigrationRepairsSameNamedIndexWithDifferentDefinition`
      (mirroring `TestAlertIndexMigrationRepairsSameNamedFullUniqueIndex`)
      cover fresh-open and same-named-stale-index reconciliation
      respectively.
- [x] Terminal scan rows (status ok or error) older than a retention horizon
      are deleted periodically during normal server operation with no
      operator action, in bounded batches inside transactions. Rows with
      non-terminal status are never deleted by retention.
      `Store.PruneTerminalScans` (`internal/storage/services.go`) deletes in
      batches of `DefaultScanRetentionBatchSize` (500, `PruneTerminalAlertEvents`
      precedent) via `id IN (SELECT id ... ORDER BY id LIMIT ?)`, retrying on
      SQLite busy (`retryAlertSQLiteBusy`, reused from `internal/storage/alerts.go`
      — already table-agnostic despite the name) and looping until a batch
      returns fewer than `batchSize` rows; each `DELETE` is its own
      transaction (SQLite's implicit per-statement transaction), matching
      `PruneTerminalAlertEvents`'s shape exactly. `status IN ('ok','error')`
      is a hard filter in the delete's inner `SELECT`, so a `running` row is
      never a deletion candidate regardless of age.
      `Scanner.runScanRetentionMaintenance`
      (`internal/scanner/scanner.go`), started unconditionally from
      `RunScheduled`, prunes once at startup and then hourly
      (`scanRetentionMaintenanceInterval`), with zero required
      configuration — mirrors `Monitor.runStatusHistoryMaintenance`'s
      zero-config periodic-prune shape rather than the alerter's
      config-driven retention loop, since the spec requires defaults to work
      with no new configuration. It runs even with zero configured
      repositories, so scan rows left behind by a repository since removed
      from config are still bounded.
      `TestPruneTerminalScansRemovesOldTerminalRowsOnly` and
      `TestPruneTerminalScansBoundsBatchSize` cover terminal-only/
      running-untouched and multi-batch pruning respectively.
- [x] Hard invariant: retention never deletes the newest terminal scan row
      of any repository, regardless of age —
      `LatestTerminalScansByRepository` returns identical results
      immediately before and after any prune. A test covers a repository
      whose ONLY terminal row is far older than the horizon.
      The delete's candidate subquery excludes
      `id IN (SELECT MAX(id) FROM scans WHERE status IN ('ok','error') GROUP BY repository)`
      — exactly the set `LatestTerminalScansByRepository` itself selects, so
      the two queries can never disagree about which row is "latest."
      `TestPruneTerminalScansNeverDeletesNewestTerminalRowPerRepository`
      captures `LatestTerminalScansByRepository` before and after a prune
      across a mixed dataset (`reflect.DeepEqual`), and includes
      `repo-only-ancient-terminal`: a repository whose single terminal row
      is dated 2020-01-01 against a `2026-06-01`-anchored horizon — it
      survives, and the evaluator baseline for it is unchanged by the prune.
      `TestScansPreChangeFixtureOpensMigratesAndPrunes` re-asserts the same
      invariant end to end against a migrated pre-existing database.
- [x] The default horizon is at least 7 days, chosen and documented here; a
      config knob is optional and none was added.
      `DefaultScanRetentionHorizon = 7 * 24 * time.Hour`
      (`internal/storage/services.go`) — see "Retention horizon" below.
      `TestDefaultScanRetentionHorizonMeetsSevenDayFloor` pins the floor.
- [x] Pre-change fixture (standing invariant 3): a database created by the
      current release with a populated scans table (running + ok + error
      rows across multiple repositories) opens, migrates (gains the index),
      operates normally, and prunes per the rules above; scan history
      visible in the summary and the evaluator's baseline results are
      preserved.
      `TestScansPreChangeFixtureOpensMigratesAndPrunes`
      (`internal/storage/scans_retention_test.go`) builds a raw
      pre-T-068-schema `scans` table (no index) with two repositories'
      running/ok/error rows via direct SQL, bypassing the Go API entirely,
      then opens it with `Open`: asserts the index was gained, the
      evaluator baseline and `Scans()` summary (all 4 rows) are visible
      pre-prune, and both are preserved (`reflect.DeepEqual` on the
      baseline) after `PruneTerminalScans` removes only the one superseded
      old terminal row.
- [x] Behavior elsewhere unchanged aside from cost: full Go test suite
      green; T-064's evaluator tests pass unmodified.
      See Verification below — no existing test file was modified except
      the query-builder extraction in `services.go`, which is a pure
      internal refactor with identical SQL text (verified by the query-plan
      test running that exact text).
- [x] In-repo record: this file, plus a `docs/tasks/tracker.md` row.

## Out of scope

Not attempted, per the spec: rewriting the `Scans()` 50-row summary query;
changing scan lifecycle or status semantics; pruning non-terminal (`running`)
rows; `alert_events`/`alert_dispatches` retention (exists,
`PruneTerminalAlertEvents`); `status_history` retention (exists,
`PruneStatusHistory`); any performance work beyond the scans table.

## Retention horizon

`DefaultScanRetentionHorizon = 7 * 24 * time.Hour`, matching
`status_history`'s existing `statusHistoryWindow` constant
(`internal/storage/storage.go`) exactly. Seven days keeps the dashboard's
50-row `Scans()` summary window populated at every scan interval the example
configs use (30s–5m): even a single repository scanning every 5 minutes
produces ~2,016 terminal rows in 7 days, far more than the 50-row window
needs, and the newest-row invariant means a repository that scans less often
than once every 7 days still always has its one true baseline row available
to the evaluator regardless of how far past the horizon it falls. No config
knob was added — the spec marks it optional, and a fixed default matches the
existing `status_history`/alert-index precedent of needing no operator
action.

## Newest-terminal-row invariant — how it is enforced and tested

`LatestTerminalScansByRepository` selects, per repository, the row with
`status IN ('ok','error') AND id = (SELECT MAX(id) FROM scans WHERE
repository=? AND status IN ('ok','error'))`. `PruneTerminalScans`'s deletion
candidate set explicitly subtracts
`id IN (SELECT MAX(id) FROM scans WHERE status IN ('ok','error') GROUP BY
repository)` — the same aggregate, computed once per prune batch instead of
per repository, but selecting exactly the same id set. Because both queries
derive "latest terminal row" from the identical `MAX(id) WHERE status IN
('ok','error')` rule, a row excluded from deletion by the prune's `NOT IN`
clause is, by construction, always the same row `LatestTerminalScansByRepository`
would return for that repository — there is no way for the two to disagree.
This holds regardless of the row's age (the horizon cutoff is a separate,
independently-applied `AND started_at < ?` filter), so a repository whose
only terminal row predates the horizon by years is still protected.

Tested by `TestPruneTerminalScansNeverDeletesNewestTerminalRowPerRepository`
(direct before/after `reflect.DeepEqual` on
`LatestTerminalScansByRepository` across a multi-repository dataset
including a repository with only one, far-older-than-horizon terminal row)
and reinforced end-to-end by `TestScansPreChangeFixtureOpensMigratesAndPrunes`.

## Query plan — before / after

Before (no index, synthetic schema/query matching production exactly):

```
QUERY PLAN
|--SCAN s
`--CORRELATED SCALAR SUBQUERY 1
   `--SEARCH s2
```

The outer `SCAN s` is a full table scan of `scans`; this is the cost the
survey measured (0.72s warm at 500k rows / 5 repos, growing linearly).

After (`idx_scans_repository_status_id` on `scans(repository, status, id)`):

```
QUERY PLAN
|--SEARCH s USING INDEX idx_scans_repository_status_id (repository=? AND status=?)
`--CORRELATED SCALAR SUBQUERY 1
   `--SEARCH s2 USING COVERING INDEX idx_scans_repository_status_id (repository=? AND status=?)
```

Both the outer query and the correlated `MAX(id)` subquery now use the
index; neither performs a full scan of `scans`.
`TestLatestTerminalScansByRepositoryQueryPlanUsesIndex` asserts this
programmatically against the live index.

## Verification

```sh
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build ./...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/app -count=1
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
# retention exists in production code (pass = at least one match).
rg -n 'DELETE FROM scans' internal/storage -g '!*_test.go'
# the mandated query-plan assertion test exists (pass = nonempty):
rg -in 'EXPLAIN QUERY PLAN' internal/storage --glob '*_test.go'
```

All commands pass; see the Definition-of-done report for observed output.
Also run and green: `GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local
go vet ./...` and `make check`.

## Commit evidence

Task base commit: `bb79ad41b57502341bf4fb0a6d772f8a8e8a6a56`
