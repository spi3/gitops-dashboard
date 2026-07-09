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
- `POST /api/monitor-overrides`

State-changing API calls must include `X-GitOps-Dashboard-CSRF: 1`. The
same-origin UI sends it automatically; curl and other non-browser clients must
set it explicitly. Requests with cross-site `Origin` or `Sec-Fetch-Site` headers
are rejected. Additional trusted browser origins can be listed in
`server.allowedOrigins`; `Sec-Fetch-Site: cross-site` is still rejected. Leave
`server.allowedOrigins` unset for strict same-host behavior.

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
export GITOPS_DASHBOARD_ADMIN_HASH="$(htpasswd -nbB admin 'change-me' | cut -d: -f2-)"
export GITOPS_DASHBOARD_AGENT_TOKEN="$(openssl rand -hex 32)"
export DOCKER_SOCKET_GID="$(
  docker run --rm --entrypoint stat \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    gitops-dashboard:latest -c '%g' /var/run/docker.sock
)"
docker compose -f examples/docker-compose.yaml up -d
docker compose -f examples/docker-compose.yaml logs -f dashboard docker-agent
```

Replace `change-me` before use. The Compose example enables basic auth for
`admin` with the bcrypt hash from `GITOPS_DASHBOARD_ADMIN_HASH`, binds the
dashboard port to `127.0.0.1:8080`, and runs the server and agent processes as
UID/GID `10001`. If `htpasswd` is not installed locally, generate the same kind
of bcrypt hash with Docker:

```sh
docker run --rm httpd:2.4-alpine htpasswd -nbB admin 'change-me' | cut -d: -f2-
```

On startup, the image briefly runs its entrypoint as root to repair ownership
of `/data` when upgrading an older root-owned Compose volume, then execs the
dashboard as UID/GID `10001`. It does not change `/ssh` or `/kube`; SSH private
keys should be owned by UID `10001` with mode `0600`, and kubeconfigs should be
owner- or group-readable by UID/GID `10001`.

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

Mounting `/var/run/docker.sock` gives Docker API access to that host even when
the bind mount is marked read-only. The example keeps the agent non-root by
adding only the socket's numeric group ID as containers see it. Use a Docker
socket proxy with an allowlist of read-only endpoints when you need a tighter
boundary.

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

This runs the verification suite in a temporary copy of the current tree so
generated UI assets, the Go binary, and Playwright reports are not written into
the checkout. It includes read-only formatting checks, frontend lint/typecheck,
Go vet, Go tests, production frontend build, Go binary build, and Playwright UI
verification. To rewrite Go formatting explicitly, run `make format`.

Common targets:

```sh
make build
make format
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

`examples/config.dev.yaml` allows `http://127.0.0.1:5173` and
`http://localhost:5173`, plus `http://regula1.lan:5173` for the Vite
`allowedHosts` entry, in `server.allowedOrigins` so Vite-proxied POST actions
work with the CSRF/origin guard during hot reload. The Vite proxy defaults to
`http://127.0.0.1:18080`. Override it when the Go server is listening elsewhere:

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
