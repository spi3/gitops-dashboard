# TASK-0030: Route discovery correctness

## Scope

Require explicit Compose port evidence for static addresses, support `expose:`,
construct IPv6 route authorities safely, and document every route discovery
source and its monitorability rules.

## Out of Scope

- Migration of stored route target identity (T-031).
- Inferring Traefik backend ports from router rules.

## Dependencies

- TASK-0028 checkout-order work (external T-028).

## E2E Plan

1. Parse Compose fixtures covering a no-port static IPv4 address, declared
   target ports, `expose:` ports, IPv6, and host networking.
2. Build the embedded dashboard UI and binary.
3. Run the dashboard Playwright specification.

## Observed Results

The Compose parser fixtures retain a no-port static address only as
`address/10.10.10.127`, retain each valid declared TCP target port (including
short-syntax ranges), omit UDP and SCTP declarations from HTTP route discovery,
and use a service static address rather than an IPv4 or IPv6 wildcard bind.
Portless IPv6 authorities are bracketed. Short-syntax equal-length published and target
ranges produce member-for-member published-host routes. Long syntax requires a
single target/container port: a ranged target is loudly skipped as invalid
input and produces no route. Allocation pools with multiple published ports
for one valid target produce no guessed published-host route, while their
declared container port still supports a static service-address route. The
route-target test verifies that the address inventory value does not become a
monitor target. Published binds are rejected semantically when their parsed IP
is unspecified, covering `0.0.0.0`, IPv4-mapped `::ffff:0.0.0.0`, and
canonical, expanded, and `::0` IPv6 spellings in both Compose port syntaxes.

## Verification Evidence

The following commands were rerun verbatim from the repository root on
2026-07-10. Each completed with exit status 0:

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/...
[exit 0]
$ make build
[exit 0]
$ make check
[exit 0]
$ git diff --check
[exit 0]
```

## Documentation Sweep

`README.md` links to `docs/discovery.md`. The discovery reference covers
Compose, Traefik, Ansible, Kubernetes, scanner assembly, canonicalization, and
egress-policy behavior.
