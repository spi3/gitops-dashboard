# GitOps Dashboard Vision

## Summary

This project is a GitOps-inspired dashboard for discovering, understanding, and
monitoring infrastructure and application repositories. It should automatically
inspect Git repositories, detect deployment specifications, infer the services
they describe, and build a useful dashboard without requiring each repository to
be manually modeled.

The first-class targets are:

- Docker Compose specifications
- Kubernetes specifications

The dashboard should make a GitOps repository observable as a system: what it
declares, what services it contains, where those services run, whether the
declared state appears healthy, and what changed recently.

## Product Goal

Give operators and developers a single place to answer:

- Which GitOps repositories exist?
- What deployable services do they define?
- Are those services Docker Compose based, Kubernetes based, or both?
- What environments are represented by the repository?
- What is currently healthy, degraded, unknown, or out of sync?
- What changed recently in Git, and what might that change affect?

The dashboard should reduce the manual work needed to maintain service
catalogs, runbooks, deployment inventories, and status dashboards.

## Intended Users

The first target user is a homelab or self-hosted operator running services
from Git. The product should assume this user wants useful infrastructure
visibility without adopting a heavy platform stack.

Secondary users include:

- A developer running several self-hosted or team services from Git.
- An operator responsible for multiple small GitOps repositories.
- A small organization that wants a lightweight service catalog without adopting
  a large platform suite.

## Guiding Principles

- Git is the source of declared intent.
- Discovery should be automatic by default.
- The dashboard should be useful even before live environment integrations are
  configured.
- Static analysis of repository contents and live monitoring should be separate
  concerns.
- Unknown state should be explicit rather than hidden.
- Infer environments from practical repository layouts instead of requiring one
  rigid convention.
- Stay read-only. The dashboard should observe, explain, and monitor declared
  infrastructure, not mutate repositories or runtimes.
- Keep configuration file based for v1, in the spirit of GitOps.

## Core Capabilities

### Repository Discovery

The system should be able to discover GitOps repositories from configured
sources. Version 1 must support:

- GitHub repositories accessed with a personal access token
- Public generic Git URLs
- Private generic Git URLs accessed with SSH keys

Potential future sources include:

- Local filesystem paths
- GitHub organizations or users beyond explicitly configured repositories
- GitLab repositories, groups, or users
- A manually curated list of repositories

For each repository, the dashboard should capture basic metadata:

- Repository name and URL
- Default branch
- Last scanned commit
- Last scan time
- Detected deployment formats
- Detected environments
- Detected services

### Specification Detection

The dashboard should inspect repository contents and identify supported
deployment specifications.

For Docker Compose, it should detect files such as:

- `compose.yaml`
- `compose.yml`
- `docker-compose.yaml`
- `docker-compose.yml`

For Kubernetes, it should detect resources such as:

- Raw Kubernetes YAML manifests
- Namespaces
- Deployments
- StatefulSets
- DaemonSets
- Services
- Ingresses
- ConfigMaps
- Secrets, without exposing secret values

Generic Kubernetes manifests are the first Kubernetes target. Potential future
Kubernetes formats:

- Kustomize overlays
- Helm charts
- Argo CD Applications
- Flux resources

### Service Model

The system should normalize Docker Compose and Kubernetes resources into a
shared service model so the dashboard can show mixed environments consistently.

A service should include, where available:

- Name
- Source repository
- Source file path
- Runtime type, such as Docker Compose or Kubernetes
- Inferred environment, such as production, staging, development, or homelab
- Container images
- Exposed ports
- Dependencies
- Volumes and persistent storage
- Network exposure, such as ingress, host ports, or service type
- Health signals
- Recent Git changes

### Dashboard Generation

The dashboard should be built from discovered repository state rather than a
hand-authored inventory.

Useful views include:

- Repository overview
- Service catalog
- Environment view
- Runtime view, grouped by Docker Compose or Kubernetes
- Health and status overview
- Recent changes
- Per-service detail pages
- Scan and sync history

The generated dashboard should make gaps visible. For example, if a service has
declared ports but no known health check, the dashboard should show that the
health state is unknown.

### Monitoring

Monitoring should have layers so the system remains useful in different
deployment contexts. Version 1 should use static repository analysis to generate
the dashboard, and should also include live health and status monitoring.

1. Static repository analysis
2. Git change monitoring
3. Runtime status checks
4. Optional live integrations

Static analysis can answer what the repository declares. Runtime checks can
answer whether the declared service appears to be running. Live integrations can
answer deeper platform-specific questions.

Version 1 monitoring inputs should include:

- Git commit history
- Docker Compose runtime status from configured local Docker targets and remote
  Docker daemons that connect outbound to the dashboard server over WebSocket
