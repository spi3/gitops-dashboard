# Implementation Plan

## Purpose

This document defines how project work is broken into tasks, tracked, executed,
verified, and closed. The goal is to keep implementation aligned with the
project vision while preserving GitOps-style traceability: task definitions live
as files, status is tracked explicitly, and completed work leaves evidence of
testing and documentation updates.

## Source Documents

Implementation work should be guided by:

- `docs/vision.md`
- `docs/requirements.md`
- `docs/tech_stack.md`
- `docs/task_acceptance_criteria.md`
- Individual task definition files under `docs/tasks/`
- The task tracker at `docs/tasks/tracker.md`

If implementation reveals that a source document is wrong, incomplete, or out
of date, the task must include the relevant documentation update.

## Task Structure

Each task must be defined in its own Markdown file under `docs/tasks/`.

Task files should use this naming pattern:

```text
docs/tasks/TASK-0001-short-description.md
docs/tasks/TASK-0002-short-description.md
```

Rules:

- The tracker must not be the only place a task is defined.
- The tracker records status and points to the task definition file.
- Task files own the scope, context, requirements, acceptance criteria, and
  verification evidence.
- Task files should be small enough that a contributor can understand the work
  without reading unrelated tasks.
- Large work should be split into multiple task files with explicit
  dependencies.

## Tracker

Use `docs/tasks/tracker.md` as the single task tracker.

The tracker should include:

- Task ID
- Status
- Priority
- Phase
- Short title
- Definition file location
- Dependencies
- Last updated date
- Notes

The tracker should not duplicate the full task definition. It is an index and
status board.

## Status Values

Use these task statuses:

- `Proposed`: task idea exists, but scope is not ready.
- `Ready`: task is defined well enough to implement.
- `In Progress`: implementation has started.
- `Blocked`: work cannot continue without a decision or external dependency.
- `In Review`: implementation is complete and verification is underway or under
  review.
- `Done`: implementation and all acceptance criteria are complete.
- `Superseded`: task was replaced by another task or is no longer relevant.

Only mark a task `Done` when the task-specific acceptance criteria and the
shared criteria in `docs/task_acceptance_criteria.md` are complete.

## Creating A Task

When creating a task:

1. Choose the next numeric task ID from `docs/tasks/tracker.md`.
2. Create a new task definition file under `docs/tasks/`.
3. Add the task to `docs/tasks/tracker.md`.
4. Link the task to relevant requirements, design decisions, or prior tasks.
5. Define task-specific acceptance criteria.
6. Confirm that the shared acceptance criteria apply.

Task definition template:

```markdown
# TASK-0000: Short Title

## Status

Proposed

## Summary

One or two paragraphs describing the task and the user/system value.

## Context

Relevant links to vision, requirements, tech stack, previous tasks, or design
notes.

## Scope

- In-scope item
- In-scope item

## Out Of Scope

- Explicit non-goal
- Explicit non-goal

## Dependencies

- `TASK-0000`, if any

## Implementation Notes

Important technical direction, constraints, or known risks.

## Task-Specific Acceptance Criteria

- Criterion specific to this task
- Criterion specific to this task

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path
- Unit/integration tests
- Build/lint/format commands
- Documentation sweep targets
- Maintainability review focus
- Conventional commit type and summary

## Verification Evidence

Fill this in before moving the task to `Done`.

- End-to-end test result:
- Automated test result:
- Build result:
- Lint result:
- Formatting result:
- Documentation sweep result:
- Maintainability sweep result:
- Conventional commit:
- Notes or exceptions:
```

## Implementing A Task

Before implementation:

- Read the task definition.
- Read the relevant source documents.
- Confirm dependencies are complete or intentionally bypassed.
- Confirm the task is still consistent with the current vision, requirements,
  and tech stack.

During implementation:

- Keep changes scoped to the task.
- Prefer established project patterns.
- Update task notes when scope changes.
- Add or update tests as behavior is implemented.
- Update documentation when decisions, behavior, or usage changes.

Before closing:

- Run the full verification plan.
- Complete the shared acceptance criteria.
- Record verification evidence in the task file.
- Create a git commit using Conventional Commits syntax.
- Update the tracker status and last updated date.

## Document Sweep

Every implementation task must include a document sweep. At minimum, review:

- `docs/vision.md`
- `docs/requirements.md`
- `docs/tech_stack.md`
- `docs/implementation_plan.md`
- `docs/task_acceptance_criteria.md`
- The task definition file
- Any related task files

If the task changes behavior, architecture, packaging, configuration, testing,
or operational expectations, update the relevant docs in the same task.

## Maintainability Sweep

Every implementation task must include a maintainability sweep before it is
closed. The sweep should check:

- Code is organized around clear module boundaries.
- Names are clear and consistent.
- Behavior is covered by appropriate tests.
- Error handling is explicit and useful.
- Credentials and secret values are not logged or exposed.
- New abstractions remove real complexity.
- Unnecessary coupling and duplication are avoided.
- Documentation explains operationally important behavior.

## Definition Of Done

A task is done when:

- The task-specific acceptance criteria are complete.
- The shared acceptance criteria are complete.
- Full end-to-end testing has been run and passed.
- Required build, lint, formatting, and test checks pass.
- The documentation sweep is complete and relevant updates are made.
- The maintainability sweep is complete and issues are addressed.
- Verification evidence is recorded in the task file.
- A git commit exists using Conventional Commits syntax.
- `docs/tasks/tracker.md` is updated.
