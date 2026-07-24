# TASK-0061: Repository synchronization truthfulness and origin reconciliation

## Status

Proposed

## Scope

`internal/scanner`, production and test code only. This is the homelab
replacement for the correctness portion of external T-040 (homelab
re-triage 2026-07-12; T-040 is superseded). Unchanged-commit parsing
optimization and parser caching remain cancelled and out of scope.

## Dependencies

TASK-0060 (`docs/tasks/TASK-0060-credential-and-build-context-safety.md`).
This task consumes T-060's `credentialFreeRemoteURL` and
`scrubCachedOriginCredentials` helpers as-is; it does not create another URL
sanitizer or weaken T-060's scrub-before-credential-validation ordering.

## Task-specific acceptance criteria

- R-2: preflight/synchronization/HEAD failures persist truthful scan rows,
  related origins reconcile, diverged origins fail visibly, and caches
  outside the owned cache root are untouched.
- N-1: all Git subprocess work per synchronization attempt stays bound
  (`repoSyncGitCommands`/`repoSyncOperationTimeout`, see
  `internal/scanner/scanner.go`).
- N-2: T-060 credential safety is preserved — no new URL sanitizer, no
  weakened scrub ordering, and secrets never appear in errors, logs,
  configuration, or persisted scan fields.
- N-3: pre-change cache fixtures back every new behavior (external symlink,
  fast-forward origin change, diverged origin).
- N-4: exact evidence — 12 named tests plus this record's E2E/Verification
  sections.

Concretely:

- [x] Gated implementation on the exact T-060 landing contract; recorded the
      landed T-060 commit as this task's base (see T-060 landing evidence).
- [x] `scanOneUnshared` now calls `Store.StartScan` before any cache-root
      absolute/symlink resolution, candidate resolution, or coalescing-key
      work that can fail. A root-preflight failure finalizes that row as
      terminal `error` (`TestScanAllPersistsRootPreflightFailure`).
- [x] `Scanner.resolveRepoCacheRoot` resolves `server.repoCacheDir` with
      `filepath.Abs`, then `os.MkdirAll`, then `filepath.EvalSymlinks`. For an
      existing expected repository path, `resolveContainedRepoPath` resolves
      the candidate with `filepath.EvalSymlinks`
      (`internal/scanner/credentials.go`).
- [x] `containedRel` computes `filepath.Rel(resolvedRoot, resolvedCandidate)`
      and accepts only a non-absolute, non-`.`, non-`..`,
      non-`..`+separator-prefixed relative path; equality with the root
      (`rel == "."`) is invalid. No string-prefix comparison is used anywhere.
- [x] A missing, non-symlink candidate is valid and proceeds to clone under
      the resolved root; an existing candidate that fails to resolve
      (including dangling/cyclic symlinks) fails the started scan.
- [x] Containment completes before every Git command or repository mutation:
      T-060 scrub, origin enumeration/reconciliation, fetch, checkout, pull,
      clone destination creation, and `rev-parse HEAD` all run after
      `resolveContainedRepoPath` returns successfully.
- [x] `TestExternalCacheSymlinkIsUntouched`: the expected cache path is a
      symlink to a valid external repository; the scan fails before any Git
      command touches it (asserted via the git-exec seam spy), and origin
      fetch/push lists, HEAD, `.git/index` bytes, `git status --porcelain`,
      and a representative worktree file are byte-for-byte unchanged.
- [x] `reconcileOrigin` enumerates fetch URLs with exactly `git config
      --get-all remote.origin.url` (via the existing T-060 `gitConfigGetAll`,
      unmodified exit-code semantics), applies `credentialFreeRemoteURL` to
      the configured URL and to the enumerated value, and requires exactly
      one fetch URL exactly equal to the configured credential-free URL.
- [x] On mismatch, `reconcileOrigin` replaces the entire fetch URL list with
      exactly one configured credential-free URL via `git config --unset-all`
      (only if a prior value existed) then `git config --add`; push URLs,
      already scrubbed by T-060, are never touched.
- [x] Every fetch, checkout, fast-forward/pull, and `rev-parse HEAD` failure
      is returned. The `DefaultRef: HEAD` exception around `git pull
      --ff-only` is removed: pull failure always fails synchronization now
      (`TestScanAllPersistsFastForwardFailureForHEAD`).
