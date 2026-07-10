# Task Tracker

The external orchestration workspace is the task system of record. This file is
the repository mirror for implementation evidence and task definitions; it does
not override externally managed assignment, priority, or status.

## Status Values

- `Proposed`
- `Ready`
- `In Progress`
- `Blocked`
- `In Review`
- `Done`
- `Superseded`

## Tasks

| ID | Status | Priority | Phase | Task | Definition File | Dependencies | Last Updated | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| TASK-0001 | Done | P0 | Foundation | Project scaffold and developer checks | `docs/tasks/TASK-0001-project-scaffold.md` | None | 2026-06-27 | Checks, smoke test, and container build passed. |
| TASK-0002 | Done | P0 | Foundation | File-based configuration and basic auth | `docs/tasks/TASK-0002-config-and-basic-auth.md` | TASK-0001 | 2026-06-27 | Basic auth smoke test and checks passed. |
| TASK-0003 | Done | P0 | Foundation | SQLite storage and migrations | `docs/tasks/TASK-0003-sqlite-storage.md` | TASK-0001, TASK-0002 | 2026-06-27 | SQLite tables and storage checks passed. |
| TASK-0004 | Done | P0 | Repository Analysis | Repository access and scan orchestration | `docs/tasks/TASK-0004-repository-access-and-scanning.md` | TASK-0002, TASK-0003 | 2026-06-27 | Fixture Git scan and scanner checks passed. |
| TASK-0005 | Done | P0 | Repository Analysis | Docker Compose discovery and parsing | `docs/tasks/TASK-0005-compose-discovery-and-parsing.md` | TASK-0004 | 2026-06-27 | Compose parser and scanner fixture checks passed. |
| TASK-0006 | Done | P0 | Repository Analysis | Kubernetes manifest discovery and parsing | `docs/tasks/TASK-0006-kubernetes-manifest-parsing.md` | TASK-0004 | 2026-06-27 | Kubernetes parser and scanner fixture checks passed. |
| TASK-0007 | Done | P0 | Inventory | Normalized inventory and environment inference | `docs/tasks/TASK-0007-inventory-and-environments.md` | TASK-0005, TASK-0006 | 2026-06-27 | Inventory and environment inference checks passed. |
| TASK-0008 | Done | P0 | Dashboard | Dashboard API and frontend views | `docs/tasks/TASK-0008-dashboard-api-and-frontend.md` | TASK-0007 | 2026-06-27 | Dashboard API, frontend build, and handler checks passed. |
| TASK-0009 | Done | P0 | Monitoring | Kubernetes live monitoring | `docs/tasks/TASK-0009-kubernetes-live-monitoring.md` | TASK-0006, TASK-0007 | 2026-06-27 | Fake dynamic-client status checks passed. |
| TASK-0010 | Done | P0 | Monitoring | Docker live monitoring and remote agent | `docs/tasks/TASK-0010-docker-monitoring-and-agent.md` | TASK-0005, TASK-0007 | 2026-06-27 | Docker health mapping and agent-auth checks passed. |
| TASK-0011 | Done | P0 | Release | Single-container packaging and deployment docs | `docs/tasks/TASK-0011-packaging-and-deployment.md` | TASK-0008, TASK-0009, TASK-0010 | 2026-06-27 | Image build and deployment docs passed. |
| TASK-0012 | Done | P0 | MVP Hardening | End-to-end MVP validation and documentation sweep | `docs/tasks/TASK-0012-mvp-validation-and-doc-sweep.md` | TASK-0011 | 2026-06-27 | MVP E2E validation, checks, image build, doc sweep, and maintainability fixes passed. |
| TASK-0013 | Done | P0 | Quality | Playwright UI verification | `docs/tasks/TASK-0013-playwright-ui-verification.md` | TASK-0012 | 2026-06-27 | Browser E2E target verifies scan, dashboard rendering, detail view, and console cleanliness. |
| TASK-0014 | Done | P0 | Quality | Complete Playwright UI feature coverage | `docs/tasks/TASK-0014-complete-playwright-ui-feature-coverage.md` | TASK-0013 | 2026-06-27 | Expanded browser coverage for all dashboard controls, health states, metrics, filtering, live status, and error display. |
| TASK-0015 | Done | P1 | Release | CI versioning process | `docs/tasks/TASK-0015-ci-versioning-process.md` | TASK-0011 | 2026-07-08 | Build metadata, SemVer CI tags, OCI labels, and desired-versus-observed image comparison implemented. |
| TASK-0026 | In Review | P1 | Release | SemVer allocator and manual release | `docs/tasks/TASK-0026-semver-allocator-and-manual-release.md` | TASK-0015 | 2026-07-10 | Guarded allocator, release-tag publication, and release-path E2E coverage. |
| TASK-0027 | In Review | P1 | Release | Inline auto-patch release on main | `docs/tasks/TASK-0027-inline-main-release.md` | TASK-0026 | 2026-07-10 | Serialized inline release allocation, immutable image reuse, and display-name-aware dispatch integration. |
| TASK-0028 | In Review | P1 | Release | Continuous versioning convergence and rollout | `docs/tasks/TASK-0028-continuous-versioning-convergence.md` | TASK-0027 | 2026-07-10 | Queued-run channel convergence, stale latest guard, operator runbook, and continuous-model documentation. |
| TASK-0030 | In Review | P1 | Route Discovery | Explicit Compose port evidence | `docs/tasks/TASK-0030-route-discovery-correctness.md` | TASK-0028 | 2026-07-10 | Static-address port-evidence rules, IPv6-safe routes, and discovery reference. |
| TASK-0047 | In Review | P1 | CI Reliability | CI publish wedge hotfix | `docs/tasks/TASK-0047-ci-publish-wedge-hotfix.md` | TASK-0015 | 2026-07-10 | Native UI build platform, bounded CI jobs, and exact Docker variable-reference guards. |

## Next Task ID

`TASK-0031`

Reserved-task traceability:

- TASK-0016 → external T-016, delivered, commit `d0cad98` (feat(server): operability hardening)
- TASK-0017 → external T-017, delivered, commit `9168b71` (build(make): non-mutating check)
- TASK-0018 → external T-018, planned externally (dependency & supply chain), not yet delivered — ID reserved
- TASK-0019 → external T-019, delivered, commit `71bc50b` (refactor(structure))
- TASK-0020 → external T-020, delivered, commit `f36a8b5` (perf(monitor): scheduler robustness)
- TASK-0021 → external T-021 (alerting foundations), in progress externally — ID reserved
- TASK-0022–0024 → external T-022…T-024 (alerting producers/sinks), planned externally — IDs reserved
- TASK-0025 → external T-025, delivered, commit `a98dd6a` (fix(dockerapi): Container.Health)

In-repository records mirror external orchestration IDs from TASK-0026 onward.
