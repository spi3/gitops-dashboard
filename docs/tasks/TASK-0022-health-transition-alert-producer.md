# TASK-0022: Health-transition alert event producer

## Scope

Emit alert-store events for stable service-rollup health edges. Target status
rows remain the canonical monitor input; after each upsert, the producer
aggregates the service's targets using the existing not-applicable exclusion
rule. Events therefore represent actionable service incidents rather than an
event per monitor target.

The first observed rollup is stored as a silent baseline. Later rollup changes
must persist for `alerting.stabilitySamples` consecutive samples (default 2).
The producer records its stable state and failure start alongside the alert
state so recovery duration survives restarts; this is the durable rollup
counterpart to the status-history observations that establish the edge.

## Out of Scope

- Alert delivery workers and sink transports (T-024).
- Agent and scan event producers (T-023).
- Changing monitor health aggregation or not-applicable semantics.

## E2E Plan

1. Enable a sink and submit baseline, failing, and recovery status samples.
2. Verify one transition event appears only after the stability window and a
   recovery event includes the recorded failure duration.
3. Repeat stable samples, flap before the window, mark a target
   not-applicable, and repeat an edge within cooldown; verify no extra event.
4. Start without alerting configuration and verify status writes create no
   alert rows.

## Observed Results

Focused storage tests exercised baseline silence, stable edge emission,
steady-state silence, stability-window flapping, cooldown suppression,
not-applicable exclusion, recovery duration, disabled alerting, legacy
in-flight-candidate migration with debounce, malformed-schema latching, and
post-commit route-target replacement rollup observation through both entry
points.

## Verification

Round-11 remediation: the concurrent cleanup regression now deterministically
pauses the older successful attempt at the SQL/state-update boundary using a
channel closed exactly once. It runs the newer forced-failure reconciliation
synchronously, asserts its retryable alert lock is set, then releases and
waits for the older success. The final assertion verifies that older completion
cannot clear the newer lock.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage -count=1 -race ./internal/...
$ make build
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-11 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-10 remediation: `reconcileHealthAlertStates` now assigns monotonic
cleanup-attempt generations and applies a result only when its generation is
newer than every completed result already applied. This prevents an older
successful attempt from clearing a newer failed attempt's retryable lock. The
deterministic concurrent regression pauses an older successful cleanup at the
SQL/state-update boundary, runs a newer cleanup after adding an orphan and a
forced-delete failure, then releases the older pass. It verifies the older
success completes its state update after the newer failure and alerting remains
locked.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
$ make build
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-10 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-9 remediation: alert-state locks now retain independent provenance. A
failed post-commit `health_alert_states` cleanup sets only a retryable cleanup
component; schema, dedupe-key, migration, and trigger failures set a durable
component. Alerting is available only when neither component is present, and a
successful cleanup clears only its own component. The regression establishes a
durable lock, forces cleanup to fail, then replays inventory so the later
cleanup succeeds with zero rows deleted. It verifies that claiming, recording
a dispatch result, and health-alert production remain locked until the durable
condition's own recovery path completes.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
$ make build
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-9 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-8 remediation: service inventory rows now carry a durable incarnation.
The core inventory transaction assigns a fresh token when an ID first appears
and carries the token only while that ID remains present through an authoritative
replacement. `health_alert_states` records the corresponding service
incarnation. Before the producer trusts any stored health state, it compares
that value to the current service row; a mismatch is atomically overwritten as
a silent fresh baseline. Consequently, a failed or canceled post-commit orphan
sweep cannot cause a reused service ID to inherit stable, candidate, or failure
state. The sweep remains best-effort hygiene only and unlocks the transient
cleanup latch whenever its orphan delete succeeds.

The idempotent migrations add both incarnation columns. Existing service rows
receive a token during migration; existing alert-state rows receive the empty
default and therefore intentionally mismatch on their next observation. That
one-time observation silently resets their baseline.

The regression commits an inventory removal while cleanup deletion is forced to
fail, replays the same nonempty inventory without repairing the cleanup path,
and confirms that the stale row remains until the producer overwrites it. Its
first status is a silent fresh baseline both in-process and after restart. The
route-target replacement ordering regression remains covered.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
$ make build
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-8 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-7 remediation: post-commit alert-state reconciliation now deletes both
orphaned rows and rows for service IDs newly appearing in the just-committed
inventory. This makes an immediately reused ID begin with no inherited stable,
candidate, or failure state. Cleanup remains outside the core transaction; a
failure latches only alerting, and a later fully successful cleanup pass clears
only that transient cleanup lock (schema and migration locks remain
fail-closed). The regression runs the exact forced-cleanup-failure, removal,
repair, and immediate same-ID reuse sequence with no empty-inventory sweep in
between. It verifies the first health observation is a silent fresh baseline
both in-process and after a restart.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage  (cached)
ok   github.com/example/gitops-dashboard/internal/agent  (cached)
ok   github.com/example/gitops-dashboard/internal/app  0.939s
ok   github.com/example/gitops-dashboard/internal/monitor  5.810s
ok   github.com/example/gitops-dashboard/internal/scanner  1.144s
...all remaining tested internal packages passed