- [x] Fixture (`createOriginFixtureSourceA` / `...SourceBFastForward`): branch
      `main`, file `docker-compose.yml`, source A commit subject `fixture:
      source A baseline` and service `source-a`; source B is cloned from A,
      keeps the same branch/history, and adds a strict descendant commit
      `fixture: source B fast-forward` changing the service to `source-b`.
      `TestScanAllReconcilesFastForwardOrigin` records runtime `A_SHA`/`B_SHA`
      and asserts `git merge-base --is-ancestor $A_SHA $B_SHA` and
      `A_SHA != B_SHA`.
- [x] Same test: populates the cache from A, reconfigures to B, preserves one
      manually added safe push URL, and proves no reclone via a marker file
      inside `.git/`. Reconciliation leaves exactly B as the fetch URL,
      preserves the push URL, fast-forwards `HEAD` to `B_SHA`, persists
      `B_SHA`, and the only exposed service is `source-b`.
- [x] Diverged fixture (`createOriginFixtureSourceBDiverged`): independent
      history on branch `main`, commit subject `fixture: source B diverged`,
      file `docker-compose.yml`, service `source-b-diverged`, so it can never
      contain A's commit as an ancestor.
      `TestScanAllDivergedOriginFailsTruthfully` reconciles the URL, then
      `git pull --ff-only` fails truthfully; the persisted error row keeps
      the prior successful `A_SHA`, the prior `source-a` service is retained,
      and the cache is not recloned, reset, merged, or force-checked-out
      (proven via an unchanged `.git/index`, unchanged `HEAD`, and a
      survives-the-attempt marker file).
- [x] For fetch, checkout, `DefaultRef: HEAD` fast-forward, `rev-parse HEAD`,
      origin-read, origin-write, root-preflight, and diverged-origin
      failures: every test asserts a returned error, newest persisted row
      status `error`, failed-row `CommitSHA` equal to the prior successful
      SHA (or empty when none), summary `LastCommit` equal to the same
      value, and unchanged prior services
      (`assertTerminalFailedScan`/`assertServiceNames` in
      `internal/scanner/scanner_test.go`). "No `ok` row for the attempt" is
      structural: `FinishScanWithRouteTargetChanges` updates the single
      `StartScan` row in place.
- [x] Added the narrow private Git-command seam
      (`internal/scanner/git_exec.go`): `gitExecFunc`, backed by
      `productionGitExec` in production, held in a `sync/atomic.Value` behind
      `invokeGit`. `gitOutput`, `gitConfigGetAll`, and `gitConfigCommand` all
      route through it exclusively.
- [x] Added the 12 exact tests (see Verification).
- [x] Retained every T-060 credential-safety assertion; consumed
      `scrubCachedOriginCredentials`/`credentialFreeRemoteURL` as-is; did not
      reimplement URL sanitizing.
- [x] Did not add unchanged-commit skipping, parser caching, cache-directory
      renaming, reclone-on-mismatch, reset/merge recovery, a migration
      framework, or storage APIs. `priorSuccessfulCommit` reads via the
      existing `Store.Repositories`; scan persistence still goes exclusively
      through `Store.StartScan` and `Store.FinishScanWithRouteTargetChanges`.
- [x] Added this task record.
- [x] Recorded T-060's landed commit as `Task base commit`, plus a proposed
      T-061 commit subject and PR title (see Commit evidence).
- [x] Added the `TASK-0061` tracker row and kept `Next Task ID` at
      `TASK-0065`.
- [x] Recorded exact A/B branch, subjects, SHAs, file, services, origins,
      persisted scans, and inventory in the E2E plan/Observed results below.

## Out of scope

Unchanged-repository optimization, parser-result caching, hostile contents
inside a valid owned repository, cache naming migration, recloning or
resetting a recoverable cache, automatic divergence repair, storage schema
changes, and filesystem hardening outside the cache boundary.

## E2E plan

1. **Root preflight**: scan a repository successfully once (populating
   `A_SHA` and service `source-a`), then replace the configured cache root
   with a plain file so `os.MkdirAll` fails. Expect: `ScanAll` returns an
   error; the newest scan row is `error` with `CommitSHA = A_SHA`; the prior
   `source-a` service is retained.
2. **External symlink containment**: point the expected cache path at a
   symlink to a separate, valid, unrelated repository outside the cache
   root. Expect: the scan fails before any Git command targets the external
   repository's real path (spied via the git-exec seam); its origin
   fetch/push lists, `HEAD`, `.git/index`, `git status --porcelain`, and a
   representative worktree file are unchanged.
