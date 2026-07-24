# TASK-0060: Credential and build-context safety

## Status

Proposed

## Scope

Two production subsystems:

1. The repository credential-lifecycle subsystem spanning `internal/config`,
   `internal/app`, and `internal/scanner`. Startup configuration, application
   construction, cached-remote scrubbing, credential resolution, and scanner
   authentication are one lifecycle for this task, so these three packages are
   treated as a single unit.
2. Docker/build tooling: `Dockerfile`, `.dockerignore`, the narrow
   `scripts/docker-context-audit.sh` context-audit script, and `internal/ci`
   build-policy tests.

This replaces only the credential-persistence portion of external T-034 and
the Docker build-safety portion of T-044 (homelab re-triage 2026-07-12; both
are superseded). The in-repo task record and tracker row are process
artifacts, not a third subsystem. `internal/sanitizer` is consumed as-is and
is not modified.

## Dependencies

None.

## Task-specific acceptance criteria

R-1/N-1..N-4 use a task-local namespace defined here, distinct from the
project `docs/requirements.md` ID space.

- R-1: supported Git credentials never remain in cached origin fetch/push
  URLs, and Docker builds exclude defined secret paths or reject
  marker-bearing allowed content before local copying.
- N-1: all external work (Git subprocess calls, including the new `git
  config` scrub commands) stays bound by the existing `GitCommandTimeout`
  discipline.
- N-2: secret values never appear in logs, errors, persisted storage, or
  `.git/config`.
- N-3: a representative pre-change fixture (multiple fetch/push URLs, mixed
  credentialed and credential-free values) proves the scrub behavior.
- N-4: automated tests cover both the focused unit contracts and the full
  repository test suite plus a real Docker build.

Concretely:

- [x] Repository credential-source and URL-transport checks removed from
      global startup validation; general configuration validation preserved.
- [x] `App.New`, redactor construction, logging setup, and scanner
      construction never call `RepositoryConfig.Token()` or read a
      `tokenFile`; only raw/decoded URL-userinfo redaction values are
      registered immediately.
- [x] Each repository attempt locates and containment-checks its expected
      cache, registers URL-userinfo redaction values, and scrubs the cache
      before any credential resolution, policy validation, origin
      reconciliation, or network-capable Git command.
- [x] All four credential cases (embedded userinfo without a token; embedded
      userinfo with a token; plain HTTP with userinfo or a token; a clean
      configured URL against a stale credentialed cache) behave exactly as
      specified.
- [x] Configured HTTP(S) userinfo is rejected with an error naming only the
      repository.
- [x] Token-authenticated repositories require case-insensitive `https://`.
- [x] `scrubCachedOriginCredentials` enumerates `remote.origin.url` and
      `remote.origin.pushurl` via `git config --get-all`, with the specified
      exit-code semantics, and rewrites only HTTP(S) values containing
      userinfo, preserving count, order, and every other remote form
      byte-for-byte.
- [x] Redaction values are registered before any scrub mutation can fail.
- [x] Pre-change fixture and the four exact-named tests added (see
      Verification).
- [x] `Dockerfile` uses explicit local `COPY` sources for the Go build; no
      broad `COPY . .` or `ADD .`.
- [x] `.dockerignore` is a deny-first allowlist excluding tests, testdata,
      unrelated `cmd/*` entry points, `internal/ci`, Git metadata, local
      artifacts, and documentation, plus the defined secret-bearing path
      classes as final rules.
- [x] A BuildKit context-audit stage inspects the real, ignore-filtered
      context through a read-only bind mount and emits only a non-secret
      success marker.
- [x] Every stage with a local `COPY`/`ADD` has an explicit graph dependency
      (`COPY --from=context-audit`) before its first local copy.
- [x] `internal/ci` test `TestDockerBuildContextPolicy` parses the checked-in
      Dockerfile/`.dockerignore`, rejects broad copies, verifies the audit
      dependency, verifies deny-first/final sensitive-path rules, and offers
      an opt-in Docker fixture that hard-fails (never skips) on a missing
      CLI or inaccessible daemon under `GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1`.
- [x] The real Dockerfile is built through at least the `build` stage.

## Credential lifecycle contract

`internal/scanner/credentials.go` defines two package-private contracts kept
stable for T-061:

- `credentialFreeRemoteURL(raw string) (clean string, stripped bool, err
  error)`: validates a single Git remote and, only for HTTP(S) schemes
  (matched case-insensitively), strips embedded userinfo. Every other
  supported form (SSH, `git:`, `file:`, SCP-like `[user@]host:path`, and
  filesystem paths) is returned byte-for-byte unchanged. Errors are
  sanitized and never echo the raw remote or userinfo.
