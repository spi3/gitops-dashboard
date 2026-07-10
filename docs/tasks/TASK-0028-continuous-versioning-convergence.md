# TASK-0028: Continuous versioning convergence and rollout

## Scope

Ensure queued release runs converge latest, major, and minor image channels;
document the continuous release model and guarded operator release workflow.

## Out of Scope

- GitHub server-side ruleset configuration. The recommended v* protection is
  documented, but must be configured by repository administrators.
- Deployment of the released image to downstream GitOps repositories.

## Dependencies

- TASK-0027 inline auto-patch release.

## Acceptance Criteria

- A release run moves latest only if its checked-out commit is still
  origin/main at publish time.
- Major and minor channels are recomputed from the highest compatible strict
  SemVer exact tag, so delayed releases converge forward.
- Release metadata records each moving channel with its resolved digest, so a
  delayed release cannot imply that a newer channel still points at its exact
  digest.
- Versioning, deployment, README, task conventions, and release operator
  guidance describe the same continuous model.
- The fallback release runbook covers prerequisites, immutable-version cost,
  locking, and safe recovery.

## E2E Plan

1. Push a new commit to main and wait for the test-gated release job.
2. Verify the exact image and GitHub Release identify that commit and one
   multi-platform digest.
3. Verify latest only changes when the release commit equals the then-current
   origin/main; inspect the highest matching exact tags for vX and vX.Y and
   confirm their channel digests match.
4. Re-run a delayed older release and confirm it cannot retarget a channel or
   latest backward.

## Controlled Rollout Expectation

Observed on 2026-07-10 before this task's eventual main push: remote strict
SemVer tags contain v0.0.1, whose peeled commit is
e93f44d299b488e1c11832144e1debd370bd7d4e; the checked-out HEAD was that
same commit. The first new main commit containing this task will therefore be
allocated v0.0.2 (the next patch after the observed highest tag). This is a
planned rollout expectation, not a remote publication result.

If a repository has no strict SemVer tag, the allocator bootstrap path instead
allocates v0.0.1; non-strict tags do not affect that result.

## Verification Evidence

### Round 9 (2026-07-10)

The imagetools regression test now walks the parsed checked-in workflow YAML
and extracts every `imagetools inspect --format` expression from its scalar
nodes. Each extracted field path is traversed through the vendored Buildx
`{{json .}}` capture decoded as `map[string]any`; no test-only Go struct
redeclares that schema. The validator accepts the production
`{{.Manifest.Digest}}` formats and rejects `.Digest`, `{{ .Digest }}`, and an
appended `{{ .Bogus }}` on every extracted production format.

The following commands were executed verbatim from the repository root:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./internal/...
ok  	github.com/example/gitops-dashboard/internal/ci	(cached)
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
npm run build

> gitops-dashboard@1.0.0 build
> vite build

(node:20) Warning: The 'NO_COLOR' env is ignored due to the 'FORCE_COLOR' env being set.
vite v8.1.0 building client environment for production...
transforming...
✓ 24 modules transformed.
rendering chunks...
computing gzip size...
✓ built in 272ms
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build -buildvcs=false -ldflags "-X github.com/example/gitops-dashboard/internal/version.Version=dev-d6e20a05b8f7 -X github.com/example/gitops-dashboard/internal/version.Commit=d6e20a05b8f7 -X github.com/example/gitops-dashboard/internal/version.BuildDate=2026-07-10T21:16:46Z" ./cmd/gitops-dashboard

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

All commands exited 0. No remote release was triggered from this workspace.

### Round 8 (2026-07-10)

Run 29122614648 allocated and pushed the immutable `v0.0.2` exact image, then
failed in `Publish moving image channels` before the moving channels, `latest`,
commit-SHA image, and GitHub Release were created. That partial state remains
intentional: immutable tags and exact images are never retargeted or deleted.
The next successful release recomputes major/minor from all exact tags and
advances `latest` only to the current main release, so it converges those
forward without incorrectly treating `v0.0.2` as a complete release.

Every workflow `imagetools inspect` now calls the local
`imagetools_manifest_digest` helper, which uses `{{.Manifest.Digest}}`. The
regression fixture is a real `{{json .}}` capture from Docker Buildx v0.34.1;
the test renders templates against that input schema and rejects the former
nonexistent top-level `{{.Digest}}` field.

