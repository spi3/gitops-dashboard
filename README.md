# GitOps Dashboard

GitOps Dashboard is a read-only dashboard for homelab and
self-hosted repositories. It scans configured Git repositories, discovers Docker
Compose and generic Kubernetes manifests, builds a normalized service inventory,
and displays live health/status when HTTP route, Docker, or Kubernetes
monitoring targets or host ping inventories are configured.

## Current Capabilities

- GitHub repositories with personal access tokens.
- Generic public Git repositories.
- Generic private Git repositories with SSH keys.
- Docker Compose file discovery and parsing.
- Generic Kubernetes YAML manifest discovery and parsing.
- SQLite-backed repository, scan, inventory, and status storage.
- Basic authentication with hashed passwords.
- File-based configuration.
- HTTP route checks for discovered service URLs.
- Read-only Docker monitoring through local/remote Docker Engine HTTP API
  targets.
- Remote Docker agent mode over WebSocket for collecting Docker reports and
  showing agent connection/container state.
- Read-only Kubernetes monitoring with mounted kubeconfig files.
- Host ping monitoring from Ansible `hosts.yml` inventories in configured
  repositories.
- React dashboard with at-a-glance status, per-service uptime history from the
  monitors, clickable routes and DNS names discovered in Git, and a detail
  drawer with live check results, Git provenance, and desired-versus-observed
  image version comparison.
- Build metadata exposed through the binary, API, dashboard footer, and
  container labels.
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
- `GET /api/version`
- `GET /api/summary`
- `POST /api/scan`
- `POST /api/monitor`

## Configuration

Configuration is file based for v1. The dashboard does not edit configuration
through the UI.

Use:

- `examples/config.dev.yaml` for local no-auth development.
- `examples/config.basic.yaml` for basic-auth local testing.
- `examples/docker-compose.yaml` for the server and Docker agent deployment
  shape, backed by `examples/compose-config/config.yaml` and
  `examples/compose-config/agent.yaml`.

Repository credentials and runtime/auth secrets can be sourced through mounted
files or environment variables. Secret values from repositories and manifests
are not rendered back in the UI.

```yaml
auth:
  mode: basic
  users:
    - username: admin
      passwordHashEnv: GITOPS_DASHBOARD_ADMIN_HASH
  agent:
    tokenFile: /run/secrets/gitops-dashboard-agent-token
runtime:
  docker:
    - name: compose-docker-agent
      kind: agent
      agentTokenEnv: GITOPS_DASHBOARD_AGENT_TOKEN
agent:
  tokenEnv: GITOPS_DASHBOARD_AGENT_TOKEN
```

Repositories can optionally narrow scanning with path filters. Plain entries
match that file or directory subtree; glob entries support `*` and recursive
`**` matching:

```yaml
repositories:
  - name: kube
    url: https://github.com/spi3/kube
    includePaths:
      - docker_files/serenity
      - clusters/main
    excludePaths:
      - clusters/retired
      - "**/gotk-components.yaml"
```

## Container Modes And Compose

Server mode is the default:

```sh
gitops-dashboard -config /config/config.yaml
```

Agent mode runs the Docker reporter:

```sh
gitops-dashboard -mode agent -config /config/agent.yaml
```

In Docker Compose, the same image can run both modes. The dashboard container
serves the UI/API and owns `/data`; the Docker agent container mounts the host
Docker socket and reports outbound to the dashboard:

```sh
docker build -t gitops-dashboard:latest .
export GITOPS_DASHBOARD_AGENT_TOKEN="$(openssl rand -hex 32)"
docker compose -f examples/docker-compose.yaml up -d
docker compose -f examples/docker-compose.yaml logs -f dashboard docker-agent
```

Pushes to `main` run tests and publish the container image to GitHub Container
Registry as `ghcr.io/spi3/gitops-dashboard:latest` and `sha-<short-sha>`.
Pushing a `vMAJOR.MINOR.PATCH` tag publishes `vMAJOR.MINOR.PATCH`,
`vMAJOR.MINOR`, and `vMAJOR` tags from the same image digest. The versioning
policy for release tags, immutable image pins, and service version inventory is
documented in [docs/versioning.md](docs/versioning.md).

The agent connects outbound to `/api/agents/connect` over WebSocket and sends an
`X-Agent-Token` value. Tokens are sent only by header and are authorized against
configured `kind: agent` Docker target names. Expected mounts, token
configuration, target binding, and the full agent config shape are documented in
[docs/deployment.md](docs/deployment.md).

Agent reports appear on the dashboard's Agents tab with connection state and
reported container state. Reports from configured `kind: agent` Docker targets
also feed per-service health and uptime rows only when a Compose service can be
bound to that target by source path (`docker_files/<target>/...`, where
`<target>` is the configured Docker target name).
Incoming agent report rows include normalized `health` and `restartCount` fields
so service health uses the same normalized container semantics as server-side
docker checks.
Dashboard health rows can also come from direct Docker Engine targets, HTTP
route checks, Kubernetes targets, and host ping targets.

To check a Docker Engine directly from the dashboard server instead of through
an agent, configure a direct Docker target:

```yaml
runtime:
  docker:
    - name: local-docker
      host: unix:///var/run/docker.sock
```

To add host health rows from an Ansible YAML inventory, keep the inventory in a
configured repository and point the ping target at that repo-relative path:

```yaml
repositories:
  - name: kube
    url: https://github.com/spi3/kube
runtime:
  ping:
    - name: homelab-hosts
      repository: kube
      ansibleInventory: infrastructure/inventory/hosts.yml
      interval: 1m
      timeout: 2s
```

The dashboard syncs the named repository, reads `hosts` entries from the
inventory, prefers `ansible_host` when present, creates one `Host` row per
inventory host, and checks each host with the system `ping` command.

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
- [Versioning](docs/versioning.md)
- [Implementation Plan](docs/implementation_plan.md)
- [Task Tracker](docs/tasks/tracker.md)

## Status

The current repository contains the v1 MVP implementation and task evidence
through TASK-0014. See [docs/tasks/tracker.md](docs/tasks/tracker.md) for the
implementation history and verification evidence.