3. **Origin enumeration/reconciliation**: clone from a source, then manually
   unset `remote.origin.url` entirely (an exit-1/empty-stdout enumeration).
   Expect: reconciliation repairs it to exactly the configured URL and the
   scan succeeds. Separately, using the git-exec seam, fail only the
   enumeration call (leaving T-060's own scrub enumeration of the same key
   untouched) and, in another run, fail only the reconciliation write after
   forcing a mismatch. Expect both: the error is returned, `fetch` never
   runs, and the prior successful state (`A_SHA`, `source-a`) is retained.
4. **Fetch/checkout/fast-forward/rev-parse failures**: after one successful
   scan against source A, use the git-exec seam to fail exactly one
   downstream command per test (`fetch`, `checkout`, `pull` — including with
   `DefaultRef: HEAD`, which previously silently ignored a pull failure —
   and `rev-parse`). Expect each: the error is returned, the newest row is
   `error` with `CommitSHA = A_SHA`, summary `LastCommit = A_SHA`, and
   `source-a` is retained.
5. **Fast-forward origin change**: branch `main`, file
   `docker-compose.yml`. Source A: commit subject `fixture: source A
   baseline`, service `source-a`. Source B: cloned from A, adds strict
   descendant commit `fixture: source B fast-forward`, service `source-b`.
   Populate the cache from A (`A_SHA`, `source-a` persisted), add a safe
   push URL and a no-reclone marker file, reconfigure to B, and rescan.
   Expect: `git merge-base --is-ancestor $A_SHA $B_SHA` holds and `A_SHA !=
   B_SHA`; no reclone (marker survives); fetch URL is exactly B; push URL
   preserved; `HEAD` fast-forwards to `B_SHA`; persisted commit is `B_SHA`;
   only `source-b` is exposed.
6. **Diverged origin**: branch `main`, file `docker-compose.yml`, source
   B-diverged is an independent history with commit subject `fixture: source
   B diverged`, service `source-b-diverged`. Populate the cache from A,
   reconfigure to B-diverged, rescan. Expect: the URL is reconciled to
   B-diverged, but `git pull --ff-only` fails; the persisted error row keeps
   `A_SHA`; `source-a` is retained; the cache is not recloned, reset,
   merged, or force-checked-out (unchanged `HEAD`, unchanged `.git/index`,
   surviving marker file).

## Observed results

- Root preflight failure after a prior success: newest scan `error`,
  `CommitSHA` and summary `LastCommit` both equal the prior `A_SHA`;
  `source-a` retained (`TestScanAllPersistsRootPreflightFailure`).
- External symlink: scan fails with "repository fixture cache path escapes
  the repository cache directory"; the git-exec spy recorded zero
  invocations against the external repository's path; every external-repo
  snapshot (HEAD, remotes, worktree status, `.git/index` bytes, a
  representative file) was byte-identical before and after
  (`TestExternalCacheSymlinkIsUntouched`).
- Origin enumeration empty-list repair: a cache with `remote.origin.url`
  fully unset repairs to exactly the configured URL and scans successfully
  (`TestOriginEnumerationEmptyListRepairs`).
- Origin enumeration read failure and reconciliation write failure both stop
  before `fetch` runs, in each case leaving the prior successful `A_SHA` and
  `source-a` in place (`TestOriginEnumerationReadFailureStopsBeforeFetch`,
  `TestOriginReconciliationWriteFailureStopsBeforeFetch`).
- Fetch, checkout, fast-forward (including the `DefaultRef: HEAD` case that
  previously silently swallowed this failure), and rev-parse failures all
  persist a truthful `error` row carrying the prior `A_SHA`, with
  `source-a` retained
  (`TestScanAllPersistsFetchFailure`/`CheckoutFailure`/`FastForwardFailureForHEAD`/`RevParseFailure`).
- Fast-forward origin reconciliation: observed `A_SHA` and `B_SHA` differed
  and `B_SHA` was a strict descendant of `A_SHA`; after reconfiguring to
  source B, the cache was not recloned (marker file survived), the fetch
  URL list collapsed to exactly source B, the manually added push URL
  survived untouched, `HEAD` advanced to `B_SHA`, and only `source-b` was
  exposed (`TestScanAllReconcilesFastForwardOrigin`).