- `scrubCachedOriginCredentials(ctx, repoPath, redactor, env) error`:
  enumerates `remote.origin.url` and `remote.origin.pushurl` with `git
  config --get-all`, treating exit 0 as an ordered value list, exit 1 with
  zero stdout bytes as a valid empty list, and every other outcome as a read
  failure. It rewrites only the HTTP(S) values that carry userinfo, via
  `--unset-all` followed by ordered `--add` calls, so count and order are
  always preserved.

Pipeline order enforced in `internal/scanner/scanner.go`
(`syncRepoUnshared`): containment-check the cache path, register
configured URL-userinfo redaction values, scrub any existing cache, then
resolve credentials and validate policy (`gitAuth`), then reconcile the
configured origin (`reconcileConfiguredOrigin`, token-auth only), then run
network-capable Git commands.

**Containment-check semantics** (explicitly under-specified by the spec):
`containedRepoPath` uses a straightforward `filepath.Rel`-based no-escape
check — the resolved cache path must equal or be nested under the
repository cache directory, verified via `filepath.Rel` plus a `..`/absolute
rejection. `safeName` already excludes path separators from repository
names, so an actual escape is not expected in practice; this check makes the
guarantee explicit and enforced rather than incidental. T-061 will formalize
this further if a stronger guarantee becomes necessary.

`internal/config.Config.Validate` no longer rejects an unset repository
`tokenEnv`; that check, along with unreadable `tokenFile`, embedded
HTTP(S) userinfo, and non-HTTPS token transport, is now enforced by
`Scanner.gitAuth`, which only runs after cache scrubbing.

## Build-context contract

The Dockerfile's `context-audit` stage runs
`scripts/docker-context-audit.sh` against the real, ignore-filtered build
context through a read-only BuildKit bind mount (`RUN
--mount=type=bind,source=.,target=/context,ro`), never a `COPY`, so its
content never enters an image layer. The script:

1. Re-checks (defense in depth) that the defined secret-bearing path classes
   are absent from the context it can see.
2. Scans for a generic PEM private-key marker (`-----BEGIN PRIVATE
   KEY-----`, assembled from two halves in the script so the file's own
   on-disk content never contains the marker contiguously and therefore
   never matches itself) in any file, failing the build if found.

Every stage that performs a local `COPY`/`ADD` (`ui`, `build`, the final
runtime stage) begins with `COPY --from=context-audit /audit-ok
/tmp/context-audit-ok` before its first local copy. This is an inter-stage
copy (not a local copy, and not restricted by the local-copy allowlist); it
creates an explicit BuildKit graph dependency, so if the audit stage fails,
none of those stages' local copies can run, regardless of Dockerfile
textual order.

This is defense in depth only: Docker already removes `.dockerignore`
matches before ever sending the context to the daemon, so the audit stage
cannot see (and therefore cannot itself prove exclusion of) anything
`.dockerignore` already stripped, and it cannot prevent allowed
marker-bearing content from entering the builder context or BuildKit's
source cache — only from reaching a local `COPY`/`ADD` layer or the
resulting image.

## Out of scope

Route-probe egress policy, local agent transport policy, conversion of
embedded URL credentials into managed tokens, dependency scanning, SBOMs,
action/base-image digest pinning, reproducible image output, network
default-deny policy, `wss://` enforcement, and arbitrary secret-content
detection beyond the defined path classes and marker.

## E2E plan

1. Configure a repository with an existing local cache whose
   `remote.origin.url`/`pushurl` carry embedded HTTP(S) credentials, and a
   configured URL that is either clean or itself rejected. Run a scan.
   Expect: the cache is scrubbed before any credential validation or
   network attempt (`internal/scanner` tests, including the required
   pre-change fixture, exercise this against real `git` subprocesses; no
   mocked Git layer exists in this codebase).
2. Build the real `Dockerfile` through the `build` stage and through the
   full default target, exercising the actual multi-stage dependency graph
   with real BuildKit.
3. Build a real, temporary fixture context (a full copy of the build-context
   inputs, plus injected forbidden-path files and a marker-bearing allowed
   file) through `docker build --target build` and confirm rejection before
   any local copy, gated behind `GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1`.

## Observed results

- Repository sync against an existing credentialed cache: origin scrubbed
  first every time, confirmed via direct `git config --get-all` inspection
  after both successful and rejected/failed sync attempts.
- `docker build --target build` and the full default build both succeed
  against the real repository content (see Verification).
- The negative Docker fixture (forbidden paths + marker) fails the build at
  the `context-audit` stage before the `ui`/`build` stages reach their first
  local copy (confirmed: `go mod download`/`npm ci` steps never start), and
  the fixture's forbidden-fixture sentinel value never appears anywhere in
  build output.

## Verification

