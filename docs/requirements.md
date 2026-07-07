# GitOps Dashboard Requirements

## Scope

Version 1 is a read-only, single-container dashboard for homelab and
self-hosted GitOps repositories. It discovers Git repositories, analyzes Docker
Compose and generic Kubernetes manifests, builds a service inventory from static
repository state, and monitors live service health/status through configured
HTTP route, Docker, and Kubernetes targets.

The dashboard must support:

- GitHub repositories accessed with a personal access token.
- Generic public Git repositories.
- Generic private Git repositories accessed with SSH keys.
- Docker Compose specifications.
- Generic Kubernetes YAML manifests.
- Local and remote Docker targets.
- Remote Docker monitoring through a read-only daemon that connects outbound to
  the dashboard server.
- Local and remote Kubernetes clusters.
- HTTP route checks for discovered service URLs.
- SQLite storage.
- Basic authentication.
- File-based configuration.
- Inferred environments.

## Functional Requirements

### Repository Configuration

- Users must be able to configure one or more GitHub repositories.
- Users must be able to configure one or more generic Git repository URLs.
- GitHub repositories must support authentication with a personal access token.
- Generic Git repositories must support public clone URLs.
- Generic Git repositories must support SSH-key authentication.
- Repository configuration must include a stable display name, URL, default
  branch or ref, authentication method, and scan settings.
- Repository configuration may include scan `includePaths` and `excludePaths`
  so large repositories can limit which directories or files are parsed.
- Repository credentials must never be displayed back to the user after entry.
- Repository configuration must be file based for v1.
- The dashboard must not mutate repository configuration through the UI in v1.

### Repository Scanning

- The system must clone or fetch configured repositories into managed local
  storage.
- The system must record the commit SHA used for each scan.
- The system must scan repositories on demand.
- The system should support scheduled scans after the initial on-demand flow is
  working.
- A scan failure in one repository must not prevent other repositories from
  being scanned.
- Scan errors must be visible in the dashboard with enough context to diagnose
  authentication, network, parsing, or unsupported-format issues.

### Specification Discovery

- The system must detect Docker Compose files named:
  - `compose.yaml`
  - `compose.yml`
  - `docker-compose.yaml`
  - `docker-compose.yml`
- The system must detect generic Kubernetes YAML manifests.
- The system must support multi-document YAML files.
- The system must ignore unsupported YAML documents without failing the whole
  scan.
- The system must track source file paths for every discovered resource.

### Docker Compose Analysis

- The system must parse Compose services.
- The system must extract service names, images, build contexts, ports,
  volumes, networks, healthchecks, restart policies, and dependencies.
- The system must record environment variable names without storing or
  displaying sensitive values.
- The system must identify services that expose ports.
- The system must identify services missing healthchecks.
- The system must normalize Compose services into the shared service model.

### Kubernetes Manifest Analysis

- The system must parse generic Kubernetes manifests without requiring Helm,
  Kustomize, Argo CD, or Flux.
- The system must extract Namespaces, Deployments, StatefulSets, DaemonSets,
  Services, Ingresses, ConfigMaps, Secrets, and PersistentVolumeClaims.
- The system must extract workload names, namespaces, replicas, container
  images, labels, selectors, ports, probes, resource requests/limits, and
  configuration references.
- Secret values must not be stored or displayed.
- The system must identify workloads missing readiness or liveness probes.
- The system must normalize Kubernetes workloads into the shared service model.

### Service Inventory

- The system must maintain a normalized inventory across Compose and Kubernetes.
- Each service record must include:
  - Name
  - Source repository
  - Source commit
  - Source file path
  - Runtime type
  - Inferred environment
  - Images
  - Exposed ports
  - Dependencies
  - Storage references
  - Network exposure
  - Static analysis warnings
  - Live health/status, when available
- The system must mark service health as `unknown` when no live signal is
  configured or available.
- The system must infer service environments from folder names in repository
  paths.
- The first environment aliases should include `prod`, `production`, `stage`,
  `staging`, `dev`, `development`, `test`, `testing`, `homelab`, `lab`, and
  `local`.
- Inferred environments must be visible as inferred values, not presented as
  user-authored facts.

### Runtime Target Configuration

- Users must be able to configure Docker monitoring targets.
- Users must be able to configure Kubernetes monitoring targets.
- Users must be able to configure HTTP route monitoring targets for discovered
  service URLs.
- Docker targets must support both local and remote engines.
- Remote Docker targets must use a read-only daemon/agent that runs near the
  Docker engine and connects outbound to the dashboard server.
- Kubernetes targets must support both local and remote clusters.
- Runtime target configuration must be separate from repository configuration.
- Users must be able to associate repositories, paths, environments, or services
  with runtime targets.
- Users must be able to include or exclude repository paths from inventory
  scans without changing repository contents.
- Failed runtime checks must not block repository scanning.
- Runtime target configuration must be file based for v1.

### HTTP Route Monitoring

- The system must be able to check discovered HTTP and HTTPS service URLs
  without mutating the target service.
- HTTP route monitoring must skip cluster-internal service references and
  Docker network names.
- HTTP route monitoring must record reachable, degraded, down, and request
  error states in the shared status history.

### Docker Monitoring

- The system must read status from configured Docker targets without mutating
  Docker state.
- Docker monitoring must support local and remote connection profiles.
- Remote Docker monitoring must be agent initiated: the remote daemon connects
  to the dashboard server rather than requiring the dashboard to open inbound
  access to the remote Docker host.
- Remote Docker daemon connections must use WebSocket as the default outbound
  protocol.
