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

## Required Server Mounts

- `/config`: file-based application configuration.
- `/data`: SQLite database and repository cache.
- `/ssh`: optional SSH keys and known hosts for private Git repositories.
- `/kube`: optional kubeconfig files for Kubernetes monitoring.

## Required Agent Mounts

- `/config`: file-based agent configuration.
- `/var/run/docker.sock`: optional local Docker socket when the agent monitors
  the colocated Docker engine.

## Health Endpoints

- `GET /healthz`
- `GET /readyz`

These endpoints are intentionally public so container orchestration can check
process health even when basic auth protects the dashboard and API.

## Configuration

Configuration is file based for v1. The UI may show effective configuration
state, but it must not edit configuration.

Runtime monitoring starts automatically for configured Docker and Kubernetes
targets. The default cadence is `monitoring.defaultInterval`, and each target
can override it with `interval`.

Repository scans are available on demand through the dashboard/API. A repository
can also opt into scheduled scans by setting `scanInterval` in its file-based
configuration.

Use `examples/config.dev.yaml` for a no-auth local development server.
Use `examples/config.basic.yaml` for a basic-auth local server example.
Use `examples/docker-compose.yaml` for server and remote-agent deployment
shape.