The following commands were executed verbatim from the repository root:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./internal/...
$ make build
$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml
$ git diff --check
```

All commands exited 0. The exact command output is retained in the task
delivery record for this round; no remote release was triggered from this
workspace.

### Round 7 (2026-07-10)

The following commands were executed verbatim from the repository root after
making displaced-main reconciliation skip workflow cancellation while retaining
post-failure repair:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./internal/...
ok  	github.com/example/gitops-dashboard/internal/ci	2.613s
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
exit 0

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

`Reconcile displaced main release` now has `if: ${{ !cancelled() }}`. That
allows its bounded repair path after an ordinary earlier publication failure,
but prevents it from dispatching a fresh release after an operator cancels the
workflow. `TestReleaseWorkflowReconcilesDisplacedCurrentMain` structurally
requires that exact guard, rather than merely accepting `always()`.

No remote release was triggered from this workspace.

### Round 6 (2026-07-10)

The following commands were executed verbatim from the repository root after
making displaced-main reconciliation run even after an earlier publication
failure:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/internal/ci  (cached)
ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard  (cached)
ok   github.com/example/gitops-dashboard/cmd/release  (cached)
ok   github.com/example/gitops-dashboard/cmd/version-allocator  (cached)
ok   github.com/example/gitops-dashboard/internal/agent  (cached)
ok   github.com/example/gitops-dashboard/internal/app  (cached)
ok   github.com/example/gitops-dashboard/internal/auth  (cached)
ok   github.com/example/gitops-dashboard/internal/config  (cached)
ok   github.com/example/gitops-dashboard/internal/core  (cached)
ok   github.com/example/gitops-dashboard/internal/dockerapi  (cached)
ok   github.com/example/gitops-dashboard/internal/environment  (cached)
?    github.com/example/gitops-dashboard/internal/hostinventory  [no test files]
ok   github.com/example/gitops-dashboard/internal/monitor  (cached)
ok   github.com/example/gitops-dashboard/internal/parser  (cached)
ok   github.com/example/gitops-dashboard/internal/routetarget  (cached)
ok   github.com/example/gitops-dashboard/internal/sanitizer  (cached)
ok   github.com/example/gitops-dashboard/internal/scanner  (cached)
ok   github.com/example/gitops-dashboard/internal/storage  (cached)
?    github.com/example/gitops-dashboard/internal/ui  [no test files]
?    github.com/example/gitops-dashboard/internal/version  [no test files]

$ make build
exit 0

$ make check
exit 0

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

`Reconcile displaced main release` now has `if: ${{ always() }}`: if the
channel-selection/publication step fails, the reconciliation still evaluates
the bounded displaced-main path, while GitHub preserves that earlier failed
step in the job conclusion. `TestReleaseWorkflowReconcilesDisplacedCurrentMain`
asserts that guard and runs the production script with current main's annotated
strict SemVer tag present but its exact image absent. The script dispatches the
same-source patch repair in that tag-only partial-release state. Existing
coverage still proves the full completeness predicate, no registry deletions,
per-head chaining, T-047 behavior, and byte-identical sanitizer protection.

No remote release was triggered from this workspace.

### Round 5 (2026-07-10)

The following commands were executed verbatim from the repository root after
repairing the incomplete-artifact reconciliation predicate and bounded
per-head chain guard:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
ok   github.com/example/gitops-dashboard/internal/ci  2.160s
ok   github.com/example/gitops-dashboard/cmd/gitops-dashboard  (cached)
ok   github.com/example/gitops-dashboard/cmd/release  (cached)
ok   github.com/example/gitops-dashboard/cmd/version-allocator  (cached)
ok   github.com/example/gitops-dashboard/internal/agent  (cached)
ok   github.com/example/gitops-dashboard/internal/app  (cached)
ok   github.com/example/gitops-dashboard/internal/auth  (cached)
ok   github.com/example/gitops-dashboard/internal/config  (cached)
ok   github.com/example/gitops-dashboard/internal/core  (cached)
ok   github.com/example/gitops-dashboard/internal/dockerapi  (cached)
ok   github.com/example/gitops-dashboard/internal/environment  (cached)
?    github.com/example/gitops-dashboard/internal/hostinventory  [no test files]
ok   github.com/example/gitops-dashboard/internal/monitor  (cached)
ok   github.com/example/gitops-dashboard/internal/parser  (cached)
ok   github.com/example/gitops-dashboard/internal/routetarget  (cached)
ok   github.com/example/gitops-dashboard/internal/sanitizer  (cached)
ok   github.com/example/gitops-dashboard/internal/scanner  (cached)
ok   github.com/example/gitops-dashboard/internal/storage  (cached)
?    github.com/example/gitops-dashboard/internal/ui  [no test files]
?    github.com/example/gitops-dashboard/internal/version  [no test files]

$ make build
exit 0

$ make check
exit 0

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml

$ git diff --check
```

`TestReleaseWorkflowReconcilesDisplacedCurrentMain` runs the production
reconciliation script with C's annotated strict SemVer tag and exact image
already allocated, but with a different `latest` digest, no commit SHA image,
and no GitHub Release. It proves delayed B dispatches C's same-source patch
repair and then executes C's production channel script to converge `latest`.
The same harness proves a reconciliation run does not re-dispatch C when C is
still current, but may dispatch strictly newer descendant D. No registry
deletion is accepted by the fake registry, and the test asserts none occurs.

No remote release was triggered from this workspace.

### Round 4 (2026-07-10)

