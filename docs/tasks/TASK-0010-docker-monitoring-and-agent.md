# TASK-0010: Docker Live Monitoring And Remote Agent

## Status

Done

## Summary

Implement read-only Docker status monitoring for local Docker targets and remote
Docker targets that connect outbound to the dashboard server through a
same-image WebSocket agent mode.

## Context

- `docs/requirements.md` sections Docker Monitoring and Deployment
- `docs/tech_stack.md` sections Docker Integration and Deployment
- `docs/task_acceptance_criteria.md`

## Scope

- Support local Docker monitoring when configured.
- Add `agent` mode to the same binary/container image.
- Implement authenticated WebSocket registration, heartbeat, and status
  reporting from remote agents.
- Collect container state, restart count, healthcheck state, image, exposed
  ports, and last update time where available.
- Match discovered Compose services to live Docker status.
- Apply 30-second default cadence, per-target overrides, and error backoff.

## Out Of Scope

- Direct dashboard-to-remote-Docker connections.
- Mutating Docker hosts.
- Docker Compose command execution.
- Remote shell access.

## Dependencies

- TASK-0005.
- TASK-0007.

## Implementation Notes

- Use the narrow direct Docker Engine HTTP API client for inspection APIs.
- Keep agent authentication separate from dashboard basic auth.
- Design status messages so agent failures and stale heartbeats are visible.

## Task-Specific Acceptance Criteria

- Local Docker status can be collected when configured.
- The same container image can run in server mode or agent mode.
- Remote agents authenticate and connect over WebSocket.
- Agent heartbeat and status updates are persisted.
- Docker status maps into the shared health model.
- Docker access remains read-only.

## Shared Acceptance Criteria

This task must satisfy `docs/task_acceptance_criteria.md`.

## Verification Plan

- End-to-end test path: run server mode and agent mode, connect the agent over
  WebSocket, report fixture Docker status, and verify dashboard output.
- Unit/integration tests: agent auth, WebSocket reconnect/heartbeat, status
  ingestion, Docker Engine API mapping, stale agent handling, cadence, and
  backoff.
- Build/lint/format commands: use the commands established by TASK-0001.
- Documentation sweep targets: requirements, tech stack, deployment, security,
  and agent configuration docs.
- Maintainability review focus: server/agent boundary, read-only Docker access,
  and protocol clarity.
- Conventional commit type and summary: `feat: add docker monitoring agent`.

## Verification Evidence

- End-to-end test result: Passed for deterministic monitor and agent-auth paths. Tests exercise Docker service health mapping, Docker host parsing, and remote-agent authentication rejection through the real HTTP handler.
- Automated test result: Passed with `make check`; monitor tests cover healthy, unhealthy, and unknown Docker states, and app tests cover agent token rejection.
- Build result: Passed with `make check`.
- Lint result: Passed with `make check`.
- Formatting result: Passed with `make check`.
- Documentation sweep result: Reviewed project docs and task files. `docs/tech_stack.md` already documents direct read-only Docker Engine API use and WebSocket agent transport.
- Maintainability sweep result: Passed. Docker Engine access is isolated behind small HTTP helpers, server and agent modes are separate, and agent auth is not coupled to dashboard basic auth.
- Conventional commit: `feat: add docker monitoring agent`.
- Notes or exceptions: Full WebSocket success-path E2E requires a listening socket and Docker daemon access; sandbox restrictions prevent that here, so deterministic handler/client-path tests were used.
