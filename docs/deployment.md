# Deployment

## Container Modes

The project ships as one container image with two modes:

- `server`: runs the dashboard server, API, scanner, storage, and frontend.
- `agent`: runs a Docker reporter that reads one Docker Engine and connects
  outbound to the dashboard server over WebSocket.

Server mode is the default.

```sh
gitops-dashboard -config /config/config.yaml
```

Build metadata can be checked without loading config:

```sh
gitops-dashboard -version
```

Agent mode:

```sh
gitops-dashboard -mode agent -config /config/agent.yaml
```

In Docker Compose, run the same image twice: once as the dashboard server and
once as the Docker agent. The dashboard exposes the UI/API and persists `/data`.
The agent does not expose a port; it opens an outbound WebSocket connection to
the dashboard server at `/api/agents/connect`.

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
export GITOPS_DASHBOARD_AGENT_TOKEN="$(openssl rand -hex 32)"
docker compose -f examples/docker-compose.yaml up -d
docker compose -f examples/docker-compose.yaml ps
docker compose -f examples/docker-compose.yaml logs -f dashboard docker-agent
```

Pushes to `main` run the GitHub Actions workflow in `.github/workflows/ci.yml`.
After tests pass, the workflow publishes the image to GitHub Container Registry:

- `ghcr.io/spi3/gitops-dashboard:latest`
- `ghcr.io/spi3/gitops-dashboard:sha-<short-sha>`
- `ghcr.io/spi3/gitops-dashboard:vMAJOR.MINOR.PATCH` on release tags
- `ghcr.io/spi3/gitops-dashboard:vMAJOR.MINOR` on release tags
- `ghcr.io/spi3/gitops-dashboard:vMAJOR` on release tags

The full container and service versioning process, including release tags and
GitOps deployment pinning guidance, is documented in
[versioning.md](versioning.md).

The image and binary carry the same build metadata: version, commit revision,
and UTC build timestamp. The running server exposes it at `GET /api/version`
and includes it in the dashboard footer.

The Compose file mounts `examples/compose-config` into both containers:

- `config.yaml`: dashboard server config that reads the accepted agent token
  from `GITOPS_DASHBOARD_AGENT_TOKEN`.
- `agent.yaml`: remote Docker agent config with the dashboard WebSocket URL,
  target name, matching token environment reference, reporting interval, and
  Docker host.

Before using the example outside local testing, provide the same
`GITOPS_DASHBOARD_AGENT_TOKEN` value to both containers through your container
runtime, secret manager, or orchestrator.

The dashboard accepts agent connections only when the token is sent in the
`X-Agent-Token` header. Query-string tokens are rejected. Shared
`auth.agent.tokens`, `auth.agent.tokenEnv`, and `auth.agent.tokenFile` values
are authorized for the configured `kind: agent` Docker targets; per-target
`agentToken`, `agentTokenEnv`, and `agentTokenFile` values are authorized only
for that Docker target's name.

The Docker target name and agent target must match. In the example, the server
declares `runtime.docker[0].name: compose-docker-agent`, and the agent uses
`agent.target: compose-docker-agent`.

Browser WebSocket upgrades must come from the same host as the dashboard
request, unless an origin is listed in `auth.agent.allowedOrigins`.
Non-browser agents that omit the `Origin` header keep working unchanged.

Minimal agent config:

```yaml
agent:
  serverUrl: ws://dashboard:8080/api/agents/connect
  target: compose-docker-agent
  tokenEnv: GITOPS_DASHBOARD_AGENT_TOKEN
  interval: 30s
  docker:
    host: unix:///var/run/docker.sock
```

Agent mode validates only the `agent` section, so server-only `auth`,
`repositories`, `runtime`, and `monitoring` sections are not required in
`agent.yaml`.

Agent reports appear on the dashboard's Agents tab with connection state and
reported container state. Reports from configured `kind: agent` Docker targets
also feed per-service health and uptime rows only when a Compose service can be
bound to that target by source path (`docker_files/<target>/...`, where
`<target>` is the configured target name). Incoming agent report rows carry
normalized `health` and `restartCount` values, so per-service status uses the
same normalized container semantics used by the dashboard's direct docker status
mapping.
Per-service dashboard health can also be produced by direct Docker Engine
targets, HTTP route checks, Kubernetes targets, and host ping targets.

To have the dashboard server check a Docker Engine directly instead of using
agent reports, configure a direct Docker target and mount or expose a Docker
Engine API to the server:

```yaml
runtime:
  docker:
    - name: local-docker
      host: unix:///var/run/docker.sock
```

Direct Docker targets are bound to Compose services by source path when the
scanner can infer a host: a service discovered under `docker_files/<target>/...`
is checked only by the Docker target with the same `name`. If a Compose service
does not have an inferable target in its source path, the dashboard checks it
only when exactly one direct Docker target is configured; with multiple direct
targets, the service is left unbound instead of guessing. Container matching uses
Docker Compose labels when available; a Compose file's top-level `name:` is used
for the project label comparison only when it is literal. Interpolated names such
as `$STACK_NAME` or `${STACK_NAME:-prod}` are treated as unknown because the
dashboard cannot know the deployment host environment or Compose `.env` values,
so the service label is matched without a strict project comparison. Escaped
`$$` sequences are unescaped to literal dollars. For unlabeled legacy agent
reports, the fallback accepts exact container-name equality and Compose-generated
`<project>-<service>-<index>` or `<project>_<service>_<index>` names with exact
service-name segment boundaries.

## Health Endpoints

- `GET /healthz`
- `GET /readyz`

These endpoints are intentionally public so container orchestration can check
process health even when basic auth protects the dashboard and API.

## Configuration

Configuration is file based for v1. The UI may show effective configuration
state, but it must not edit configuration.

Mounted Git-managed config files can reference secrets without an entrypoint
render step. Use literal fields for non-secret values and one of the matching
`*Env` or `*File` fields for values supplied by the environment or mounted
secret files:

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
  serverUrl: ws://dashboard:8080/api/agents/connect
  target: compose-docker-agent
  tokenEnv: GITOPS_DASHBOARD_AGENT_TOKEN
```

Supported secret source fields:

- `auth.users[].passwordHash`, `passwordHashEnv`, or `passwordHashFile`
- `auth.agent.tokens[]`, `tokenEnv`, or `tokenFile`
- `runtime.docker[].agentToken`, `agentTokenEnv`, or `agentTokenFile`
- `agent.token`, `tokenEnv`, or `tokenFile`

The loader also expands `${ENV_NAME}` placeholders before YAML parsing for
simple mounted-config deployments, but the `*Env` and `*File` fields are safer
for values that may contain YAML-sensitive characters or newlines.

Runtime monitoring starts automatically for configured HTTP route, Docker,
Kubernetes, and ping targets. The default cadence is
`monitoring.defaultInterval`, and each target can override it with `interval`.

Host ping monitoring can be driven from an Ansible YAML inventory committed to
one of the configured repositories:

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

The inventory path is relative to the named repository. Each host under an
inventory `hosts` map becomes a dashboard `Host` service. If the host has
`ansible_host`, that address is pinged; otherwise the inventory host name is
pinged.

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