$ make build
npm run build
vite v8.1.0 building client environment for production...
✓ built in 284ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-7e3805d02675 -X github.com/example/gitops-dashboard/internal/version.Commit=7e3805d02675 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-11T02:38:20Z" ./cmd/gitops-dashboard

$ make check
npm run format
npm run build
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
npm test

$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-7 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-6 remediation: every successful configured-service replacement,
runtime-service replacement, runtime prune, and successful scan inventory
commit now runs an alert-only set-difference sweep outside the inventory
transaction. The sweep deletes each `health_alert_states` row whose
`service_id` is no longer present in `services`; it retries even while alerting
is latched, so repairing a prior cleanup failure needs no durable pending-ID
ledger. Sweep failures continue to latch alerting/readiness without rolling
back committed inventory. Regressions force a cleanup failure with multiple
removed IDs, repair it and run a later pass, and repeat across a restart. They
assert every orphan is removed and that a reused ID establishes a silent fresh
health baseline rather than inheriting stale failure state.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage  6.807s
ok   github.com/example/gitops-dashboard/internal/... (all tested packages passed)

$ make build
npm run build
vite v8.1.0 building client environment for production...
✓ built in 419ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-7e3805d02675 -X github.com/example/gitops-dashboard/internal/version.Commit=7e3805d02675 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-11T02:30:22Z" ./cmd/gitops-dashboard

$ make check
npm run format
npm run build
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
npm test

$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-6 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-5 remediation: `health_alert_states` lifecycle cleanup now runs after the
core inventory transaction commits in configured-service replacement,
runtime-service replacement, runtime pruning, and scan inventory replacement.
It is best-effort alert-only work: a cleanup failure latches alerting, while
the committed inventory remains atomic and any orphaned alert-state row is
reconciled by a later cleanup pass. The regression exercises both
`RAISE(ABORT)` and transaction-destroying `RAISE(ROLLBACK)` DELETE triggers at
all four sites. Each case asserts a successful operation, alerting latched,
and its atomic core result: service removal, status-result and status-history
removal where that lifecycle owns them, plus committed repository and scan
state for scan replacement.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage  6.010s
ok   github.com/example/gitops-dashboard/internal/... (all tested packages passed)

$ make build
npm run build
✓ built in 265ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-7e3805d02675 -X github.com/example/gitops-dashboard/internal/version.Commit=7e3805d02675 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-11T02:21:54Z" ./cmd/gitops-dashboard

$ make check
npm run format
npm run build
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
all checks passed

$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the
round-5 changes, confirming the sanitizer remains byte-identical to HEAD.

Round-4 remediation: schema repair now quarantines and latches alerting for any
trigger attached to `health_alert_states`. Cleanup DELETE failures are alert-only
regardless of their SQLite error, so all four inventory reconciliation paths
commit their core work while alerting locks. The regression covers a canonical
schema with a `BEFORE DELETE RAISE(ABORT)` trigger at startup, plus trigger
aborts during configured-service replacement, runtime-service replacement,
runtime pruning, and scan inventory replacement.

The following commands were run verbatim from the repository root on
2026-07-10 and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage  (cached)
ok   github.com/example/gitops-dashboard/internal/... (all tested packages passed)

$ make build
npm run build
✓ built in 263ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false ... ./cmd/gitops-dashboard

$ make check
npm run format
npm run build
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...

$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output before the
verification run, confirming the sanitizer is byte-identical to HEAD.

Round-3 remediation observed on 2026-07-10 from the repository root. The
following commands were run verbatim and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/...
$ make build
$ make check
$ git diff --check
```

The lifecycle cleanup regressions verify that configured-service removal still
commits core inventory when alert state is latched at startup and
`health_alert_states` is absent, and when that table is incompatible because
it lacks `service_id`. In both cases alerting remains latched.

Round-2 remediation observed on 2026-07-10 from the repository root. The
following commands were run verbatim and exited successfully:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage 4.807s
ok   github.com/example/gitops-dashboard/internal/monitor 5.864s
ok   github.com/example/gitops-dashboard/internal/... (all tested packages passed)

$ make build
npm run build
✓ built in 269ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false ... ./cmd/gitops-dashboard

$ make check
npm run format
npm run build
npm run lint
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go vet ./cmd/... ./internal/...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./cmd/... ./internal/...
npm test

$ git diff --check
```

The added regressions cover independent observation identity, debounce
continuity, the first-candidate failure duration through promotion, silent
failing-baseline recovery duration, not-applicable and pruning rollup
observation, replacement merge recovery observation, cooldown suppression,
legacy candidate reset, incompatible-schema latching, and state
reconciliation.

## Documentation Sweep

Pass: this record documents the rollup decision, compatibility behavior, E2E
plan, and observed focused-test evidence. User configuration documentation is
deferred to the alerting delivery task because no public delivery surface is
introduced here.
