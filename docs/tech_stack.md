# GitOps Dashboard Tech Stack

## Recommendation

Use a Go backend, React/TypeScript frontend, SQLite database, and a single
container deployment.

This stack fits the project constraints:

- Go produces a small, straightforward single-container service.
- Go has mature Kubernetes, Docker, Git, HTTP, and SQLite libraries.
- React with TypeScript is a practical choice for a dashboard with tables,
  filters, detail pages, and status views.
- SQLite keeps v1 simple for homelab/self-hosted deployments.
- A single container lowers operational friction for the target user.

## Backend

Recommended backend:

- Language: Go
- HTTP API: Go standard library or a small router such as Chi
- Database access: SQLite with typed query support or a lightweight query layer
- Migrations: embedded SQL migrations
- Background jobs: in-process scheduler and worker pool for v1
- Configuration: YAML or TOML file plus environment variable overrides

The backend should own:

- Repository configuration
- Credential loading
- Repository clone/fetch operations
- Static analysis pipelines
- Runtime monitoring integrations
- Normalized service inventory
- Health/status calculation
- API for the frontend
- Basic authentication
- Remote Docker agent registration and status ingestion

## Frontend

Recommended frontend:

- Language: TypeScript
- UI framework: React
- Build tool: Vite
- Styling: CSS modules, plain CSS, or a small utility layer
- Data fetching: generated or hand-written typed API client

The frontend should provide:

- Repository overview
- Service catalog
- Service details
- Runtime target status
- Scan history
- Static analysis warnings
- Health/status indicators
- Read-only visibility into loaded file-based configuration where useful

The frontend should be built into static assets and served by the Go backend so
the final artifact remains one container.

## Storage

Version 1 should use SQLite.

SQLite should store:

- Repository records
- Runtime target records
- Remote Docker agent registrations and heartbeats
- Last scan metadata
- Parsed inventory snapshots
- Normalized services
- Static analysis warnings
- Live health/status results
- Scan and monitoring errors
- Basic audit-style timestamps for important events

Sensitive credentials should not be stored casually in normal application
tables. Prefer mounted files, environment variables, or encrypted-at-rest
storage if persisted credential management becomes necessary.

Postgres can be considered later if multi-user, high-scale, or multi-instance
deployment becomes a requirement.

## Repository Access

GitHub v1 access:

- Personal access token
- Explicit repository configuration
- Read-only API permissions

Generic Git v1 access:

- Public clone URLs
- SSH clone URLs with configured private keys

Implementation options:

- Use the system `git` binary for maximum compatibility.
- Use a Go Git library where it is sufficient and simpler.

Recommendation: start with the system `git` binary behind a narrow internal
interface. It handles real-world SSH, known hosts, credential helpers, and Git
edge cases more predictably than reimplementing those paths early. The narrow
interface keeps a future library swap possible.

## Docker Integration

Docker monitoring must support local and remote targets. Remote Docker
monitoring should use a daemon/agent that runs near the Docker engine and
connects outbound to the dashboard server.

Recommended approach:

- Use the Docker Engine HTTP API directly through a narrow internal client.
- Model Docker monitoring targets as named profiles.
- Support local Unix socket profiles when local Docker monitoring is configured.
- Support remote Docker targets through an authenticated outbound agent
  connection.
- Use WebSocket as the default persistent channel for remote agent status
  updates, with reconnect and heartbeat behavior.
- Keep the agent read-only and limited to Docker inspection APIs.

The direct Engine API client keeps the project compatible with the selected Go
toolchain while preserving a small boundary that can be replaced by an official
client later if dependency constraints change.

Docker integration should be read-only and limited to inspection APIs.

## Kubernetes Integration

Kubernetes monitoring must support local and remote clusters.

Recommended approach:

- Use the official Kubernetes Go client libraries.
- Support mounted kubeconfig files.
- Support selecting kubeconfig contexts.
- Do not require explicit API server/token/CA fields in v1.
- Treat in-cluster configuration as a future deployment mode unless it falls out
  naturally from mounted kubeconfig support.

Kubernetes integration should be read-only and limited to list/watch/get style
operations.

## Host Ping Integration