- Diverged origin: URL reconciled to source B-diverged, but `git pull
  --ff-only` failed with "Not possible to fast-forward, aborting."; the
  persisted row was `error` with `CommitSHA = A_SHA`; `HEAD` and
  `.git/index` were unchanged from before the attempt; `source-a` remained
  the only exposed service (`TestScanAllDivergedOriginFailsTruthfully`).
- `repoSyncGitCommands` is documented and asserted at `7` (origin
  enumeration, reconciliation unset-all+add, fetch, checkout, fast-forward
  pull, HEAD resolution), with `repoSyncOperationTimeout` derived from it
  (`TestRepositorySyncOperationBudgetIncludesOriginReconciliation`).
- The multi-fetch-URL pre-change fixture from T-060
  (`TestSyncRepoScrubsMultipleCachedOriginURLsBeforeNetworkAttempt`) now also
  observes T-061 reconciliation collapsing three cached fetch URLs to the
  single configured one before the (still-failing) network fetch attempt;
  push URL scrub behavior is unchanged.

## Verification

All commands were run from the worktree.

```text
$ test -f docs/tasks/TASK-0060-credential-and-build-context-safety.md && echo present
present
$ rg -n 'func credentialFreeRemoteURL|func scrubCachedOriginCredentials' internal/scanner
internal/scanner/credentials.go:33:func credentialFreeRemoteURL(raw string) (clean string, stripped bool, err error) {
internal/scanner/credentials.go:136:func scrubCachedOriginCredentials(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string) error {
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/scanner -run '^(TestCachedOriginScrubPrecedesCredentialValidation|TestScrubCachedOriginCredentialsAllFetchAndPushURLs)$' -count=1
ok  	github.com/example/gitops-dashboard/internal/scanner	0.064s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/scanner -run "^(Test(ScanAllPersistsRootPreflightFailure|ExternalCacheSymlinkIsUntouched|OriginEnumerationEmptyListRepairs|OriginEnumerationReadFailureStopsBeforeFetch|OriginReconciliationWriteFailureStopsBeforeFetch|ScanAllPersistsFetchFailure|ScanAllPersistsCheckoutFailure|ScanAllPersistsFastForwardFailureForHEAD|ScanAllPersistsRevParseFailure|ScanAllReconcilesFastForwardOrigin|ScanAllDivergedOriginFailsTruthfully|RepositorySyncOperationBudgetIncludesOriginReconciliation))$" -count=1 -v
--- PASS: TestScanAllPersistsRootPreflightFailure (0.10s)
--- PASS: TestExternalCacheSymlinkIsUntouched (0.08s)
--- PASS: TestOriginEnumerationEmptyListRepairs (0.16s)
--- PASS: TestOriginEnumerationReadFailureStopsBeforeFetch (0.10s)
--- PASS: TestOriginReconciliationWriteFailureStopsBeforeFetch (0.11s)
--- PASS: TestScanAllPersistsFetchFailure (0.11s)
--- PASS: TestScanAllPersistsCheckoutFailure (0.14s)
--- PASS: TestScanAllPersistsFastForwardFailureForHEAD (0.12s)
--- PASS: TestScanAllPersistsRevParseFailure (0.15s)
--- PASS: TestScanAllReconcilesFastForwardOrigin (0.23s)
--- PASS: TestScanAllDivergedOriginFailsTruthfully (0.20s)
--- PASS: TestRepositorySyncOperationBudgetIncludesOriginReconciliation (0.00s)
ok  	github.com/example/gitops-dashboard/internal/scanner	1.506s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/scanner -count=1
ok  	github.com/example/gitops-dashboard/internal/scanner	2.605s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.024s
ok  	github.com/example/gitops-dashboard/internal/alerter	0.287s
ok  	github.com/example/gitops-dashboard/internal/app	1.275s
ok  	github.com/example/gitops-dashboard/internal/auth	0.010s
ok  	github.com/example/gitops-dashboard/internal/ci	2.321s
ok  	github.com/example/gitops-dashboard/internal/config	0.030s
ok  	github.com/example/gitops-dashboard/internal/core	0.005s
ok  	github.com/example/gitops-dashboard/internal/dockerapi	0.014s
ok  	github.com/example/gitops-dashboard/internal/environment	0.026s
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	6.177s
ok  	github.com/example/gitops-dashboard/internal/parser	0.015s
ok  	github.com/example/gitops-dashboard/internal/routetarget	0.013s
ok  	github.com/example/gitops-dashboard/internal/sanitizer	0.005s
ok  	github.com/example/gitops-dashboard/internal/scanner	3.017s
ok  	github.com/example/gitops-dashboard/internal/storage	7.017s
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]
```

