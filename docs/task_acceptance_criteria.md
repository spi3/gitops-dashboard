# Task Acceptance Criteria

## Purpose

This document defines the shared acceptance criteria that apply to every task.
Each task file may add task-specific criteria, but it must not remove these
shared expectations.

## Required For Every Task

### Task Scope

- The task has a dedicated file under `docs/tasks/`.
- The task is listed in `docs/tasks/tracker.md`.
- The task file clearly defines scope, out-of-scope work, dependencies, and
  task-specific acceptance criteria.
- Any scope change discovered during implementation is reflected in the task
  file before completion.

### End-To-End Testing

- Full end-to-end testing must be defined in the task file.
- Full end-to-end testing must be run before the task is marked `Done`.
- The E2E path must exercise the actual user-facing or system-facing workflow
  affected by the task.
- User-facing UI changes must include Playwright browser verification for the
  affected workflow.
- For server or agent behavior, E2E testing should cover the relevant process
  boundary, configuration loading, API behavior, persistence, and dashboard
  result where applicable.
- For documentation-only tasks, E2E testing means reading the affected
  documentation flow from entry point to outcome and confirming that a user can
  follow it without missing context.
- E2E results must be recorded in the task file.

### Automated Tests

- Relevant unit tests must be added or updated.
- Relevant integration tests must be added or updated when behavior crosses
  module, process, storage, network, or runtime boundaries.
- Existing tests must continue to pass.
- If a test type is not applicable, the task file must explain why.

### Build, Lint, And Formatting

- Build checks must run and pass.
- Lint checks must run and pass.
- Formatting checks must run and pass.
- Type checks must run and pass when the stack includes type checking.
- The exact commands and results must be recorded in the task file.

### Best Practices

- Code follows the stack decisions in `docs/tech_stack.md`.
- Behavior satisfies the product constraints in `docs/requirements.md`.
- The implementation remains read-only toward Git repositories, Docker hosts,
  and Kubernetes clusters unless a future requirement explicitly changes that.
- Structured parsers and official clients are preferred over ad hoc parsing for
  Git, YAML, Docker, Kubernetes, and API behavior.
- Credentials and secret values are not logged, rendered, or stored
  unnecessarily.
- Errors include enough context for diagnosis without leaking secrets.
- Configuration remains file based for v1.

### Documentation Sweep

- A full document sweep has been completed.
- Relevant updates have been made to:
  - `docs/vision.md`
  - `docs/requirements.md`
  - `docs/tech_stack.md`
  - `docs/implementation_plan.md`
  - `docs/task_acceptance_criteria.md`
  - Relevant task files under `docs/tasks/`
- Documentation changes are included in the same task when behavior,
  architecture, configuration, packaging, testing, or operations change.
- The task file records which documents were reviewed and which were updated.

### Maintainability Sweep

- A maintainability sweep has been completed before closing the task.
- Module boundaries are clear.
- New code is cohesive and avoids unrelated refactors.
- Duplication is removed when it creates meaningful maintenance cost.
- New abstractions are justified by real complexity or established project
  patterns.
- Naming is clear and consistent.
- Comments are used only where they clarify non-obvious behavior.
- Tests are readable and maintainable.
- Operational behavior is diagnosable through logs, errors, or status surfaces.
- The task file records the maintainability sweep result.

### Security And Privacy

- Secret values are redacted from logs, UI, persisted records, and test output.
- Authentication and credential behavior is covered by tests where relevant.
- Basic auth behavior is preserved for dashboard access.
- Remote Docker agent authentication is preserved where agent behavior is
  touched.
- Kubernetes access remains read-only.
- Docker access remains read-only.

### Tracker And Evidence

- The task tracker points to the correct task definition file.
- The task status is accurate.
- Dependencies are recorded and updated.
- Verification evidence is recorded in the task file.
- Known limitations or follow-up tasks are documented before closing.

### Git Commit

- The completed task must be committed to git before it is marked `Done`.
- The commit message must follow Conventional Commits syntax.
- The commit type must match the work, such as `feat`, `fix`, `docs`, `test`,
  `refactor`, `build`, `ci`, or `chore`.
- The commit subject must be concise, imperative, and specific to the task.
- The commit body should reference the task ID and summarize verification
  evidence when useful.
- The task file must record the commit hash or commit subject before the task is
  marked `Done`.

## Completion Rule

A task must not be marked `Done` until all applicable shared and task-specific
acceptance criteria are complete. If a criterion is intentionally not applicable
for a task, the task file must explain why and record the closest reasonable
verification performed.
