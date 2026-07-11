# TASK-0031: Route identity replacement migration

## Scope

Migrate monitor state only when a successful repository rescan proves a
provenance-backed bijection between old and new route set differences. The
migration covers every configured HTTP target prefix, preserves historical
payloads, moves mutable alert/dedupe state atomically, and retains ambiguous
old identities through monitor stale pruning until a later scan resolves them.

## Out of Scope

- New route discovery sources.
- Changing general URL canonicalization or its default-port rules.

## Dependencies

- TASK-0030 route discovery correctness.
- TASK-0021 storage overlap, to land first externally.

## E2E Plan

1. Exercise the scanner's route replacement derivation with a manifest-derived
   route transition and reject multiple new port candidates for one host.
2. Rescan through the storage handoff, then confirm active migrated overrides
   suppress monitoring and the old state target is gone.
3. Run the dashboard browser specification after the Go package suite and
   build.

## Observed Results

Round 1 on 2026-07-10 added regression coverage for set-difference/bijection
matching, still-present old identities, ambiguity retention during stale
pruning, payload-preserving history migration, mutable-vs-delivered alert
retargeting with idempotent retry, and the zero-replacement fast path. Route
override state is now applied when current status is read; migration does not
rewrite historical observations. The sanitizer has no diff from HEAD.

Round 2 on 2026-07-10 reconciles the complete unresolved identity set during
each successful scan. A prior A->{B,C} ambiguity is reconsidered against the
later complete exposure set, so the next {B} scan proves A->B, migrates state,
clears the exclusion, and restores normal stale pruning. Mutable alerts are
now retargeted only for the replacement service, and canonicalization plus
replacement migration preserve the observed history health/message payload
under active overrides. The sanitizer remains byte-identical to HEAD.

Round 3 on 2026-07-10 validates the alert-state lock and dedupe-key currency
inside the route migration transaction before it reads or writes alert rows. A
safety validation failure does not block the authoritative route scan: exact
old/new alert targets are persisted as deferred reconciliation work and applied
atomically by a later successful scan after alert state is unlocked. Pending
dedupe collisions are reconciled atomically by keeping the old-identity event,
merging active destination sinks into it, terminalizing the destination
duplicate, and only then retargeting the keeper. Terminal events and dispatches
are not rewritten.

Round 4 on 2026-07-10 replays deferred alert replacements to a fixed point,
so adversarial lexical ordering cannot strand a locked A->B->C chain at B.
Deferred edges remain until their mutable source no longer exists after that
fixed-point replay. Pending-collision reconciliation now moves a
non-conflicting destination in-flight dispatch to the keeper without changing
its worker, claim, or lease; an overlapping pending row is removed rather than
turning the active delivery into a fresh claimable dispatch. The sanitizer
remains byte-identical to HEAD.

## Verification Evidence

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/scanner ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage
ok   github.com/example/gitops-dashboard/internal/scanner
ok   github.com/example/gitops-dashboard/internal/monitor
ok   github.com/example/gitops-dashboard/internal/agent
ok   github.com/example/gitops-dashboard/internal/app
ok   github.com/example/gitops-dashboard/internal/auth
ok   github.com/example/gitops-dashboard/internal/ci
ok   github.com/example/gitops-dashboard/internal/config
ok   github.com/example/gitops-dashboard/internal/core
ok   github.com/example/gitops-dashboard/internal/dockerapi
ok   github.com/example/gitops-dashboard/internal/environment
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/parser
ok   github.com/example/gitops-dashboard/internal/routetarget
ok   github.com/example/gitops-dashboard/internal/sanitizer
?    github.com/example/gitops-dashboard/internal/ui [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]
$ make build
✓ built in 258ms
$ make check
$ git diff --check
```

Round 2 verification (verbatim commands):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/scanner ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage
ok   github.com/example/gitops-dashboard/internal/scanner
ok   github.com/example/gitops-dashboard/internal/monitor
ok   github.com/example/gitops-dashboard/internal/agent
ok   github.com/example/gitops-dashboard/internal/app
ok   github.com/example/gitops-dashboard/internal/auth
ok   github.com/example/gitops-dashboard/internal/ci
ok   github.com/example/gitops-dashboard/internal/config
ok   github.com/example/gitops-dashboard/internal/core
ok   github.com/example/gitops-dashboard/internal/dockerapi
ok   github.com/example/gitops-dashboard/internal/environment
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/parser
ok   github.com/example/gitops-dashboard/internal/routetarget
ok   github.com/example/gitops-dashboard/internal/sanitizer
?    github.com/example/gitops-dashboard/internal/ui [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]
$ make build
✓ built in 259ms
$ make check
$ git diff --check
```

Round 3 verification (verbatim commands):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/scanner ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage
ok   github.com/example/gitops-dashboard/internal/scanner
ok   github.com/example/gitops-dashboard/internal/monitor
ok   github.com/example/gitops-dashboard/internal/agent
ok   github.com/example/gitops-dashboard/internal/app
ok   github.com/example/gitops-dashboard/internal/auth
ok   github.com/example/gitops-dashboard/internal/ci
ok   github.com/example/gitops-dashboard/internal/config
ok   github.com/example/gitops-dashboard/internal/core
ok   github.com/example/gitops-dashboard/internal/dockerapi
ok   github.com/example/gitops-dashboard/internal/environment
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/parser
ok   github.com/example/gitops-dashboard/internal/routetarget
ok   github.com/example/gitops-dashboard/internal/sanitizer
?    github.com/example/gitops-dashboard/internal/ui [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]
$ make build
✓ built in 266ms
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the round 3
work, confirming the sanitizer is byte-identical to HEAD.