Also confirmed clean with the race detector: `go test ./internal/scanner/... -race -count=3` passed with no data races.

```text
$ make check
...
16 passed (18.1s)
bash scripts/release_test.sh
ok  	github.com/example/gitops-dashboard/cmd/release	(cached)
release binary clean-clone invocation passed
```

`make check` ran gofmt, the UI build/lint/typecheck, `go vet`, the full Go
test suite (including `internal/scanner`), Playwright (16/16), and
`scripts/release_test.sh`, all against a clean clone checkout, all green.

```text
$ git diff --check
```

No output; exited 0.

## Documentation sweep

- `docs/vision.md`: reviewed; no change. Product vision is unaffected by
  scanner-internal truthfulness/containment hardening.
- `docs/requirements.md`: reviewed; no change. R-2/N-1..N-4 are task-local
  IDs defined in this record, distinct from the project ID space, per the
  same convention T-060 used.
- `docs/tech_stack.md`, `docs/implementation_plan.md`,
  `docs/task_acceptance_criteria.md`, `docs/deployment.md`,
  `docs/versioning.md`, `docs/discovery.md`: reviewed; no change.
- `docs/tasks/TASK-0060-credential-and-build-context-safety.md`: reviewed;
  no change. This task consumes its helpers as-is.
- `docs/tasks/tracker.md`: updated to add the `TASK-0061` row and keep
  `Next Task ID` at `TASK-0065` (the spec-review-approved value; not
  advanced by this task).

## Maintainability sweep

- The Git-command seam (`internal/scanner/git_exec.go`) is a single narrow
  file: one function type, one production implementation, one atomic
  holder, one call-through helper. `gitOutput`, `gitConfigGetAll`, and
  `gitConfigCommand` all route through it exclusively rather than each
  keeping its own `exec.Command` construction, removing duplication that
  existed before this task.
- `reconcileOrigin` replaces T-060's narrower, token-auth-only
  `reconcileConfiguredOrigin`: the new enumeration-based contract is a
  strict superset (it already leaves an origin unchanged when it matches,
  covering the old token-auth-sync case) rather than living alongside it as
  a second mechanism.
- `resolveContainedRepoPath`/`containedRel` in `internal/scanner/credentials.go`
  replace the old lexical-only `containedRepoPath`, keeping the
  containment/path-resolution surface in one place next to the T-060
  credential helpers it now runs immediately before.
- `syncResult` (path + commit) replaces a separate, later `rev-parse HEAD`
  call in the scan-orchestration path, so HEAD resolution is inside the same
  coalesced, bounded synchronization attempt as fetch/checkout/pull instead
  of a second, unbounded step layered on top.
- `repoOperationKey` is now a pure, non-I/O, non-fallible lexical key
  (`filepath.Join` of the configured — not resolved — cache dir and the
  repository's safe name); it no longer calls the fallible root-resolution
  path, which is the mechanism that lets a per-repository scan row exist
  before cache-root resolution can fail. It remains necessarily distinct
  from the raw repository name alone, because `repoScanFlights` and
  `repoSyncFlights` are process-wide singleflight groups that would
  otherwise coalesce unrelated `Scanner` instances (e.g. concurrent tests)
  sharing a repository name.
- No unrelated refactors were made beyond what the restructured
  scan-before-preflight ordering and origin-reconciliation contract
  required.

## T-060 landing evidence

- `docs/tasks/TASK-0060-credential-and-build-context-safety.md`: present.
- `credentialFreeRemoteURL` and `scrubCachedOriginCredentials`: present in
  `internal/scanner/credentials.go`.
- `TestCachedOriginScrubPrecedesCredentialValidation` and
  `TestScrubCachedOriginCredentialsAllFetchAndPushURLs`: both listed and
  passing (see Verification).

## Commit evidence

Task base commit: `3febca3b2fd7bb47079714cab55cceb463f779a8`

Proposed commit subject: `feat(scanner): make repository sync failures and origin drift truthful (T-061)`

Proposed PR title: `feat(scanner): make repository sync failures and origin drift truthful (T-061)`
