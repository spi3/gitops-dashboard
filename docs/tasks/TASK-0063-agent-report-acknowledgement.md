# TASK-0063: Acknowledge persisted agent reports and collect before dialing

## Status

Proposed

## Scope

`internal/agent` and `internal/app`, production and test code only. This is
the homelab replacement for the acknowledgement and collect-before-dial
portion of external T-037 (homelab re-triage 2026-07-12; T-037 is
superseded). `internal/core` is frozen for this task: no shared acknowledgement
or report wire type was added there. `internal/monitor` is unchanged; its
existing `ApplyAgentReport`/`ErrAgentTargetUnauthorized` contract is reused
as-is for authorization and persistence.

## Dependencies

None.

## Task-specific acceptance criteria

R-4/N-1/N-2/N-4/N-5 are a task-local re-triage namespace defined inline in
this record, distinct from `docs/requirements.md`'s ID space, per the same
convention T-060/T-061/T-062 used (see spec review 2026-07-24):

- R-4: collect-before-dial and persistence-before-success.
- N-1: bounded work (fixed dial/write/read deadlines and size limits).
- N-2: secret safety (agent tokens, response bodies, and raw handshake
  headers never appear in errors, acknowledgements, close reasons, or logs).
- N-4: exact focused, wire-schema, response-matrix, and E2E test coverage
  (see Verification, E2E plan, Documentation sweep, Commit evidence).
- N-5: strict protocol handling (exactly one report, exactly one
  acknowledgement, strict schema/semantic validation, exact close codes).

Concretely:

- [x] Production `sendOnce` (`internal/agent/agent.go`) completes
      `collectDocker` and `normalizeAgentMessage` before constructing or
      dialing the WebSocket. A collection failure returns an error
      satisfying `errors.Is(err, errAgentCollectionFailure)` and performs
      zero handshakes (`TestSendOnceCollectsAndNormalizesBeforeDial`).
- [x] `sendOnce` dials with a copy of `websocket.DefaultDialer` whose
      `HandshakeTimeout` is `agentDialHandshakeTimeout` (10s, a package
      var for tests). HTTP 401/403 handshake responses classify as
      `errAgentServerRejection`; other dial failures classify as
      `errAgentConnectionFailure`. Neither ever includes the response body,
      the request headers, or the agent token
      (`TestSendOnceClassifiesHandshakeRejection`,
      `TestSendOnceClassifiesConnectionFailures`).
- [x] After dialing, `sendOnce` sets a fresh `agentReportWriteTimeout` (10s
      var) write deadline and writes exactly one normalized JSON text
      message. A write failure (including a peer/server close during the
      write) classifies as `errAgentConnectionFailure`.
- [x] After a successful write, `sendOnce` sets a fresh
      `agentAckReadTimeout` (10s var) read deadline and a
      `agentAckReadLimit` (4 KiB var) read limit before reading the
      acknowledgement. Success requires exactly
      `{"type":"agent_report_ack","status":"ok","code":"persisted"}`.
- [x] A typed error acknowledgement (`status":"error"` with an allowlisted
      `code`) classifies as `errAgentServerRejection`, exposing only that
      code. A read deadline classifies as `errAgentAcknowledgementTimeout`.
      Malformed JSON, missing/unknown fields, an unknown status or code, an
      invalid status/code pairing (e.g. `ok`+`unauthorized_target`, or
      `error`+`persisted`), a binary acknowledgement, an oversized
      acknowledgement, or an early close after the report write all
      classify as `errAgentProtocolFailure`. There is no fallback path
      (`TestSendOnceAcknowledgementMatrix`,
      `TestSendOnceRejectsOversizedAcknowledgement`).
- [x] All five categories are private sentinel errors
      (`internal/agent/report.go`), independently testable with
      `errors.Is` regardless of the wrapped cause.
- [x] The server (`internal/app/app.go` `agentConnect`/
      `handleAgentReport`) authenticates the `X-Agent-Token` header before
      upgrading; missing/invalid authentication returns HTTP 401 with no
      WebSocket upgrade or acknowledgement (unchanged from prior behavior,
      reverified in `TestAgentReportResponseMatrix/handshake_auth_rejected`
      and the pre-existing `TestAgentEndpointRejectsInvalidToken`).