Round 5 on 2026-07-10 defers a pending-dedupe collision whenever a same-sink
merge would touch an in-flight claim. Both pending events, their distinct
hashes, and their claims remain unchanged while the existing deferred-edge
fixed-point reconciliation waits for terminal delivery. Terminal keeper
dispatches are never deleted or rewritten; a delivered keeper row remains
byte-identical when a colliding destination claim exists.

Round 6 on 2026-07-10 makes same-sink pending collision reconciliation
status-aware. A pending destination row is reset only when the keeper row is
delivered. A dead-lettered keeper instead defers reconciliation, preserving the
claimable destination delivery and both terminal dispatch rows verbatim. The
scan commits, and deferred reconciliation converges after the active keeper
and destination deliveries settle. The sanitizer remains byte-identical to
HEAD.

Round 6 verification (verbatim commands):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/scanner ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage  4.647s
ok   github.com/example/gitops-dashboard/internal/scanner  1.111s
ok   github.com/example/gitops-dashboard/internal/monitor  6.069s
ok   github.com/example/gitops-dashboard/internal/agent    (cached)
ok   github.com/example/gitops-dashboard/internal/app  0.715s
ok   github.com/example/gitops-dashboard/internal/auth (cached)
ok   github.com/example/gitops-dashboard/internal/ci   (cached)
ok   github.com/example/gitops-dashboard/internal/config   (cached)
ok   github.com/example/gitops-dashboard/internal/core (cached)
ok   github.com/example/gitops-dashboard/internal/dockerapi    (cached)
ok   github.com/example/gitops-dashboard/internal/environment  (cached)
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/parser   (cached)
ok   github.com/example/gitops-dashboard/internal/routetarget  (cached)
ok   github.com/example/gitops-dashboard/internal/sanitizer    (cached)
?    github.com/example/gitops-dashboard/internal/ui  [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]
$ make build
✓ built in 263ms
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the round 6
work, confirming the sanitizer is byte-identical to HEAD.

Round 5 verification (verbatim commands):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/scanner ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage	4.454s
ok   github.com/example/gitops-dashboard/internal/scanner	1.115s
ok   github.com/example/gitops-dashboard/internal/monitor	5.810s
ok   github.com/example/gitops-dashboard/internal/agent	(cached)
ok   github.com/example/gitops-dashboard/internal/app	0.740s
ok   github.com/example/gitops-dashboard/internal/auth	(cached)
ok   github.com/example/gitops-dashboard/internal/ci	(cached)
ok   github.com/example/gitops-dashboard/internal/config	(cached)
ok   github.com/example/gitops-dashboard/internal/core	(cached)
ok   github.com/example/gitops-dashboard/internal/dockerapi	(cached)
ok   github.com/example/gitops-dashboard/internal/environment	(cached)
?    github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok   github.com/example/gitops-dashboard/internal/parser	(cached)
ok   github.com/example/gitops-dashboard/internal/routetarget	(cached)
ok   github.com/example/gitops-dashboard/internal/sanitizer	(cached)
?    github.com/example/gitops-dashboard/internal/ui	[no test files]
?    github.com/example/gitops-dashboard/internal/version	[no test files]
$ make build
✓ built in 250ms
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the round 5
work, confirming the sanitizer is byte-identical to HEAD.

Round 4 verification (verbatim commands):

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/storage ./internal/scanner ./internal/monitor ./internal/...
ok   github.com/example/gitops-dashboard/internal/storage
ok   github.com/example/gitops-dashboard/internal/scanner
ok   github.com/example/gitops-dashboard/internal/monitor
ok   github.com/example/gitops-dashboard/internal/agent
ok   github.com/example/gitops-dashboard/internal/app
ok   github.com/example/gitops-dashboard/internal/auth
ok   github.com/example/gitops-dashboard/internal/ci
ok   github.com/example/gitops-dashboard/internal/config
ok   github.com/example/gitops-dashboard/internal/core
ok   github.com/example/gitops-dashboard/internal/dockerapi
ok   github.com/example/gitops-dashboard/internal/environment
?    github.com/example/gitops-dashboard/internal/hostinventory [no test files]
ok   github.com/example/gitops-dashboard/internal/parser
ok   github.com/example/gitops-dashboard/internal/routetarget
ok   github.com/example/gitops-dashboard/internal/sanitizer
?    github.com/example/gitops-dashboard/internal/ui [no test files]
?    github.com/example/gitops-dashboard/internal/version [no test files]
$ make build
$ make check
$ git diff --check
```

`git diff --numstat -- internal/sanitizer` produced no output after the round 4
work, confirming the sanitizer is byte-identical to HEAD.

## Documentation Sweep

No user-facing documentation change is needed: this is persisted monitor-state
continuity during discovery rescan, not a new discovery source or configuration
surface.
