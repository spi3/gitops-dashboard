# Route discovery

The dashboard discovers inventory and candidate HTTP routes from repository
configuration. A candidate is monitored only after it is normalized as an HTTP
route and is allowed by the configured egress policy. Discovery never guesses a
port.

## Compose

- `ports:` supplies two distinct kinds of evidence. A concrete `host_ip` bind
  produces a route using the published port; short syntax behaves the same.
  Equal-length published and target ranges expand member-for-member only in
  short syntax. Long syntax requires a single target/container port, so a
  ranged long-syntax target is warned and skipped as invalid input. A published
  allocation pool has no knowable selected host port and produces no
  published-host route. Wildcard `0.0.0.0` binds are skipped because they do
  not identify a host to probe. A static `ipv4_address` or `ipv6_address` is
  instead paired with each valid declared target/container port.
- `expose:` supplies declared container ports. Each static `ipv4_address` and
  `ipv6_address` is paired with every `expose:` port, in addition to every
  `ports:` target port. Multiple explicit ports produce multiple routes.
- A static address with no target-port or `expose:` evidence is retained as the
  non-monitorable inventory value `address/<literal>` and produces no HTTP
  route. In particular, it is never converted into an implicit port-80 probe.
- `network_mode: host` has no independent container-address or published-port
  boundary, so Compose port, expose, and static-address declarations produce no
  route. Explicit Traefik label host rules remain independent route evidence.
- Compose Traefik labels contribute only `Host(...)` and `HostSNI(...)` router
  host rules. They do not infer backend ports.

## Traefik

- File-provider HTTP and TCP router rules contribute `Host(...)` and
  `HostSNI(...)` routes associated with their named service.
- File-provider backend server URLs and TCP server addresses are collected too;
  they retain their explicitly declared ports. Router rules are not enriched
  with backend ports: the router host is a public-routing assertion, while a
  backend address can be private, shared, or selected dynamically, so combining
  them would invent an endpoint.

## Hosts and Kubernetes

- Ansible inventory produces `host/<address>` services for host-ping targets.
  It adds no HTTP route enrichment.
- Kubernetes Services contribute load-balancer and external IP addresses paired
  with declared Service ports; Ingress contributes its declared host routes.
  Workloads receive matching Service and Ingress exposure by selector and
  backend reference. Route-looking values found in referenced ConfigMap data
  are also collected.

## Assembly, canonicalization, and policy

The scanner associates Traefik routes to Compose services through normalized
names (lower case, with `_` and `.` treated as `-`), then sorts and deduplicates
exposure. It does not reconcile, merge, or infer ports across sources.

HTTP target canonicalization lowercases scheme and authority, strips URL
userinfo, removes default `:80` for HTTP and `:443` for HTTPS, brackets IPv6
literals in an authority, and removes a root-only trailing slash while
preserving meaningful paths, queries, fragments, and escapes. Non-HTTP
inventory values such as `address/...`, `host/...`, and `service/...` are not
HTTP monitor targets.

Before a route is dialed, the egress policy checks the declared host and every
resolved address, including redirects. A denied route is recorded as a
`not_applicable` result with a `blocked by policy` message, rather than being
probed or counted as a failed HTTP check.