- [x] After upgrade, the server reads exactly one WebSocket message (up to
      the existing `agentWSReadLimit`, 1 MiB). A text message may be
      RFC-fragmented (Gorilla reassembles fragments transparently). The
      report is decoded with a private, strict wire type
      (`agentReportWire`/`decodeAgentReportWire`,
      `internal/app/agent_report.go`) that rejects unknown fields and
      trailing JSON, then validated against the schema and semantic
      contracts below (Protocol/Validation contract).
- [x] A binary report message is rejected as `invalid_report`, close 1007
      (`TestAgentReportRejectsBinaryMessage`).
- [x] Validation runs in the exact required order: message
      type/usability/size (the initial `ReadMessage` call and the binary
      check) → JSON syntax/schema (`decodeAgentReportWire`) → semantic
      fields (`validateAgentReportSemantics`) → target authorization →
      persistence (both via the `applyAgentReport` seam, which is
      `monitor.ApplyAgentReport` in production and already performs
      authorization before persistence internally).
- [x] Every response in the spec's exact-responses table is implemented
      (see Protocol contract) and covered by
      `TestAgentReportResponseMatrix`.
- [x] The server refreshes `agentWSWriteWait` immediately before writing
      the acknowledgement (`respondAgentReport`) and refreshes it again,
      independently, immediately before the close control message
      (existing `closeAgentConnection` helper) — proven under a shrunk
      `agentWSWriteWait` with an artificially slow persistence stage
      (`TestAgentReportResponseMatrix/refreshed_ack_and_close_deadlines`).
      A failed acknowledgement write is logged as a write failure, never
      claimed as sent, and the connection is still closed.
- [x] `App.applyAgentReport` is a new private function field matching
      `monitor.ApplyAgentReport`'s signature, initialized in `New` to
      `app.monitor.ApplyAgentReport`, used solely by tests to inject a
      deterministic post-validation persistence failure
      (`TestAgentReportResponseMatrix/persistence_failure`,
      `TestAgentReportPersistsBeforeAcknowledgement`).
- [x] Separate sentinels (an agent token, a rejected-field name, and an
      injected persistence error) are asserted absent from acknowledgements,
      close reasons, the HTTP 401 body, and captured logs
      (`TestAgentReportResponseMatrix/no_sensitive_data_leaks`). Rejection
      logs carry only a fixed classification string and, where a target is
      known, a 12-hex-character SHA-256 digest of it (`agentTargetDigest`) —
      never the raw target, never decoded field names/values, which are
      attacker-controlled.
- [x] Added all five exact server tests plus `TestAgentReportResponseMatrix`'s
      required coverage: handshake auth, authorization, syntax, type,
      unknown field, every semantic category, persistence, oversize, broken
      protocol, peer close, success, and refreshed ack/close deadlines (see
      Verification).
- [x] `TestAgentReportWireSchemaBoundaries` names subtests for the 255/256-byte
      target, 255/256-byte-accepted/257-byte-rejected container ID,
      10,000/10,001 containers, duplicate IDs, the name/image/imageId/state/
      status scalar 4,096/4,097 boundary (see Validation contract for why
      `health` is excluded from that specific boundary check), 256/257
      digests, and 32/33 labels.
- [x] `TestAgentReportWireSchemaMissingAndNullFields` covers every required
      top-level and container field missing and null, missing-vs-null
      `repoDigests`, missing-valid vs null-invalid `labels`, null container
      entries, unknown container fields, and non-string label values.
- [x] Added all five exact agent tests, including the acknowledgement matrix
      covering all four valid objects plus every listed invalid pairing (see
      Verification).
- [x] The slow-collection subtest of `TestSendOnceCollectsAndNormalizesBeforeDial`
      uses production `sendOnce`, a blocking fake Docker API, and an atomic
      handshake counter to prove zero handshakes before release, normalized
      non-null `containers`/`repoDigests` after release, and that success
      requires the ack read deadline to have started only after
      collection+dial+write (see E2E plan for the exact mechanism).
- [x] Added the same-package `internal/agent` E2E test
      `TestAgentReportAcknowledgementE2E`, importing `internal/app` and
      calling private `sendOnce` against the real server handler, real
      persistence, and `/api/summary`.
- [x] No negotiation, compatibility fallback, retransmission, queues,
      persistent multi-message sessions, or shared-core acknowledgement
      types were added.