- Kubernetes API status from configured local or remote clusters using mounted
  kubeconfig files for resources discovered from generic manifests
- HTTP health checks where endpoints can be configured or inferred safely
- Host reachability checks from configured Ansible `hosts.yml` inventories

Potential future monitoring inputs:

- Container image metadata
- Argo CD sync status
- Flux reconciliation status

## Docker Compose Support

Docker Compose support should focus on practical service discovery first.

The dashboard should parse Compose files and extract:

- Services
- Images and build contexts
- Ports
- Volumes
- Networks
- Environment variable names, without exposing sensitive values
- Healthchecks
- Restart policies
- Dependencies declared through `depends_on`

Useful Compose-specific views:

- Compose project summary
- Service dependency graph
- Published ports
- Persistent volumes
- Services missing healthchecks
- Build-context services versus image-based services

The dashboard should read Compose specs to build the service inventory and
should support live Docker status monitoring in version 1 for configured local
targets and remote Docker daemons. Remote Docker monitoring should be agent
initiated: the daemon runs near the Docker engine and reaches out to the
dashboard server over WebSocket. The daemon should ship as the same container
image in an agent mode.

## Kubernetes Support

Kubernetes support should start with manifest parsing and a normalized inventory
of workloads, services, ingress, and configuration.

The dashboard should parse Kubernetes resources and extract:

- Workloads and replicas
- Container images
- Services and exposed ports
- Ingress hosts and paths
- Namespaces
- ConfigMap and Secret references
- PersistentVolumeClaims
- Labels and selectors
- Resource requests and limits
- Probes, including liveness, readiness, and startup probes

Useful Kubernetes-specific views:

- Namespace summary
- Workload status
- Service and ingress exposure
- Image inventory
- Resource requests and limits
- Workloads missing probes
- Workloads referencing missing configuration

Version 1 Kubernetes health should come from generic manifest analysis plus
direct Kubernetes API status for the corresponding live resources in configured
local or remote clusters. Kubernetes access should use mounted kubeconfig files
in v1. Argo CD and Flux should be treated as future integrations, not required
for the first version.

## Initial MVP

The first useful version should be read-only, repository driven, and useful for
a homelab or self-hosted operator.

MVP capabilities:

- Configure one or more GitHub repositories.
- Configure one or more generic public Git repository URLs.
- Configure one or more generic private Git repository URLs with SSH keys.
- Scan repositories on demand.
- Detect Docker Compose files.
- Detect generic Kubernetes YAML manifests.
- Parse discovered specs into a normalized service inventory.
- Render a dashboard with repositories, services, runtime type, source paths,
  exposed ports, images, and health status.
- Monitor live Docker Compose status where a local or remote Docker target is
  configured.
- Monitor live Kubernetes status where local or remote cluster access is
  configured.
- Use mounted kubeconfig files for Kubernetes monitoring.
- Use remote Docker daemons that connect outbound to the dashboard server for
  remote Docker monitoring.
- Use WebSocket as the remote Docker daemon transport.
- Ship the remote Docker daemon as the same container image in agent mode.
- Infer environments from repository folder names.
- Treat common folder names such as `prod`, `production`, `stage`, `staging`,
  `dev`, `development`, `test`, `testing`, `homelab`, `lab`, and `local` as
  environment signals.
- Check live runtime health every 30 seconds by default, with per-target
  overrides and backoff on repeated connection errors.
- Mark health as `unknown` when no runtime signal is configured.
- Track scan errors per repository without failing the whole dashboard.
- Package the application as a single container.
- Protect the dashboard with basic authentication.
- Use file-based configuration rather than UI-driven configuration editing.

Non-goals for the MVP:

- Deploying changes
- Editing repository files
- Mutating Docker or Kubernetes runtime state
- Secret value inspection
- Full Kubernetes controller behavior simulation
- Replacing Argo CD, Flux, Docker, or Kubernetes-native dashboards

## Future Capabilities

- Scheduled repository scans
- Webhook-triggered scans
- GitHub organization/user discovery
- GitLab integrations
- Argo CD and Flux integrations
- Additional Docker host connection methods
- Additional Kubernetes cluster connection methods
- UI-assisted configuration validation
- Service dependency graph
- Drift detection between Git and runtime state
- Alerting for failed scans, unhealthy services, or out-of-sync resources
- RBAC and multi-user access
- Change impact summaries
- Policy checks for missing probes, exposed ports, latest-tag images, or
  unpinned versions
- Exportable inventory data

## Design Questions

These questions need product decisions before the vision becomes an
implementation plan:

1. For GitHub v1 support, should repository discovery start with explicit
   repository URLs only, or should authenticated account/org scanning also be
   included?
2. What repository layouts should be considered common enough to support from
   day one?
