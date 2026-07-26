# TASK-0069: Repository agent guidance — root AGENTS.md

## Status

In Review

## Scope

Repository root documentation only: a new `AGENTS.md` and tracking of the
existing one-line `CLAUDE.md` that imports it. No build, lint, test, CI, or
source behavior changes.

`CLAUDE.md` already contained exactly `@AGENTS.md` but was untracked, and no
`AGENTS.md` existed, so the import dangled and agents opening this repository
received no guidance at all. `.agents/` and `.codex/` are empty directories.

## Dependencies

None.

## Task base commit

`f841624a0066d0b1cfe36f6f3fc5c6a946e9d0cb` (main, after TASK-0068/v0.0.19).

## Requirements

- [x] `AGENTS.md` exists at the repository root and is the only agent-facing
      convention document; `CLAUDE.md` stays a one-line `@AGENTS.md` import with
      no duplicated content. Both are non-ignored and land together, so the
      import resolves in a fresh clone.
- [x] It follows the section shape used by peer repositories: `# Repository
      Guidelines` with *Project Structure & Module Organization*, *Build, Test,
      and Development Commands*, *Coding Style & Naming Conventions*, *Testing
      Guidelines*, *Commit & Pull Request Guidelines*, and *Security &
      Configuration Tips*, plus a *Scope & Operating Model* section after the
      structure section.
- [x] Using only `AGENTS.md`, a reader can perform first-time setup and pick the
      right gate for a Go-only, UI, or docs-only change without opening the
      `Makefile`, `package.json`, or the CI workflow.
- [x] Layout coverage includes the single binary's dual server/agent roles, the
      `internal/ui/dist` embedding path (so a UI change needs a rebuild), and
      every top-level directory an implementer edits.
- [x] Every command, path, and behavioral claim is verified true at this commit.
      Specifically, `make check` is documented as non-mutating (it runs in a
      temp copy of the tracked-and-untracked, non-ignored manifest) and
      `make format` is documented as the only mutating command — the stale
      "`make check` runs `gofmt -w`" claim is not reproduced. `npm run format`
      is documented as `scripts/check-format.mjs`, a trailing-whitespace check,
      not Prettier.
- [x] Non-negotiable constraints are stated: read-only toward Git, Docker, and
      Kubernetes; secrets never logged, persisted, rendered, or added to the
      build context, with `internal/sanitizer` as the reuse point and
      `scripts/docker-context-audit.sh` as the build-time guard; release tags
      and images are never deleted, recreated, or retargeted; `main` must stay
      releasable because every push releases.
- [x] The operating scope that bounds review effort (one operator, one process
      per SQLite database, restart overlap supported, continuous multi-replica
      not) is stated and sourced from in-repo `docs/requirements.md`.
- [x] The testing bar is stated: suites and how to run them, no live Docker
      daemon / cluster / network / real credentials in default tests, success
      path plus the realistic motivating failure, a pre-change fixture for
      persisted-format changes, Playwright for user-visible UI behavior, and
      recording exact commands in the task record.
- [x] Commit and evidence conventions are stated: Conventional Commits with a
      subsystem scope and trailing task ID, no co-author trailers, the
      `docs/tasks/` record plus `tracker.md` row, and CI as the completion gate.
- [x] No external project-management state (task status, priority, assignment,
      or backlog rows) appears in `AGENTS.md`; it names the external
      orchestration workspace as the system of record and points at
      `docs/tasks/` as the in-repo mirror. Referring to the *format* of task IDs
      in commit subjects is permitted and is the only ID-shaped content present.
- [x] `AGENTS.md` passes the repository's formatting gate and `make check` is
      green with it present.
- [x] This record exists with scope, verification, and evidence, plus a
      `docs/tasks/tracker.md` row.

## Out of scope

Nested per-directory `AGENTS.md` files; rewriting or relocating existing `docs/`
content; any build, lint, test, or CI change; importing an external review-bar
document into the repository; renaming the `github.com/example/gitops-dashboard`
module path.

## Verification

Commands run at the task base commit with the change applied:

```sh
npm run format
git ls-files --cached --others --exclude-standard | grep -Fx AGENTS.md
git ls-files --cached --others --exclude-standard | grep -Fx CLAUDE.md
grep -Fxq '@AGENTS.md' CLAUDE.md
[ "$(wc -l < CLAUDE.md)" -le 2 ]
grep -nE '^\s*\|?\s*(T-[0-9]{3})\b.*\b(ready|dispatched|blocked|backlog|P[0-3])\b' AGENTS.md
grep -n 'gofmt -w' AGENTS.md | grep -v 'make format'
make check
```

## Observed evidence

- `npm run format`: exit 0, no findings — `AGENTS.md` and this record are both
  inside `scripts/check-format.mjs`'s `.md` coverage.
- Manifest membership: both `AGENTS.md` and `CLAUDE.md` appear in
  `git ls-files --cached --others --exclude-standard`, so both enter the
  `make check` and release-test file manifests. `CLAUDE.md` is one line long and
  is exactly `@AGENTS.md`.
- Prohibited-content scans: both greps produce **no output** against `AGENTS.md`.
  Both were validated in the failing direction against a scratch file
  containing `| T-069 | Some task | ready | P2 |` and `run gofmt -w cmd
  internal`, which each matched — so a silent pass is not a vacuous pass. The
  second scan's `make format` exclusion is deliberate: `AGENTS.md` is required
  to document `make format` as the mutating `gofmt -w` entrypoint, and the
  prohibited claim is `gofmt -w` attributed to any other command.
- `make check`: **exit 0**, fully green — `format-check`, `lint`, Go and UI
  tests, `build`, Playwright `16 passed (17.9s)`, and
  `release binary clean-clone invocation passed`.

Documentation-only change: no unit or integration tests apply. End-to-end
verification for a documentation task means reading the flow from entry point to
outcome — `AGENTS.md` was read end to end against the live repository, and every
command it names was confirmed to exist in the `Makefile`, `package.json`, or
`scripts/`, with the setup, gate-selection, and run instructions followed to a
working `make check`.

## Known limitations

`docs/tasks/tracker.md`'s "Next Task ID" field still reads `TASK-0065` and was
already stale before this task (TASK-0066 through TASK-0068 exist). It is left
untouched here: records mirror external orchestration IDs from TASK-0026 onward,
so the field no longer allocates anything.
