# TASK-0067: Post-landing consolidation: shared monitor failure writer, single agent-ack vocabulary, agent.Run trusts validated config

## Status

Proposed

## Scope

`internal/monitor` (Kubernetes and generic per-service failure writers),
`internal/app` + `internal/agent` (agent-ack protocol vocabulary, extracted
to a new `internal/agentprotocol` package), `internal/agent` (`Run`'s config
handling). Items 1–2 are behavior-preserving refactors; item 3 is one
deliberate, spec-called-out behavior change.

Satisfies survey 2026-07-25 IR-3 (F3+F4+F5/P1).

## Dependencies

None (no file overlap with TASK-0066: that task is `internal/storage` +
config redaction only; TASK-0046/T-046 is docker inspection, different
code).

## Task base commit

`ee0d9cad4bdb36b49b331c9f785b9e301609062f` (main, after TASK-0066/v0.0.17).

## Spec line-number drift found

The spec was written against `3dd9ece3` (before TASK-0065/TASK-0066
landed) and carried a rebase-awareness warning. Verified against the base
commit above:

- `internal/monitor/kubernetes_bounds.go` and `internal/monitor/monitor.go`
  citations (`recordKubernetesTargetFailure` 159-177,
  `kubernetesFailureWriteTimeout` line 39, `recordTargetFailure` 366-376,
  `statusWriteContext` 438-448, `statusWriteTimeout` line 29,
  `handleKubernetesCheckFailure` 185-194): **no drift**, every line matched
  exactly.
- `internal/app/agent_report.go` / `internal/agent/report.go` citations
  (ack struct/const block 19-45 / 54-87, JSON-null helpers 265-267 / 135-137):
  **no drift**, every line matched exactly.
- `internal/agent/agent.go:17-19` (silent nil) and `:20-25` (inline interval
  parse): **no drift**.
- `internal/config/config.go`: **drifted**. `AgentConfig.IntervalDuration()`
  is at line 1409 (spec cited 1641-1643) and `Config.ValidateAgent()` is at
  line 1001 (spec cited 1233-1245). Both functions still exist with the
  semantics the spec described (`ValidateAgent` rejects empty
  `serverUrl`/`target`/`token` and calls `IntervalDuration()`, which rejects
  a malformed interval and returns `0, nil` when unset); only the line
  numbers moved, most likely from TASK-0065/TASK-0066 edits earlier in the
  file. `LoadForMode`'s agent-mode branch (line 260) still calls
  `resolveAgentSecrets` then `ValidateAgent` before returning, confirming
  the only supported call path (`cmd/gitops-dashboard/main.go` agent mode)
  still validates at boot with the stronger semantics the spec relies on.

## Requirements

Verbatim from the spec, each mapped to what/where:

- [x] Items 1–2 behavior-preserving as observed by the existing suite: the
      full existing Go test suite passes unmodified, except for tests that
      named the moved private ack-vocabulary symbols (see below — those
      symbols moved package, from `internal/app`/`internal/agent` private
      constants to exported `internal/agentprotocol` identifiers).
- [x] Exactly one shared per-service target-failure writer in
      `internal/monitor`: `recordTargetFailureLogged`
      (`internal/monitor/monitor.go`) via `upsertMonitorStatus`/
      `statusWriteContext`, with `recordTargetFailure` as its
      default-log-message entry point. `handleKubernetesCheckFailure`
      (`internal/monitor/kubernetes_bounds.go`) now routes both the deadline
      branch and the non-deadline branch through this one writer;
      `recordKubernetesTargetFailure` and `kubernetesFailureWriteTimeout`
      were deleted. **Deliberately unified** (see rationale below) for the
      write budget and cancellation gate, not preserved-exactly, since no
      existing test pinned the 2s-vs-30s budget or the "any parent error"
      vs "Canceled-only" cancellation gate (spec review confirmed this at
      dispatch). The two callers' operational log messages
      (`"persist kubernetes target failure"` vs
      `"persist monitor target failure"`) are **not** unified — that is an
      explicit Out of scope item — so `recordTargetFailureLogged` takes the
      message as a parameter and the deadline branch passes its own.
- [x] Persisted failure row unchanged: `Health = core.HealthError`,
      `Message = "monitor target check failed"`, now defined at exactly one
      site, `internal/monitor/monitor.go`'s `recordTargetFailure` (the
      duplicate literal in the deleted `recordKubernetesTargetFailure` is
      gone).