All commands below were run from the worktree.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/config ./internal/app ./internal/scanner ./internal/ci -count=1
ok  	github.com/example/gitops-dashboard/internal/config	0.056s
ok  	github.com/example/gitops-dashboard/internal/app	1.069s
ok  	github.com/example/gitops-dashboard/internal/scanner	1.437s
ok  	github.com/example/gitops-dashboard/internal/ci	1.809s
```

```text
$ GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1 GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/ci -run '^TestDockerBuildContextPolicy$' -count=1
ok  	github.com/example/gitops-dashboard/internal/ci	1.341s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.015s
ok  	github.com/example/gitops-dashboard/internal/alerter	0.324s
ok  	github.com/example/gitops-dashboard/internal/app	1.213s
ok  	github.com/example/gitops-dashboard/internal/auth	0.009s
ok  	github.com/example/gitops-dashboard/internal/ci	2.301s
ok  	github.com/example/gitops-dashboard/internal/config	0.027s
ok  	github.com/example/gitops-dashboard/internal/core	0.014s
ok  	github.com/example/gitops-dashboard/internal/dockerapi	0.013s
ok  	github.com/example/gitops-dashboard/internal/environment	0.005s
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	5.950s
ok  	github.com/example/gitops-dashboard/internal/parser	0.030s
ok  	github.com/example/gitops-dashboard/internal/routetarget	0.019s
ok  	github.com/example/gitops-dashboard/internal/sanitizer	0.011s
ok  	github.com/example/gitops-dashboard/internal/scanner	1.367s
ok  	github.com/example/gitops-dashboard/internal/storage	7.144s
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]
```

```text
$ docker build --target build --build-arg VERSION=t060-test --build-arg COMMIT=t060-test --build-arg BUILD_DATE=2026-07-12T00:00:00Z -t gitops-dashboard:t060-build-context .
...
#31 exporting to image
#31 writing image sha256:fdeec55c6a33823ce533b2bd6ce0414f765e275915f8682e8101594d44c71b21 done
#31 naming to docker.io/library/gitops-dashboard:t060-build-context done
```

A full default build (all stages) was also run and produced a working image;
`docker run --rm gitops-dashboard:t060-full -version` printed the expected
`gitops-dashboard t060-test (commit t060-test, built 2026-07-12T00:00:00Z)`.

```text
$ make check
...
16 passed (17.8s)
bash scripts/release_test.sh
ok  	github.com/example/gitops-dashboard/cmd/release	(cached)
release binary clean-clone invocation passed
```

`make check` ran gofmt, the UI build/lint/typecheck, `go vet`, the full Go
test suite, Playwright (16/16 passed), and `scripts/release_test.sh`, all
green.

```text
$ git diff --check
```

No output; exited 0.

## Documentation sweep

- `docs/vision.md`: reviewed; no change. Product vision is unaffected by
  implementation-level credential and build-context hardening.
- `docs/requirements.md`: reviewed; no change. Its existing Security section
  ("Secrets, PATs, SSH keys, kubeconfigs... must be treated as sensitive";
  "Logs must avoid printing credentials or secret values") already states
  the product constraint this task enforces more strictly.
- `docs/tech_stack.md`: reviewed; no change. No stack decision changed.
- `docs/implementation_plan.md`: reviewed; no change.
- `docs/task_acceptance_criteria.md`: reviewed; no change. The shared
  criteria already require secret redaction and E2E/automated test evidence,
  which this task record supplies.
- `docs/deployment.md`: reviewed; no change. The container remains a single
  image with the same runtime contract; the internal Dockerfile stage
  structure is not part of its documented interface.
- `docs/versioning.md`, `docs/discovery.md`: reviewed; no change.
- `docs/tasks/TASK-0047-ci-publish-wedge-hotfix.md`: reviewed; no change.
  Its `$BUILDPLATFORM` pin on the `ui` stage and CI job timeouts are
  untouched by this task.
- `docs/tasks/tracker.md`: updated to add the `TASK-0060` row and advance
  `Next Task ID`.

## Maintainability sweep

- The new `internal/scanner/credentials.go` file holds the entire new
  credential-scrub/validation surface as a cohesive unit, separate from the
  pre-existing `scanner.go` sync orchestration it is called from.
- `gitAuth` and `reconcileConfiguredOrigin` replace the former
  `migrateRemote`/`tokenFreeRemoteURL` pair with names that describe their
  post-scrub role (credential resolution/policy vs. origin reconciliation)
  rather than the credential-stripping behavior now owned by
  `scrubCachedOriginCredentials`.
- The Docker context-audit script is a single narrow shell script performing
  exactly two checks; it is not duplicated in Go, and the new
  `internal/ci` test derives its assertions from parsing the checked-in
  Dockerfile/`.dockerignore` rather than hand-maintaining a parallel list of
  every file the build needs.
- No unrelated refactors were made to `internal/config`, `internal/app`, or
  `internal/scanner` beyond what the deferred-validation contract required.

## Commit evidence

Task base commit: `7d43e993686ff93a19cd2444151decde2b45790a`

Proposed commit subject: `fix(security): scrub cached Git credentials and harden Docker build context (T-060)`

Proposed PR title: `fix(security): scrub cached Git credentials and harden Docker build context (T-060)`
