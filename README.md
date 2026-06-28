# Docker Monitor

Docker Monitor is a read-only GitOps-inspired dashboard for homelab and
self-hosted repositories. It scans configured Git repositories, discovers Docker
Compose and generic Kubernetes manifests, builds a normalized service inventory,
and displays live health/status when Docker or Kubernetes monitoring targets are
configured.

## Current Capabilities

- GitHub repositories with personal access tokens.
- Generic public Git repositories.
- Generic private Git repositories with SSH keys.
- Docker Compose file discovery and parsing.
- Generic Kubernetes YAML manifest discovery and parsing.
- SQLite-backed repository, scan, inventory, and status storage.
- Basic authentication with hashed passwords.
- File-based configuration.
- Read-only Docker monitoring through local/remote Docker Engine HTTP API
  targets.
- Remote Docker monitoring agent mode over WebSocket.
- Read-only Kubernetes monitoring with mounted kubeconfig files.
- React dashboard with repository overview, scan history, service catalog,
  service detail, runtime filtering, static warnings, health metrics, and live
  status rows.
- Playwright browser coverage for the dashboard workflow.

## Quick Start

For local development without auth:

```sh
make build
./gitops-dashboard -config examples/config.dev.yaml
```

Then open:

```text
http://127.0.0.1:18080
```

Useful endpoints:

- `GET /healthz`
- `GET /readyz`
- `GET /api/summary`
- `POST /api/scan`
- `POST /api/monitor`

## Configuration

Configuration is file based for v1. The dashboard does not edit configuration
through the UI.

Use:

- `examples/config.dev.yaml` for local no-auth development.
- `examples/config.basic.yaml` for basic-auth local testing.
- `examples/docker-compose.yaml` for the server and remote-agent deployment
  shape, backed by `examples/compose-config/config.yaml` and
  `examples/compose-config/agent.yaml`.

Repository credentials should be mounted through files or environment variables.
Secret values from repositories and manifests are not rendered back in the UI.

## Container Modes

Server mode is the default:

```sh
gitops-dashboard -config /config/config.yaml
```

Agent mode runs the remote Docker monitor:

```sh
gitops-dashboard -mode agent -config /config/agent.yaml
```

In Compose, the same image can run both modes:

```sh
docker build -t gitops-dashboard:latest .
docker compose -f examples/docker-compose.yaml up
```

The agent connects outbound to `/api/agents/connect` over WebSocket and sends an
`X-Agent-Token` value. The server must accept the same token through
`auth.agent.tokens` or a Docker target `agentToken`. Expected mounts, token
configuration, and the full agent config shape are documented in
[docs/deployment.md](docs/deployment.md).

## Development

Install Node dependencies:

```sh
npm install
```

Install the Playwright Chromium browser bundle:

```sh
npm run test:e2e:install
```

Run all checks:

```sh
make check
```

This runs formatting checks, frontend lint/typecheck, Go vet, Go tests,
production frontend build, Go binary build, and Playwright UI verification.

Common targets:

```sh
make build
make test
make ui-e2e
```

Frontend hot reload uses Vite as the browser entry point and proxies API
requests to the Go server. Run these in two terminals:

```sh
make dev-server
make dev-ui
```

Then open:

```text
http://127.0.0.1:5173
```

The Vite proxy defaults to `http://127.0.0.1:18080`. Override it when the Go
server is listening elsewhere:

```sh
make dev-ui VITE_API_TARGET=http://127.0.0.1:19090
```

## Documentation

- [Vision](docs/vision.md)
- [Requirements](docs/requirements.md)
- [Tech Stack](docs/tech_stack.md)
- [Deployment](docs/deployment.md)
- [Implementation Plan](docs/implementation_plan.md)
- [Task Tracker](docs/tasks/tracker.md)

## Status

The current repository contains the v1 MVP implementation and task evidence
through TASK-0014. See [docs/tasks/tracker.md](docs/tasks/tracker.md) for the
implementation history and verification evidence.