- [x] Kubernetes failure routing unchanged: `handleKubernetesCheckFailure`
      still special-cases `context.DeadlineExceeded` (writes only
      `applicableKubernetesServices`, the same selection
      `recordKubernetesTargetFailure` used) and `context.Canceled` (writes
      nothing), and still falls back to `runtimeServices(services,
      "kubernetes")` (every kubernetes-runtime service, not just
      applicable-kind ones) for every other error — only the underlying
      *writer* the deadline branch calls changed, not which services it
      covers or when it declines to write.
- [x] Agent-ack protocol vocabulary — type `agent_report_ack`, statuses
      `ok`/`error`, codes `persisted`/`unauthorized_target`/
      `invalid_report`/`persistence_failed`, plus the JSON-null and
      strict-decode helpers — now defined once, in new package
      `internal/agentprotocol` (`internal/agentprotocol/ack.go`; not
      `internal/core`), consumed by both `internal/app`
      (`agent_report.go`, `app.go`) and `internal/agent` (`report.go`,
      `agent.go`). The per-side `agentReportAck` wire structs stayed
      private and separate in each package, per the spec and the existing
      in-code rationale (internal/core is frozen for this protocol).
- [x] Deliberate behavior change, item 3 only: `internal/agent/agent.go`'s
      `Run` now returns `errors.New("agent.serverUrl and agent.token are
      required")` instead of silent `nil` when `ServerURL`/`Token` is
      empty, and obtains the interval via `cfg.Agent.IntervalDuration()`,
      defaulting to 30s only when `IntervalDuration()` returns `0, nil`
      (unset). A malformed interval now propagates as an error instead of
      silently falling back to 30s; this path stays unreachable via the
      only supported call path (`LoadForMode` validates first). No
      behavior change for a validated config: `main.go`'s agent-mode branch
      already treated a `Run` error as fatal (`os.Exit(1)`), so the new
      error return only matters for a config that reaches `Run` unvalidated
      (not possible via the supported CLI path).
- [x] Test seams: none of the three items introduced or needed a new test
      seam; no new package-level mutable seams, process-global values, or
      nil-checked production fallbacks were added.
- [x] In-repo record: this file, plus the `docs/tasks/tracker.md` row below.
      `Next Task ID` left at `TASK-0065` per dispatch instruction.

## Failure-writer decision: unify, not preserve-exactly

Chose to unify the Kubernetes deadline-failure write onto the same writer
(`recordTargetFailure`/`statusWriteContext`, 30s budget, refuse-on-Canceled-
only / escape-`DeadlineExceeded`-via-`WithoutCancel` gate) already used by
every other runtime (docker, HTTP routes, ping), rather than keep a second,
narrower writer (2s budget, refuse-on-any-parent-error) just for Kubernetes.

Rationale:

- No existing test pinned either the 2s-vs-30s budget or the
  any-parent-error-vs-Canceled-only gate (verified above and at spec
  review), so the "unification latitude" the spec grants applies cleanly.
- The write itself is a local SQLite `UpsertStatus` call, not a Kubernetes
  API call — the 30s generic budget (already the norm for three of four
  runtimes) is not meaningfully riskier for this local write than the 2s
  Kubernetes-specific one was; a stalled local disk would already be a
  bigger problem than either budget addresses.
- Keeping Kubernetes on the generic writer removes an entire bespoke
  function, its dedicated constant, and its duplicated failure-message
  literal, which is the actual point of a "collapse to single
  implementation" task — a second writer that merely delegates its
  budget/gate to the generic one would still be two functions.
- The two writers' *service-selection* logic (which services a given
  failure touches) is untouched and intentionally still differs between
  the deadline branch (`applicableKubernetesServices`) and the
  non-deadline branch (`runtimeServices(..., "kubernetes")`) — that
  distinction is orthogonal to the writer being unified and is covered by
  `TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched`, which
  passed unmodified.

## The new shared package: `internal/agentprotocol`

`internal/agentprotocol/ack.go` (package `agentprotocol`), consumed by
`internal/app` and `internal/agent`; not `internal/core`, which the
existing in-code comments in both consumer packages state is frozen for
this protocol.

Exports:

- `AckType = "agent_report_ack"`
- `AckStatusOK = "ok"`, `AckStatusError = "error"`
- `AckCodePersisted = "persisted"`, `AckCodeUnauthorizedTarget =
  "unauthorized_target"`, `AckCodeInvalidReport = "invalid_report"`,
  `AckCodePersistenceFailed = "persistence_failed"`
