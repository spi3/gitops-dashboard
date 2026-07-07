# Deployment

## Container Modes

The project ships as one container image with two modes:

- `server`: runs the dashboard server, API, scanner, storage, and frontend.
- `agent`: runs a remote Docker monitoring agent that connects outbound to the
  dashboard server over WebSocket.

Server mode is the default.

```sh
gitops-dashboard -config /config/config.yaml
```

Agent mode:

```sh
gitops-dashboard -mode agent -config /config/agent.yaml
```

In Docker Compose, run the same image twice: once as the dashboard server and
once as the Docker agent. The agent does not expose a port. It opens an outbound
WebSocket connection to the dashboard server at `/api/agents/connect`.

## Required Server Mounts

- `/config`: file-based application configuration.
- `/data`: SQLite database and repository cache.
- `/ssh`: optional SSH keys and known hosts for private Git repositories.
- `/kube`: optional kubeconfig files for Kubernetes monitoring.

## Required Agent Mounts

- `/config`: file-based agent configuration.
- `/var/run/docker.sock`: optional local Docker socket when the agent monitors
  the colocated Docker engine.

Mounting the Docker socket gives the container Docker API access on that host.
For stricter deployments, point `agent.docker.host` at a constrained Docker API
proxy instead of mounting the socket directly.

## Docker Compose Agent Example

`examples/docker-compose.yaml` starts both modes from the same image:

```sh
docker build -t gitops-dashboard:latest .
docker compose -f examples/docker-compose.yaml up
```

Pushes to `main` run the GitHub Actions workflow in `.github/workflows/ci.yml`.
After tests pass, the workflow publishes the image to GitHub Container Registry:

- `ghcr.io/spi3/gitops-dashboard:latest`
- `ghcr.io/spi3/gitops-dashboard:sha-<short-sha>`

The Compose file mounts `examples/compose-config` into both containers:

- `config.yaml`: dashboard server config with an accepted agent token.
- `agent.yaml`: remote Docker agent config with the dashboard WebSocket URL,
  target name, matching token, reporting interval, and Docker host.

Before using the example outside local testing, replace
`replace-me-with-a-long-random-agent-token` in both files with the same secret
token.

The dashboard accepts agent connections when the token matches either
`auth.agent.tokens` or a Docker target `agentToken`. The agent sends the token in
the `X-Agent-Token` header.

Minimal agent config:

```yaml
auth:
  mode: dev-no-auth
monitoring:
  defaultInterval: 30s
repositories: []
runtime:
  docker: []
  kubernetes: []
agent:
  serverUrl: ws://dashboard:8080/api/agents/connect
  target: compose-docker-agent
  token: replace-me-with-a-long-random-agent-token
  interval: 30s
  docker:
    host: unix:///var/run/docker.sock
```

The `auth`, `monitoring`, `repositories`, and `runtime` sections are included
because the same configuration loader is used for server and agent mode. Agent
mode uses the `agent` section at runtime.

## Health Endpoints

- `GET /healthz`
- `GET /readyz`

These endpoints are intentionally public so container orchestration can check
process health even when basic auth protects the dashboard and API.

## Configuration

Configuration is file based for v1. The UI may show effective configuration
state, but it must not edit configuration.

Runtime monitoring starts automatically for configured HTTP route, Docker, and
Kubernetes targets. The default cadence is `monitoring.defaultInterval`, and
each target can override it with `interval`.

Repository scans are available on demand through the dashboard/API. A repository
can also opt into scheduled scans by setting `scanInterval` in its file-based
configuration.

Repository scans can be narrowed with `includePaths` and `excludePaths` under a
repository entry. Plain paths match that file or directory subtree, and glob
paths support `*` plus recursive `**` patterns.

Use `examples/config.dev.yaml` for a no-auth local development server.
Use `examples/config.basic.yaml` for a basic-auth local server example.
Use `examples/docker-compose.yaml` for server and remote-agent deployment
shape, with concrete configuration in `examples/compose-config`.