- The remote Docker daemon must ship as the same container image in an agent
  mode.
- The remote Docker daemon must authenticate to the dashboard server.
- The remote Docker daemon must have read-only Docker permissions.
- Docker monitoring must determine whether discovered Compose services appear to
  be running.
- Docker monitoring should capture container state, restart count, healthcheck
  state, image, exposed ports, and last status update time when available.
- Docker monitoring must report connection and permission errors clearly.

### Kubernetes Monitoring

- The system must read status from configured Kubernetes clusters without
  mutating cluster state.
- Kubernetes monitoring must support local and remote clusters through explicit
  configuration.
- Kubernetes monitoring must use mounted kubeconfig files in v1.
- Kubernetes monitoring must support selecting contexts from mounted kubeconfig
  files.
- Kubernetes monitoring must match discovered manifest resources to live
  resources by kind, namespace, and name.
- Kubernetes monitoring should capture workload readiness, available replicas,
  pod status summaries, service presence, ingress presence, and recent relevant
  conditions when available.
- Kubernetes monitoring must report connection, authentication, authorization,
  and missing-resource errors clearly.

### Dashboard

- The dashboard must provide a repository overview.
- The dashboard must provide a service catalog.
- The dashboard must provide per-service detail pages.
- The dashboard must show scan status and scan history.
- The dashboard must show live monitoring status and last checked time.
- The dashboard must distinguish `healthy`, `degraded`, `unhealthy`, `unknown`,
  and `error` states.
- Runtime health checks should run every 30 seconds by default.
- Runtime health check cadence should be configurable per target.
- Runtime health checks should apply backoff after repeated connection errors.
- The dashboard must make static analysis warnings visible.
- The dashboard must remain useful when runtime monitoring is not configured.

### Authentication

- The dashboard must support basic authentication.
- Basic auth must be configurable through deployment configuration.
- Basic auth configuration must be file based for v1.
- The application must not run without an explicit authentication decision.
  Acceptable states are enabled basic auth or an explicit local-only/no-auth
  development mode.
- Passwords must not be stored in plaintext.

## Non-Functional Requirements

### Deployment

- Version 1 must be packaged as a single container.
- The container must support mounted configuration, mounted SQLite storage, and
  mounted credentials.
- The container should expose a single HTTP port.
- The application should be practical to run from Docker Compose even though the
  product artifact is a single container.
- The same container image must be able to run in dashboard server mode or
  remote Docker agent mode.

### Security

- The product must be read-only toward repositories and runtimes.
- The product must not deploy, sync, restart, delete, scale, edit, or otherwise
  mutate external systems.
- Secrets, PATs, SSH keys, kubeconfigs, Docker credentials, and basic-auth
  passwords must be treated as sensitive.
- Secret values discovered in manifests must not be stored or rendered.
- Logs must avoid printing credentials or secret values.

### Reliability

- Repository scanning, static analysis, and runtime monitoring must fail
  independently.
- A failure in one repository or runtime target must not take down the
  dashboard.
- The system must preserve the last successful inventory when a later scan
  fails.
- The system must expose enough error state for users to diagnose broken
  configuration.

### Performance

- Version 1 should be comfortable for a homelab-scale deployment.
- The initial target scale is dozens of repositories and hundreds of services.
- Scanning should avoid unnecessary full reclones when a repository already
  exists locally.
- Runtime checks should be bounded by timeouts.

### Portability

- The application should run on common Linux container hosts.
- The application should not require Kubernetes to run.
- The application should not require Docker socket access unless Docker
  monitoring is configured.
- The application should not require kubeconfig or cluster credentials unless
  Kubernetes monitoring is configured.
- The dashboard UI should expose the effective loaded configuration in a
  read-only way where useful, but configuration changes should happen through
  files.

## MVP Acceptance Criteria

- A user can start the application as a single container.
- A user can protect the dashboard with basic authentication.
- A user can add a GitHub repository using a PAT.
- A user can add a public generic Git repository.
- A user can add a private generic Git repository using an SSH key.
- A user can run a repository scan.
- The scan discovers Compose and Kubernetes manifests.
- The dashboard shows discovered repositories and services.
- The dashboard shows source paths, images, ports, and runtime type.
- The dashboard shows scan errors without crashing.
- The user can configure at least one Docker monitoring target.
- The user can configure at least one Kubernetes monitoring target.
- The user can configure at least one HTTP route monitoring target.
- A remote Docker daemon can connect outbound to the dashboard server and report
  read-only status over WebSocket.
- The same container image can be started in remote Docker agent mode.
- Kubernetes monitoring can use a mounted kubeconfig.
- The dashboard shows live health/status where monitoring is configured.
- Services without live monitoring show `unknown` rather than pretending to be
  healthy.
- Environments are inferred from repository folder names.

## Explicit Non-Goals

- Mutating Git repositories.
- Mutating Docker hosts.
- Mutating Kubernetes clusters.
- Running deployments.
- Replacing Argo CD, Flux, Docker Desktop, Portainer, Lens, or Kubernetes-native
  dashboards.
- Supporting Helm rendering in v1.
- Supporting Kustomize rendering in v1.
- Multi-user RBAC in v1.
- OIDC or SSO in v1.
- UI-based configuration editing in v1.
- Direct dashboard-to-remote-Docker connections in v1.

## Open Questions

1. For GitHub v1 support, should repository discovery start with explicit
   repository URLs only, or should authenticated account/org scanning also be
   included?
2. What repository layouts should be considered common enough to support from
   day one?