The following commands were executed verbatim from the repository root after
adding displaced-current-main reconciliation:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
exit 0 — internal/ci, cmd/gitops-dashboard, cmd/release,
cmd/version-allocator, and every tested internal package passed;
internal/hostinventory, internal/ui, and internal/version reported [no test files].

$ make build
exit 0 — Vite transformed 24 modules and the Go dashboard binary built.

$ make check
exit 0 — formatting, Vite build, ESLint, go vet, Go tests, TypeScript
typecheck, and the final ESLint pass completed in the isolated check checkout.

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml
exit 0 — no output.

$ git diff --check
exit 0 — no output.
```

`internal/ci/workflow_test.go` executes the production reconciliation and
channel scripts against fake GitHub, Git, and registry commands. Its
A-running/C-pending/B-finishes-last interleaving proves that B dispatches C
only after API/tag checks show C is uncovered, then simulates C's normal
serialized release and verifies `latest` converges to C. The fake registry
records and fails deletion requests; the test asserts none occur anywhere in
this path. The reconciliation input marker prevents a workflow-dispatch run
from re-dispatching the same displaced head.

No remote release was triggered from this workspace.

### Round 3 (2026-07-10)

The following commands were executed verbatim from the repository root after
removing all registry-deletion behavior from empty-prior-`latest` recovery:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
exit 0 — internal/ci, cmd/gitops-dashboard, cmd/release,
cmd/version-allocator, and every tested internal package passed;
internal/hostinventory, internal/ui, and internal/version reported [no test files].

$ make build
exit 0 — Vite transformed 24 modules and the Go dashboard binary built.

$ make check
exit 0 — formatting, Vite build, ESLint, go vet, Go tests, TypeScript
typecheck, and the final ESLint pass completed in the isolated check checkout.

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml
exit 0 — no output.

$ git diff --check
exit 0 — no output.
```

`internal/ci/workflow_test.go` executes the production YAML channel script
against a stateful fake registry. With no prior `latest`, it verifies that an
already released advanced-main digest advances `latest`; without that digest,
it verifies the successful current digest remains until a simulated subsequent
release converges it forward. The fake registry fails and records any deletion
request, and the harness asserts none was issued.

No remote release was triggered from this workspace.

### Round 2 (2026-07-10)

The following commands were executed verbatim from the repository root after
the empty/unreadable-prior-`latest` recovery and delayed-release metadata fixes:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
exit 0 — internal/ci, cmd/gitops-dashboard, cmd/release,
cmd/version-allocator, and every tested internal package passed;
internal/hostinventory, internal/ui, and internal/version reported [no test files].

$ make build
exit 0 — Vite transformed 24 modules and the Go dashboard binary built.

$ make check
exit 0 — formatting, Vite build, ESLint, go vet, Go tests, TypeScript
typecheck, and the final ESLint pass completed in the isolated check checkout.

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml
exit 0 — no output.

$ git diff --check
exit 0 — no output.
```

`internal/ci/workflow_test.go` then executed the production YAML paths,
including the then-current absent-prior-`latest` recovery and a delayed release
whose minor/major digests differ from the exact digest and whose notes preserve
those resolved digests. Round 3 supersedes the registry-removal recovery with
the pointer-only policy above.

No remote release was triggered from this workspace.

### Round 1

Observed on 2026-07-10 from the repository root:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci ./cmd/... ./internal/...
PASS (exit 0): internal/ci; cmd/gitops-dashboard; cmd/release;
cmd/version-allocator; and all tested internal packages passed. internal/hostinventory,
internal/ui, and internal/version reported [no test files].

$ make build
PASS (exit 0): Vite transformed 24 modules and the Go dashboard binary built.

$ make check
PASS (exit 0): formatting, Vite build, ESLint, go vet, and go test ./cmd/... ./internal/...
passed in the isolated check checkout.

$ /tmp/gitops-dashboard-bin/actionlint .github/workflows/ci.yml
PASS (exit 0; no output).

$ git diff --check
PASS (exit 0; no output).
```

All commands exited successfully. No remote release was triggered from this
workspace.

## Documentation and Maintainability Sweep

- README.md, docs/deployment.md, and docs/versioning.md: distinguish accepted
  revisions from executed bounded-queue release jobs; document pending-job
  replacement, displaced-run reconciliation and convergence after a burst, and
  why skip markers remain banned.
- .github/workflows/ci.yml and internal/ci/workflow_test.go: publish SHA tags
  independently; recheck and restore or advance latest across the registry
  race; reconcile an uncovered displaced current-main head through the
  test-gated dispatch front door; never delete registry content; and execute
  production-script harnesses for both no-prior outcomes and the
  A-running/C-pending/B-last interleaving.
- docs/requirements.md: reviewed; its carve-out allowing only repository
  release tooling and CI to create tags/lock refs remains consistent.
- docs/task_acceptance_criteria.md and docs/tasks/tracker.md: reconciled
  with the external orchestration workspace as system of record while retaining
  repository acceptance and evidence mirrors.

Proposed Conventional Commit subject: ci(release): reconcile displaced main releases (T-028).
