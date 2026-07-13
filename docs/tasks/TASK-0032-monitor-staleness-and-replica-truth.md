# TASK-0032 — Monitor staleness and replica truth

## Scope

T-032 prevents last-known-good monitor data from remaining healthy after a
collector failure or an agent reporting lapse. It also makes Docker compose
replica health represent the worst matching replica.

## Out of scope

Alert delivery for these transitions remains owned by T-022/T-023.

## Implementation

- Persist a server-derived expiry on monitor results: two configured target
  intervals, so the budget follows configured work rather than a global timer.
- Route child targets resolve freshness through their configured route-monitor
  parent. Legacy rows without the new expiry fields derive the same deadline at
  read time, including already-offline agent projections.
- Convert expired results to `unknown` when summaries/statuses are computed;
  legacy rows without an expiry remain readable until their next monitor write.
- Persist each agent report's server receipt-based expiry and use that same
  boundary for agent-card connected/stale/offline presentation.
- Record an explicit target-check error for all services covered by failed
  Docker, Kubernetes, HTTP, and ping collectors; ping coverage is constrained
  to the target's runtime inventory source. Cancellation does not write failure
  status, and history maintenance failures remain advisory.
- Replace agent timestamps outside 30 seconds of server receipt time, avoiding
  future rows that could dominate latest-status selection.
- Aggregate all matching Docker replicas, retaining a replica-count summary
  and the worst health result; lifecycle state takes precedence over a retained
  healthcheck value.
- Freshness-table migrations validate an exact accepted DDL contract plus
  `table_xinfo` metadata (including hidden columns), indexes, foreign keys, and
  triggers. Drift is atomically rebuilt; conflicting legacy identities retain
  the newest timestamped observation and every conflicting raw row is recorded
  in `freshness_recovery`, so monitor schema drift does not block startup.
- Agent report projection uses the same override semantics as ordinary monitor
  writes, and only produces advisory health alerts after the receipt and all
  dependent status writes commit.
- Legacy agent projections with no current target policy use their persisted
  receipt/stale deadline; old agent-controlled timestamps cannot leave removed
  agents healthy forever.

## E2E plan

1. Run a focused monitor/storage suite covering a stale persisted row, skewed
   agent timestamps, and mixed healthy/unhealthy replicas.
2. Run the complete monitor/storage/internal suite, build, and web checks.
3. Launch the built dashboard against temporary data, send one agent WebSocket
   report, stop reporting, and inspect the summary at the shared expiry.

## Observed results

- `GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/monitor ./internal/storage ./internal/...` passed. This includes the alert unique-constraint migration regression: its index inspection now closes its SQLite cursor before inspecting candidate index columns.
- `make build` passed; Vite produced the UI bundle and Go built `./cmd/gitops-dashboard`.
- `npm test` passed (`tsc --noEmit` and ESLint).
- `git diff --check` passed.
- Manual E2E (launched process; temporary harness created the data directory,
  seeded `manual-web`, launched the binary on `127.0.0.1:18106`, sent the
  authenticated WebSocket report, queried the API, then removed the temporary
  directory):

  ```sh
  GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local \
    go build -o /tmp/t032-current-dashboard ./cmd/gitops-dashboard
  GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local \
    go run ./cmd/t032-e2e
  # beforeStatuses=0 after=healthy expired=unknown
  ```

  The report made the compose tile healthy; withholding the next report past
  the two one-second intervals made it unknown.

## Documentation sweep

No user configuration fields were added. Existing runtime target intervals now
also govern freshness; no configuration-reference change is required.