- `IsJSONNull(json.RawMessage) bool`
- `DecodeStrictJSONObject(*json.Decoder, map[string]struct{})
  (map[string]json.RawMessage, error)` — the generic decode-exactly-one-
  object-with-only-allowed-keys idiom, previously duplicated as
  `internal/app`'s private `decodeStrictJSONObject` and hand-inlined again
  in `internal/agent`'s `decodeAgentReportAck`.
- `EnsureNoTrailingJSON(*json.Decoder) error` — previously
  `internal/app`'s private `ensureNoTrailingJSON`, hand-inlined again in
  `internal/agent`.
- `StringField{Name string; Dest *string}` and `DecodeStringFields(...)
  error` — the `{name, dest}` string-field decode loop, previously
  duplicated between `internal/app`'s container-field loop and
  `internal/agent`'s ack-field loop.

Both consumer packages' private `agentReportAck` wire structs (with/without
JSON tags, per their side's marshal/unmarshal need) are unchanged and stay
separate, per the spec.

## Out of scope

Not attempted, per the spec: changing wire protocol messages, close codes,
or ack semantics; moving wire structs into `internal/core`; unifying the
two writers' log messages — `handleKubernetesCheckFailure`'s deadline
branch still logs `"persist kubernetes target failure"` on a write error,
distinct from every other caller's `"persist monitor target failure"`,
via `recordTargetFailureLogged`'s log-message parameter (see the
Requirements entry above; an earlier draft of this change collapsed the
two messages as an unintended side effect of sharing the writer function
and was corrected before landing, commit `7737ce7`); the docker inspection
refactor (TASK-0046/T-046); `internal/storage` (TASK-0066/T-066); the
alerter (TASK-0065/T-065); adding panic recovery (closed out of bar,
survey 2026-07-25 IR-2).

## Verification

```sh
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go build ./...
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/monitor ./internal/app ./internal/agent -count=1
GOCACHE=/tmp/gitops-dashboard-go-cache GOTOOLCHAIN=local go test ./internal/... -count=1

# ack wire-type literal defined in exactly one non-test file (pass = 1)
rg -l '"agent_report_ack"' --type go -g '!*_test.go' internal/ | wc -l

# no inline duration parsing left in the agent package (pass = prints nothing)
rg -n 'ParseDuration' internal/agent -g '!*_test.go' && exit 1 || true
```

All commands pass. Observed:

- `go build ./...`: clean, no output.
- `go test ./internal/monitor ./internal/app ./internal/agent -count=1`:
  `ok` for all three (monitor ~6s, app ~3.5s, agent ~0.3s).
- `go test ./internal/... -count=1`: `ok` for every package with tests; no
  failures.
- `rg -l '"agent_report_ack"' ... | wc -l`: `1` (only
  `internal/agentprotocol/ack.go`).
- `rg -n 'ParseDuration' internal/agent -g '!*_test.go'`: no output (the
  `&& exit 1 || true` guard did not trigger).

Also run and green: `make check` (format, `go vet ./cmd/... ./internal/...`,
`go test ./cmd/... ./internal/...`, UI typecheck/lint/build, 16/16
Playwright e2e, release test) — run twice, once before and once after
splitting/finalizing commits, both fully green.

## Commit evidence

Task base commit: `ee0d9cad4bdb36b49b331c9f785b9e301609062f`

Commits (in order):

- `351a0b7` — `refactor(monitor): unify kubernetes and generic target-failure writers (T-067)`
- `0c177bb` — `refactor(app): extract shared agent-ack protocol vocabulary (T-067)`
- `f925c95` — `refactor(agent): consume shared agent-ack protocol vocabulary (T-067)`
- `85e0a34` — `fix(agent): trust validated config instead of silently defaulting (T-067)`
- `7737ce7` — `refactor(monitor): keep the kubernetes deadline write's log message distinct (T-067)`
- `docs(tasks): add TASK-0067 record (T-067)` (this file + tracker row)

Each of the five code commits builds and passes its directly affected
package's tests standalone (verified individually before finalizing, not
just as a final squashed state) — `351a0b7` and `7737ce7` both against
`internal/monitor`, `0c177bb` against `internal/app` +
`internal/agentprotocol`, `f925c95` and `85e0a34` against `internal/agent`.