Host monitoring should support Ansible YAML inventories from configured
repositories and produce ordinary runtime status rows.

Recommended approach:

- Parse repository-relative `hosts.yml` files with the existing YAML parser
  dependency.
- Read inventory `hosts` maps recursively through groups and children.
- Prefer `ansible_host` as the ping address when present.
- Represent each discovered host as a normalized `Host` inventory item.
- Use the system `ping` command behind a narrow internal function so tests can
  inject deterministic reachability results.

Host ping integration should be read-only and limited to reachability checks.

## YAML and Spec Parsing

Recommended approach:

- Use structured YAML parsing rather than ad hoc string parsing.
- Parse Docker Compose into typed internal structs that preserve unknown fields
  where useful.
- Parse Kubernetes manifests through Kubernetes API machinery where possible.
- Support multi-document YAML.
- Treat malformed files as scan errors scoped to the file or repository.

Helm and Kustomize rendering are out of scope for v1, but the parser design
should leave room to add renderers later.

## Authentication

Version 1 should use basic authentication.

Recommended approach:

- Configure one or more username/password pairs through mounted configuration
  files, with environment variable overrides only for deployment convenience.
- Store password hashes, not plaintext passwords.
- Use secure password hashing such as bcrypt or Argon2.
- Require an explicit development-mode setting to run without auth.

OIDC, SSO, and RBAC are future concerns.

## Deployment

Version 1 should ship as a single container.

The container should include:

- Go backend binary
- Built frontend assets
- Required static files
- Runtime dependencies needed for Git operations and ping checks

The container should support mounts for:

- Application configuration
- SQLite database directory
- Repository cache directory
- SSH keys and known hosts
- Kubeconfig files for Kubernetes monitoring
- Remote Docker agent tokens or certificates

The app should expose one HTTP port and use health/readiness endpoints for
container orchestration.

Remote Docker monitoring requires a small daemon/agent deployed near the Docker
engine. Ship the same Go binary and container image with a mode switch so the
project has one primary build artifact. The normal mode runs the dashboard
server; agent mode connects outbound to the dashboard over WebSocket and reports
read-only Docker status.

## Suggested Internal Architecture

Recommended modules:

- `config`: application, repository, runtime target, and auth configuration
- `auth`: basic authentication middleware and password verification
- `git`: clone/fetch abstraction and GitHub integration
- `scanner`: repository scan orchestration
- `compose`: Docker Compose discovery and parsing
- `kubernetes`: manifest parsing and live Kubernetes monitoring
- `docker`: Docker live monitoring
- `agent`: remote Docker daemon connection, heartbeat, and status reporting
- `environment`: folder-name based environment inference
- `inventory`: normalized service model and status calculation
- `storage`: SQLite persistence and migrations
- `api`: HTTP API handlers
- `ui`: frontend application

Keep static analysis and live monitoring as separate pipelines that converge in
the inventory/status model.

Configuration should be file first. The UI should not edit configuration in v1;
it can show loaded repositories, runtime targets, auth state, and agent status
as read-only operational context.

Runtime health checks should default to a 30-second cadence, with per-target
overrides and backoff after repeated connection errors.

## Testing Strategy

Backend tests should cover:

- Compose file detection and parsing
- Kubernetes multi-document manifest parsing
- Normalized service model generation
- Secret redaction
- Scan failure isolation
- Docker and Kubernetes status mapping with mocked clients
- Remote Docker agent authentication and heartbeat handling
- Folder-name environment inference
- Basic authentication behavior
- SQLite migrations

Frontend tests should cover:

- Repository overview rendering
- Service catalog filtering
- Status display states
- Error and unknown-state rendering

Browser end-to-end tests should use Playwright against the built dashboard
server. They should exercise fixture repositories with representative Compose
and Kubernetes manifests, verify the rendered UI workflow, and fail on browser
console warnings or errors.

## Future Stack Considerations

- Postgres if SQLite becomes limiting.
- OIDC when multi-user deployments matter.
- GitHub App support when PAT-based access is too limited.
- Helm/Kustomize renderers when generic manifest parsing is not enough.
- Separate worker process only if in-process jobs become a bottleneck.