- [x] No persisted-format change; no upgrade fixture added.
- [x] Added this task record with the required headings.
- [x] Recorded the base commit, a proposed T-063 commit subject, and a
      proposed PR title (see Commit evidence).
- [x] Added the `TASK-0063` tracker row and kept `Next Task ID` at
      `TASK-0065`.
- [x] Ran `make check` and the full spec Verification block; recorded
      evidence below; committed all work to the task branch per the
      orchestrator's process adaptation (superseding the spec's
      "leave work uncommitted" sentence).

## Out of scope

Agent disconnect alerts, negotiation/versioning, old/new pairing,
retransmission, durable queues, multi-report sessions, Docker collection
redesign, changes to `internal/core` or `internal/monitor`, and storage
schemas.

## Protocol contract

One WebSocket connection carries exactly one report and one acknowledgement.
Acknowledgement objects are exactly:

```json
{"type":"agent_report_ack","status":"ok","code":"persisted"}
{"type":"agent_report_ack","status":"error","code":"unauthorized_target"}
{"type":"agent_report_ack","status":"error","code":"invalid_report"}
{"type":"agent_report_ack","status":"error","code":"persistence_failed"}
```

Server responses (`internal/app/app.go` `handleAgentReport`/
`respondAgentReport`):

| Condition | Acknowledgement | Close |
| --- | --- | --- |
| Invalid handshake | — | HTTP 401, no upgrade |
| Unauthorized target | `unauthorized_target` | 1008 |
| Schema/syntax/binary/trailing failure | `invalid_report` | 1007 |
| Semantic failure | `invalid_report` | 1008 |
| Persistence failure | `persistence_failed` | 1011 |
| Oversized report message | none | 1009 (Gorilla's own read-limit handling) |
| Broken WebSocket protocol | none | 1002 when writable (Gorilla's own frame validation) |
| Peer close before report | none | — |
| Success | `persisted` | 1000 |

The oversized-message and broken-protocol rows are handled by Gorilla
itself before application code ever runs: exceeding `agentWSReadLimit`
during frame assembly writes close 1009 internally
(`gorilla/websocket` `conn.go`'s `advanceFrame`), and a frame-level protocol
violation (bad opcode, reserved bits, an unmasked client frame, etc.) writes
close 1002 via `handleProtocolError`. `handleAgentReport` never writes an
acknowledgement after a failed `ReadMessage`, in either case.

Agent responses (`internal/agent/agent.go` `sendOnce`,
`internal/agent/report.go`):

| Condition | Classification |
| --- | --- |
| Docker collection failure | `errAgentCollectionFailure` |
| HTTP 401/403 handshake | `errAgentServerRejection` |
| Other dial failure | `errAgentConnectionFailure` |
| Report write failure | `errAgentConnectionFailure` |
| Typed error acknowledgement | `errAgentServerRejection` (exposes only its code) |
| Ack read deadline | `errAgentAcknowledgementTimeout` |
| Malformed/missing/unknown fields, invalid status/code pairing, binary ack, oversized ack, early close | `errAgentProtocolFailure` |
| `ok`/`persisted` | success (nil error) |

### Server read-bound and keepalive decision

The spec pinned every other timeout exactly but left the server's read
bound for the single report message unpinned. This task kept the existing
`agentWSPongWait`-based idle deadline and ping/pong keepalive entirely
unchanged: `agentConnect` still sets an initial `agentWSPongWait` read
deadline, still installs the pong handler that refreshes it, and still
starts the same `pingAgentConnection` background ticker before ever calling
`handleAgentReport`. `handleAgentReport`'s single `conn.ReadMessage()` call
therefore inherits whatever deadline is currently in force from that
existing mechanism — no separate, report-specific read deadline was added.

This means the ping/pong keepalive still fully applies under the one-message
model: an agent that is slow to send its single report (but still
responding to pings) will not be dropped early, and one that goes truly
idle is still bounded by the same `agentWSPongWait` as before. This was the
simplest correct choice because it reuses an already-tested mechanism
(`TestAgentWebSocketReadDeadlineClosesIdleConnection`, unchanged and still
passing) instead of introducing a second, competing read-deadline concept
for a connection that, under this task, will only ever carry one message
before closing anyway.

## Validation contract

Schema failures (`decodeAgentReportWire`, close 1007):

- Top-level value must be one JSON object with exactly `target`,
  `checkedAt`, `containers`. Unknown fields, missing fields, `null` values,
  wrong types, or trailing JSON after the object are all schema failures.
- Every container entry must be a non-null object with exactly the ten
  allowed fields (nine required, `labels` optional). Missing `labels` is
  valid; explicit `labels: null` is a schema failure. `repoDigests` must be
  a non-null array of strings (missing or null is a schema failure).
  Unknown container fields are rejected. Label values must be strings;
  nested objects, arrays, numbers, booleans, or null label values are
  schema failures.
- Presence/nullness is determined by decoding into
  `map[string]json.RawMessage` and checking each raw value directly
  (`isAgentJSONNull`), since `encoding/json` silently no-ops a JSON `null`
  unmarshaled into a non-pointer Go field rather than erroring — a plain
  `json.Unmarshal` into typed struct fields could not have distinguished
  "missing" or "null" from a legitimate zero value.

Semantic failures (`validateAgentReportSemantics`, close 1008), all pinned
bounds:

- Trimmed `target` non-empty; raw (untrimmed) `target` ≤ 255 bytes.
- `checkedAt` parses as a non-zero RFC3339 timestamp.
- ≤ 10,000 containers.
- Each container ID: raw length ≤ 256 bytes, `strings.TrimSpace(id)`
  non-empty, unique by exact transmitted string.
- `restartCount >= 0`; `health` ∈ {`healthy`, `unhealthy`, `starting`,
  `none`}.
- `name`/`image`/`imageId`/`state`/`status`/`health` ≤ 4,096 bytes each.
- Each `repoDigests` entry ≤ 4,096 bytes; ≤ 256 digests.
- Each label key/value ≤ 4,096 bytes; ≤ 32 labels.

`containers: null` is only ever a schema failure (missing/null top-level
field, close 1007) — decoding never reaches the semantic container-count
check in that case, so it is never double-counted as a semantic failure
too.

**Deviation, `health`'s 4,096-byte boundary**: the spec lists `health`
among the scalars "bounded to 4,096 bytes," but `health` is also
constrained to exactly one of four short enum strings by the semantic
validity check (longest valid value, `unhealthy`, is 9 bytes). A `health`
value long enough to probe the 4,096/4,097-byte boundary can therefore
never simultaneously be a valid enum member, making an "accepted at 4,096
bytes" case for `health` specifically unconstructible. `TestAgentReportWireSchemaBoundaries`
exercises the shared length-check code path (identical for all six
scalars) on the five fields where it is actually observable
(`name`/`image`/`imageId`/`state`/`status`), and `health`'s own enum
validity is separately covered by
`TestAgentReportResponseMatrix/semantic_health_invalid_value`.

## E2E plan

1. **Collect-before-dial and fresh ack timing**
   (`TestSendOnceCollectsAndNormalizesBeforeDial`): a fake Docker
   `/containers/json` handler blocks on a channel; a fake WebSocket server
   increments an atomic handshake counter on every upgrade. Production
   `sendOnce` is run in a goroutine. After a 200ms sleep, the handshake
   counter is asserted at 0. Releasing the Docker block lets collection
   finish; the handshake counter becomes 1 once the report arrives at the
   fake server. The fake server replies immediately with a valid `persisted`
   acknowledgement, under a package-var-shrunk `agentAckReadTimeout`
   (150ms) that is shorter than the artificial collection delay: if the ack
   deadline had (incorrectly) started before collection/dial/write, this
   reply would already be too late and `sendOnce` would return
   `errAgentAcknowledgementTimeout` instead of `nil`. The captured raw
   report bytes are decoded and asserted to have non-null `containers` and
   non-null per-container `repoDigests`. A separate, fast subtest calls
   `collectDocker`+`normalizeAgentMessage` directly against an empty Docker
   container list to confirm the empty-collection case also normalizes to
   a non-null empty slice.
2. **Handshake/dial classification**
   (`TestSendOnceClassifiesHandshakeRejection`,
   `TestSendOnceClassifiesConnectionFailures`): a plain (non-upgrading)
   HTTP handler returning 401/403 proves `errAgentServerRejection`, with an
   assertion that the returned error text never contains the configured
   agent token. A closed-then-released TCP port and a malformed
   `ws://%zz` server URL both prove `errAgentConnectionFailure`.
3. **Acknowledgement matrix** (`TestSendOnceAcknowledgementMatrix`,
   `TestSendOnceRejectsOversizedAcknowledgement`): a fake WebSocket server
   reads the report, then replies with a table-driven set of raw
   acknowledgement payloads (all four valid objects, every invalid status/
   code pairing, unknown status, unknown code, missing fields, unknown
   fields, malformed JSON, a binary message, an oversized message, or an
   immediate close with no reply), asserting the exact resulting
   classification (or success) via `errors.Is`.
4. **Full protocol response matrix**
   (`TestAgentReportResponseMatrix`): against the real `internal/app`
   handler, drives every row of the Protocol contract table plus the
   refreshed-deadline and no-leaked-secrets properties, using raw
   WebSocket frames (including a deliberately unmasked client frame,
   written directly to the underlying connection, to trigger Gorilla's own
   protocol-error close) where the schema/semantic layer cannot be reached
   by construction.
5. **Wire schema boundaries and missing/null fields**
   (`TestAgentReportWireSchemaBoundaries`,
   `TestAgentReportWireSchemaMissingAndNullFields`): table- and
   subtest-driven coverage of every documented byte/count boundary and
   every required-field presence/nullness combination, against the real
   handler.
6. **Persists-before-acknowledgement proof**
   (`TestAgentReportPersistsBeforeAcknowledgement`): the `applyAgentReport`
   seam is overridden to call through to production persistence, close a
   `persisted` channel, then block on a `releaseAck` channel before
   returning. The test confirms the store already contains the persisted
   report immediately after `persisted` fires (i.e. before the handler can
   possibly have written an acknowledgement), confirms no acknowledgement
   has arrived yet while `releaseAck` remains closed, then releases it and
   confirms the acknowledgement arrives only afterward.
7. **Full-stack E2E** (`TestAgentReportAcknowledgementE2E`, in
   `internal/agent`, importing `internal/app`): production `sendOnce`
   against a real `internal/app` server (real auth, real upgrade, real
   `monitor.ApplyAgentReport`, real SQLite persistence) and a fake Docker
   API. Asserts `sendOnce` returns success only once the report is fully
   persisted, then confirms the same data is queryable via a real
   `/api/summary` request.

## Observed results

- Zero handshakes occurred while Docker collection was blocked; exactly one
  handshake occurred after release; `sendOnce` returned `nil` despite a
  150ms ack deadline racing a 200ms+ artificial collection delay, proving
  the deadline is applied fresh after collection/dial/write
  (`TestSendOnceCollectsAndNormalizesBeforeDial`).
- 401 and 403 handshake responses both classified as `server_rejection`
  with no token leakage; a closed port and a malformed server URL both
  classified as `connection_failure`
  (`TestSendOnceClassifiesHandshakeRejection`,
  `TestSendOnceClassifiesConnectionFailures`).
- All 15 acknowledgement-matrix cases (4 valid/success plus 11 invalid
  variants) resolved to their exact expected classification or success
  (`TestSendOnceAcknowledgementMatrix`); an 8 KiB acknowledgement (over the
  4 KiB agent-side limit) classified as `protocol_failure`
  (`TestSendOnceRejectsOversizedAcknowledgement`).
- Every row of the server response table matched exactly, including an
  unmasked raw frame producing close 1002 and a shrunk `agentWSReadLimit`
  producing close 1009 with no acknowledgement; a persistence stage slower
  than a shrunk `agentWSWriteWait` still produced a successful `persisted`
  acknowledgement, proving the write deadline is refreshed immediately
  before the write rather than inherited from earlier in the connection's
  lifetime; three independent sentinel values (an agent token, a rejected
  field name, an injected persistence error) were all absent from every
  acknowledgement, close reason, HTTP 401 body, and the captured log buffer
  (`TestAgentReportResponseMatrix`, all subtests including
  `no_sensitive_data_leaks`).
- Every documented byte/count boundary (target, container ID, container
  count, five scalar fields, digests, labels) and every required-field
  missing/null/unknown-field/non-string-label combination resolved to the
  exact expected schema or semantic outcome
  (`TestAgentReportWireSchemaBoundaries`,
  `TestAgentReportWireSchemaMissingAndNullFields`).
- The store already contained the persisted report the instant the
  `applyAgentReport` seam signaled persistence complete — strictly before
  any acknowledgement could have been written — and the acknowledgement
  reader received nothing until the seam was explicitly released
  (`TestAgentReportPersistsBeforeAcknowledgement`).
- The full-stack E2E run returned success from `sendOnce` only once
  `/api/summary` already reflected the persisted `serenity` target with its
  one reported container (`TestAgentReportAcknowledgementE2E`).
- While developing this suite, `-race` caught a pre-existing latent
  goroutine-lifecycle issue unrelated to the wire protocol itself:
  `net/http/httptest.Server.Close` does not wait for hijacked (WebSocket)
  connections, so a connection's keepalive ping goroutine could still be
  reading the package-level deadline variables briefly after a test
  believed its server was fully shut down, racing the next test's mutation
  of those same variables under `-race`. Fixed with a new
  `App.agentConnsWG sync.WaitGroup` (mirroring the existing `actionsWG`
  pattern already used for other background work in `App.Close`) that
  `agentConnect` holds for its full lifetime including the ping goroutine;
  tests that mutate the shared deadline/limit variables now explicitly
  wait on it before restoring them. `App.Close` itself was deliberately
  left unchanged (not wired to wait on `agentConnsWG`) to avoid any
  production shutdown-latency regression — see Maintainability sweep.

## Verification

All commands were run from the worktree.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/agent ./internal/app -list 'Test(AgentReportResponseMatrix|AgentReportWireSchemaBoundaries|AgentReportWireSchemaMissingAndNullFields|AgentReportRejectsBinaryMessage|AgentReportPersistsBeforeAcknowledgement|SendOnceCollectsAndNormalizesBeforeDial|SendOnceClassifiesHandshakeRejection|SendOnceClassifiesConnectionFailures|SendOnceAcknowledgementMatrix|SendOnceRejectsOversizedAcknowledgement|AgentReportAcknowledgementE2E)'
TestAgentReportResponseMatrix
TestAgentReportWireSchemaBoundaries
TestAgentReportWireSchemaMissingAndNullFields
TestAgentReportRejectsBinaryMessage
TestAgentReportPersistsBeforeAcknowledgement
TestSendOnceCollectsAndNormalizesBeforeDial
TestSendOnceClassifiesHandshakeRejection
TestSendOnceClassifiesConnectionFailures
TestSendOnceAcknowledgementMatrix
TestSendOnceRejectsOversizedAcknowledgement
TestAgentReportAcknowledgementE2E
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/agent ./internal/app -run "^(Test(AgentReportResponseMatrix|AgentReportWireSchemaBoundaries|AgentReportWireSchemaMissingAndNullFields|AgentReportRejectsBinaryMessage|AgentReportPersistsBeforeAcknowledgement|SendOnceCollectsAndNormalizesBeforeDial|SendOnceClassifiesHandshakeRejection|SendOnceClassifiesConnectionFailures|SendOnceAcknowledgementMatrix|SendOnceRejectsOversizedAcknowledgement|AgentReportAcknowledgementE2E))$" -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.261s
ok  	github.com/example/gitops-dashboard/internal/app	1.768s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/agent -run '^TestAgentReportAcknowledgementE2E$' -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.054s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/agent ./internal/app -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.268s
ok  	github.com/example/gitops-dashboard/internal/app	2.514s
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test -race ./internal/agent ./internal/app -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	1.388s
ok  	github.com/example/gitops-dashboard/internal/app	8.304s
```

Also confirmed clean across three consecutive `-race` runs of
`./internal/app` alone while diagnosing the goroutine-lifecycle issue above.

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1
ok  	github.com/example/gitops-dashboard/internal/agent	0.308s
ok  	github.com/example/gitops-dashboard/internal/alerter	0.334s
ok  	github.com/example/gitops-dashboard/internal/app	3.511s
ok  	github.com/example/gitops-dashboard/internal/auth	0.011s
ok  	github.com/example/gitops-dashboard/internal/ci	2.557s
ok  	github.com/example/gitops-dashboard/internal/config	0.044s
ok  	github.com/example/gitops-dashboard/internal/core	0.011s
ok  	github.com/example/gitops-dashboard/internal/dockerapi	0.014s
ok  	github.com/example/gitops-dashboard/internal/environment	0.006s
?   	github.com/example/gitops-dashboard/internal/hostinventory	[no test files]
ok  	github.com/example/gitops-dashboard/internal/monitor	6.420s
ok  	github.com/example/gitops-dashboard/internal/parser	0.034s
ok  	github.com/example/gitops-dashboard/internal/routetarget	0.016s
ok  	github.com/example/gitops-dashboard/internal/sanitizer	0.012s
ok  	github.com/example/gitops-dashboard/internal/scanner	3.106s
ok  	github.com/example/gitops-dashboard/internal/storage	7.064s
?   	github.com/example/gitops-dashboard/internal/ui	[no test files]
?   	github.com/example/gitops-dashboard/internal/version	[no test files]
```

```text
$ GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local make check
(see full transcript captured at commit time; gofmt, UI build/lint/typecheck,
go vet, the full Go test suite including internal/app and internal/agent,
Playwright, and scripts/release_test.sh all ran against a clean checkout
copied from tracked files, all green)
```

```text
$ git diff --check
```

No output; exited 0.

## Documentation sweep

- `docs/vision.md`: reviewed; no change. Product vision is unaffected by an
  internal wire-protocol hardening change.
- `docs/requirements.md`: reviewed; no change. R-4/N-1/N-2/N-4/N-5 are
  task-local IDs defined in this record, distinct from the project ID
  space, per the same convention T-060/T-061/T-062 used.
- `docs/tech_stack.md`, `docs/implementation_plan.md`,
  `docs/task_acceptance_criteria.md`, `docs/deployment.md`,
  `docs/versioning.md`, `docs/discovery.md`: reviewed; no change.
- `docs/tasks/tracker.md`: updated to add the `TASK-0063` row and keep
  `Next Task ID` at `TASK-0065` (the spec-review-approved value; not
  advanced by this task).

## Maintainability sweep

- All new report/acknowledgement wire logic lives in two new, focused
  files mirroring the existing per-feature split
  (`ping.go`/`http_routes.go` in `internal/monitor`,
  `kubernetes_bounds.go` from T-062): `internal/app/agent_report.go`
  (server-side strict schema decode, semantic validation, and the private
  ack type) and `internal/agent/report.go` (agent-side classification
  sentinels, timeouts, and its own private ack type), rather than growing
  the already-large `app.go`/`agent.go` further than the handler/`sendOnce`
  rewrites themselves required.
- The duplicated private `agentReportAck` type in both packages is
  deliberate per the spec (`internal/core` is frozen; a shared-core ack
  type is explicitly prohibited), not an oversight — each copy is `internal`
  to its own package and the two are kept structurally identical only by
  convention, not by a shared definition.
- `handleAgentReport`/`respondAgentReport` replace the prior unbounded
  `for { conn.ReadJSON(...) }` loop with a single linear
  read-validate-authorize-persist-acknowledge sequence, which is both what
  the one-report-per-connection protocol requires and strictly simpler to
  read than the loop it replaces.
- `App.applyAgentReport` follows the same test-seam pattern already
  established by `App.scanAll`/`App.checkAll` (function fields defaulting
  to the real method, overridable in tests) rather than introducing a new
  mechanism.
- The `agentConnsWG` fix (see Observed results) is the minimum-diff way to
  give tests a reliable synchronization point for a goroutine whose
  lifetime `httptest.Server.Close` cannot observe (hijacked connections
  are removed from its tracked-connection set immediately on upgrade).
  `App.Close` was deliberately left unmodified rather than also waiting on
  `agentConnsWG`, since doing so would make process shutdown block on
  slow/idle agent connections (bounded only by the existing 60s
  `agentWSPongWait`) for no functional benefit in production, where no
  code currently depends on `Close` draining agent connections
  synchronously.
- No config fields, negotiation, retransmission, or persisted-format
  changes were added. `core.AgentMessage`/`core.ContainerStatus` are
  unchanged; the existing `inferContainerHealth` and
  `core.FilterDockerComposeLabels`/`core.FilterAgentMessageDockerLabels`
  helpers are reused as-is, not duplicated.
- No unrelated refactors were made beyond what the protocol rewrite, its
  test suite, and the `-race` goroutine-lifecycle fix required.

## Commit evidence

Task base commit: `117060acd94edff6e4ab02e6bd538ad39c9f6355`

Proposed commit subject: `feat(app): acknowledge persisted agent reports and collect before dial (T-063)`

Proposed PR title: `feat(app): acknowledge persisted agent reports and collect before dial (T-063)`
