package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
)

// --- shared test scaffolding -------------------------------------------

// recordingHandler is a minimal slog.Handler that captures every record, so
// tests can assert on exact log level/message counts instead of parsing
// formatted output.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) messagesAtLevel(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var messages []string
	for _, r := range h.records {
		if r.Level == level {
			messages = append(messages, r.Message)
		}
	}
	return messages
}

func (h *recordingHandler) countContaining(level slog.Level, substr string) int {
	n := 0
	for _, message := range h.messagesAtLevel(level) {
		if strings.Contains(message, substr) {
			n++
		}
	}
	return n
}

type alertEvaluatorTestOptions struct {
	agentTargets    []string
	repositories    []string
	defaultInterval string
	sinkEnabled     bool
}

// newAlertEvaluatorTestApp builds a real App (real SQLite-backed store, no
// mocks) configured for alert-evaluator testing. Alerting is wired the same
// way production New() wires it, so tests exercise the same allowlist,
// redaction, and cooldown/dedupe machinery production traffic does.
func newAlertEvaluatorTestApp(t *testing.T, opts alertEvaluatorTestOptions) (*App, *recordingHandler, string) {
	t.Helper()
	handler := &recordingHandler{}
	logger := slog.New(handler)

	dockerTargets := make([]config.DockerTarget, len(opts.agentTargets))
	for i, name := range opts.agentTargets {
		dockerTargets[i] = config.DockerTarget{Name: name, Kind: "agent"}
	}
	repos := make([]config.RepositoryConfig, len(opts.repositories))
	for i, name := range opts.repositories {
		repos[i] = config.RepositoryConfig{Name: name, URL: "https://example.invalid/" + name + ".git"}
	}
	interval := opts.defaultInterval
	if interval == "" {
		interval = "1h"
	}
	alerting := config.AlertingConfig{StabilitySamples: 1}
	if opts.sinkEnabled {
		alerting.Sinks.Webhook = config.WebhookAlertSinkConfig{
			Enabled: true, URL: "http://127.0.0.1:9/webhook", Method: "POST", Timeout: "2s",
		}
	}
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring:   config.MonitoringConfig{DefaultInterval: interval},
		Runtime:      config.RuntimeConfig{Docker: dockerTargets},
		Repositories: repos,
		Alerting:     alerting,
	}
	app, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	dbPath := filepath.Join(dataDir, "gitops-dashboard.db")
	return app, handler, dbPath
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type capturedAlertEvent struct {
	Kind, Repository, Agent, OldState, NewState, Reason, DedupeKey string
}

func pendingAlertEvents(t *testing.T, store *storage.Store) []capturedAlertEvent {
	t.Helper()
	events, err := store.ListUndeliveredAlertEvents(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]capturedAlertEvent, len(events))
	for i, event := range events {
		out[i] = capturedAlertEvent{
			Kind: event.Kind, Repository: event.Repository, Agent: event.Agent,
			OldState: event.OldState, NewState: event.NewState, Reason: event.Reason, DedupeKey: event.DedupeKey,
		}
	}
	return out
}

func upsertAgent(t *testing.T, store *storage.Store, target string, staleAfter, receivedAt time.Time) {
	t.Helper()
	if err := store.UpsertAgentReport(context.Background(), core.AgentMessage{Target: target, StaleAfter: staleAfter}, nil, receivedAt); err != nil {
		t.Fatal(err)
	}
}

// corruptAgentStaleAfter writes a value into agents.stale_after that cannot
// parse as RFC3339, via a second raw connection to the same database file
// (the storage package intentionally exposes no API to persist malformed
// data). This is the only way to exercise the "malformed freshness" unknown
// classification, since every public write path only ever persists a valid
// timestamp or an empty string.
func corruptAgentStaleAfter(t *testing.T, dbPath, target, value string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), `UPDATE agents SET stale_after=? WHERE target=?`, value, target); err != nil {
		t.Fatal(err)
	}
}

func finishScan(t *testing.T, store *storage.Store, repo string, scanErr error) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := store.StartScan(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, id, repo, "deadbeef", nil, scanErr); err != nil {
		t.Fatal(err)
	}
	return id
}

func startRunningScan(t *testing.T, store *storage.Store, repo string) int64 {
	t.Helper()
	id, err := store.StartScan(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// --- required scheduler tests -------------------------------------------

func TestAlertEvaluatorEnabledStartsSingleton(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{sinkEnabled: true, defaultInterval: "1h"})
	var calls int32
	app.alertEvaluate = func(context.Context) { atomic.AddInt32(&calls, 1) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.RunBackground(ctx)

	waitForCondition(t, time.Second, func() bool { return atomic.LoadInt32(&calls) >= 1 })
	// The interval is an hour, so no second tick can plausibly fire during
	// this window; any count above 1 indicates more than one scheduler
	// running (or a repeated immediate start) rather than the required
	// single long-lived goroutine.
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("evaluate calls = %d, want exactly 1 (singleton scheduler, immediate first evaluation, long interval)", got)
	}
}

func TestAlertEvaluatorDisabledAlertingStartsNoScheduler(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{sinkEnabled: false, defaultInterval: "5ms"})
	var calls int32
	app.alertEvaluate = func(context.Context) { atomic.AddInt32(&calls, 1) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.RunBackground(ctx)
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("evaluate calls = %d, want 0 when alerting is disabled", got)
	}
}

func TestAlertEvaluatorDoesNotOverlap(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{sinkEnabled: true, defaultInterval: "5ms"})
	var active int32
	var overlapped int32
	var calls int32
	app.alertEvaluate = func(context.Context) {
		if atomic.AddInt32(&active, 1) > 1 {
			atomic.AddInt32(&overlapped, 1)
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		atomic.AddInt32(&calls, 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.RunBackground(ctx)
	waitForCondition(t, 2*time.Second, func() bool { return atomic.LoadInt32(&calls) >= 4 })
	if got := atomic.LoadInt32(&overlapped); got != 0 {
		t.Fatalf("overlapping evaluate calls = %d, want 0", got)
	}
}

func TestAlertEvaluatorIntervalStartsAfterCompletion(t *testing.T) {
	t.Parallel()
	interval := 60 * time.Millisecond
	work := 40 * time.Millisecond
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{sinkEnabled: true, defaultInterval: interval.String()})
	var mu sync.Mutex
	var starts, ends []time.Time
	app.alertEvaluate = func(context.Context) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(work)
		mu.Lock()
		ends = append(ends, time.Now())
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.RunBackground(ctx)
	waitForCondition(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ends) >= 2
	})

	mu.Lock()
	defer mu.Unlock()
	gap := starts[1].Sub(ends[0])
	// If the interval were (wrongly) measured from the previous start
	// rather than completion, the gap here would be close to
	// interval-work (~20ms) instead of ~interval (~60ms).
	if gap < interval-15*time.Millisecond {
		t.Fatalf("second evaluation started %s after the first completed, want at least ~%s (interval must be measured from completion, not start)", gap, interval)
	}
}

func TestAlertEvaluatorInvalidDefaultIntervalDisablesOnlyEvaluator(t *testing.T) {
	t.Parallel()
	for _, interval := range []string{"not-a-duration", "0s", "-5s"} {
		t.Run(interval, func(t *testing.T) {
			t.Parallel()
			app, handler, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{sinkEnabled: true, defaultInterval: interval})
			var calls int32
			app.alertEvaluate = func(context.Context) { atomic.AddInt32(&calls, 1) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			app.RunBackground(ctx)
			time.Sleep(80 * time.Millisecond)

			if got := atomic.LoadInt32(&calls); got != 0 {
				t.Fatalf("evaluate calls = %d, want 0 for invalid interval %q", got, interval)
			}
			if got := handler.countContaining(slog.LevelError, "alert evaluator"); got != 1 {
				t.Fatalf("alert-evaluator error log lines = %d, want exactly 1 for interval %q (messages: %v)", got, interval, handler.messagesAtLevel(slog.LevelError))
			}
		})
	}
}

// --- agent state machine -------------------------------------------------

func TestAlertEvaluatorAgentNeverReportedIsUnknown(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, sinkEnabled: true})
	app.alertEvaluator.evaluate(context.Background())
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events for a never-reported agent = %v, want none", got)
	}
}

func TestAlertEvaluatorAgentMalformedStaleAfterIsUnknown(t *testing.T) {
	t.Parallel()
	app, _, dbPath := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, sinkEnabled: true})
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", now.Add(time.Hour), now)
	corruptAgentStaleAfter(t, dbPath, "alpha", "not-a-timestamp")

	app.alertEvaluator.now = func() time.Time { return now }
	app.alertEvaluator.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events for a malformed stale_after = %v, want none", got)
	}
	if app.alertEvaluator.agents["alpha"].established {
		t.Fatal("baseline established from malformed freshness, want it to remain unestablished")
	}
}

func TestAlertEvaluatorAgentExactStaleBoundaryIsOffline(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, sinkEnabled: true})
	ctx := context.Background()
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))

	ev := app.alertEvaluator
	ev.now = func() time.Time { return staleAfter.Add(-time.Second) }
	ev.evaluate(ctx) // silent online baseline
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after online baseline = %v, want none", got)
	}

	ev.now = func() time.Time { return staleAfter } // now == stale_after: offline (equality is offline)
	ev.evaluate(ctx)
	got := pendingAlertEvents(t, app.store)
	if len(got) != 1 || got[0].Kind != "agent.offline" {
		t.Fatalf("events at exact stale boundary = %v, want exactly one agent.offline", got)
	}
}

func TestAlertEvaluatorAgentFailureThenRecoveryEmitsExactEdges(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, sinkEnabled: true})
	ctx := context.Background()
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))

	ev := app.alertEvaluator
	ev.now = func() time.Time { return staleAfter.Add(-time.Minute) }
	ev.evaluate(ctx) // silent online baseline

	ev.now = func() time.Time { return staleAfter.Add(time.Minute) }
	ev.evaluate(ctx) // offline sample: failure transition
	// Repeated offline sample must not re-emit.
	ev.evaluate(ctx)

	got := pendingAlertEvents(t, app.store)
	if len(got) != 1 {
		t.Fatalf("events after failure (with a repeated offline sample) = %v, want exactly one", got)
	}
	want := capturedAlertEvent{
		Kind: "agent.offline", Agent: "alpha", OldState: "online", NewState: "offline",
		Reason:    "agent report is stale",
		DedupeKey: "agent:alpha:agent.offline:" + staleAfter.UTC().Format(time.RFC3339Nano),
	}
	if got[0] != want {
		t.Fatalf("failure event = %+v, want %+v", got[0], want)
	}

	// The agent reports again (new last_seen_at/stale_after) and recovers.
	newStaleAfter := staleAfter.Add(3 * time.Hour)
	newReceivedAt := staleAfter.Add(2 * time.Hour)
	upsertAgent(t, app.store, "alpha", newStaleAfter, newReceivedAt)
	ev.now = func() time.Time { return newReceivedAt }
	ev.evaluate(ctx) // recovery transition
	ev.evaluate(ctx) // repeated online sample must not re-emit

	got = pendingAlertEvents(t, app.store)
	if len(got) != 2 {
		t.Fatalf("events after recovery (with a repeated online sample) = %v, want exactly two", got)
	}
	wantRecovery := capturedAlertEvent{
		Kind: "agent.recovery", Agent: "alpha", OldState: "offline", NewState: "online",
		Reason:    "agent report received",
		DedupeKey: "agent:alpha:agent.recovery:" + newReceivedAt.UTC().Format(time.RFC3339Nano),
	}
	if got[1] != wantRecovery {
		t.Fatalf("recovery event = %+v, want %+v", got[1], wantRecovery)
	}
}

func TestAlertEvaluatorAgentStartupOfflineBaselineThenOnlineEmitsNoRecovery(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, sinkEnabled: true})
	ctx := context.Background()
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-2*time.Hour))

	ev := app.alertEvaluator
	// First observation is already offline: silent baseline, not a handled
	// failure.
	ev.now = func() time.Time { return staleAfter.Add(time.Hour) }
	ev.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after startup-offline baseline = %v, want none", got)
	}

	newStaleAfter := staleAfter.Add(3 * time.Hour)
	newReceivedAt := staleAfter.Add(2 * time.Hour)
	upsertAgent(t, app.store, "alpha", newStaleAfter, newReceivedAt)
	ev.now = func() time.Time { return newReceivedAt }
	ev.evaluate(ctx)

	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after online report following an unhandled startup-offline baseline = %v, want none (recovery requires a handled failure)", got)
	}
}

func TestAlertEvaluatorAgentEnqueueErrorRetriesNextSample(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, sinkEnabled: true})
	ctx := context.Background()
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))

	ev := app.alertEvaluator
	ev.now = func() time.Time { return staleAfter.Add(-time.Minute) }
	ev.evaluate(ctx) // silent online baseline

	// Force the enqueue to fail on the first offline sample by pointing at a
	// sink identity the store's allowlist will reject.
	ev.sinks = []string{"not-a-configured-sink"}
	ev.now = func() time.Time { return staleAfter.Add(time.Minute) }
	ev.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after a failed enqueue = %v, want none", got)
	}
	if ev.agents["alpha"].online != true {
		t.Fatal("baseline advanced to offline despite a failed enqueue, want it to remain online so the next sample retries")
	}

	// Restore a valid sink and retry with the identical still-offline
	// sample: the identity is unchanged, so the retry must succeed exactly
	// once.
	ev.sinks = []string{"webhook"}
	ev.evaluate(ctx)
	got := pendingAlertEvents(t, app.store)
	if len(got) != 1 || got[0].Kind != "agent.offline" {
		t.Fatalf("events after the retried enqueue = %v, want exactly one agent.offline", got)
	}
}

// --- scan state machine ---------------------------------------------------

func TestAlertEvaluatorScanNoTerminalScanIsUnknown(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{repositories: []string{"repo"}, sinkEnabled: true})
	startRunningScan(t, app.store, "repo")
	app.alertEvaluator.evaluate(context.Background())
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events for a repository with only a running scan = %v, want none", got)
	}
}

func TestAlertEvaluatorScanFailureThenRecoveryEmitsExactEdges(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{repositories: []string{"repo"}, sinkEnabled: true})
	ctx := context.Background()
	ev := app.alertEvaluator

	finishScan(t, app.store, "repo", nil) // silent ok baseline
	ev.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after ok baseline = %v, want none", got)
	}

	failID := finishScan(t, app.store, "repo", errors.New("clone failed: exit status 128"))
	ev.evaluate(ctx)
	ev.evaluate(ctx) // repeated error sample must not re-emit

	got := pendingAlertEvents(t, app.store)
	if len(got) != 1 {
		t.Fatalf("events after failure (with a repeat) = %v, want exactly one", got)
	}
	wantFailed := capturedAlertEvent{
		Kind: "scan.failure", Repository: "repo", OldState: "ok", NewState: "error",
		Reason:    "clone failed: exit status 128",
		DedupeKey: fmt.Sprintf("repository:repo:scan.failure:%d", failID),
	}
	if got[0] != wantFailed {
		t.Fatalf("failure event = %+v, want %+v", got[0], wantFailed)
	}

	recoverID := finishScan(t, app.store, "repo", nil)
	ev.evaluate(ctx)
	ev.evaluate(ctx) // repeated ok sample must not re-emit

	got = pendingAlertEvents(t, app.store)
	if len(got) != 2 {
		t.Fatalf("events after recovery (with a repeat) = %v, want exactly two", got)
	}
	wantRecovered := capturedAlertEvent{
		Kind: "scan.recovery", Repository: "repo", OldState: "error", NewState: "ok",
		Reason:    "repository scan recovered",
		DedupeKey: fmt.Sprintf("repository:repo:scan.recovery:%d", recoverID),
	}
	if got[1] != wantRecovered {
		t.Fatalf("recovery event = %+v, want %+v", got[1], wantRecovered)
	}
}

func TestAlertEvaluatorScanFailureFallsBackToDefaultReasonWhenErrorEmpty(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{repositories: []string{"repo"}, sinkEnabled: true})
	ctx := context.Background()
	ev := app.alertEvaluator
	finishScan(t, app.store, "repo", nil)
	ev.evaluate(ctx)
	// FinishScanWithRouteTargetChanges stores an empty error string whenever
	// scanErr is nil, so directly exercise the empty-reason fallback by
	// finishing a scan as an error status with no message via a raw update
	// is unnecessary: passing a non-nil error with an empty message covers
	// the same fallback branch through the public API.
	finishScan(t, app.store, "repo", errors.New(""))
	ev.evaluate(ctx)
	got := pendingAlertEvents(t, app.store)
	if len(got) != 1 || got[0].Reason != "repository scan failed" {
		t.Fatalf("events = %v, want one scan.failure with the fallback reason", got)
	}
}

func TestAlertEvaluatorScanStartupFailedBaselineThenOkEmitsNoRecovery(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{repositories: []string{"repo"}, sinkEnabled: true})
	ctx := context.Background()
	ev := app.alertEvaluator

	finishScan(t, app.store, "repo", errors.New("boom")) // silent error baseline
	ev.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after startup-error baseline = %v, want none", got)
	}

	finishScan(t, app.store, "repo", nil)
	ev.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after an ok scan following an unhandled startup-error baseline = %v, want none (recovery requires a handled failure)", got)
	}
}

func TestAlertEvaluatorScanSanitizesLateRegisteredSecretInReason(t *testing.T) {
	t.Parallel()
	app, handler, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{repositories: []string{"repo"}, sinkEnabled: true})
	ctx := context.Background()
	ev := app.alertEvaluator
	finishScan(t, app.store, "repo", nil)
	ev.evaluate(ctx)

	secret := "ghp_super_secret_token"
	finishScan(t, app.store, "repo", fmt.Errorf("clone failed: token %s rejected", secret))
	// The secret is registered for redaction only after the row was
	// written, mirroring a scanner resolving a repository credential after
	// an earlier failed attempt.
	app.store.AddRedactionValues(secret)
	ev.evaluate(ctx)

	got := pendingAlertEvents(t, app.store)
	if len(got) != 1 || got[0].Kind != "scan.failure" {
		t.Fatalf("events = %v, want one scan.failure", got)
	}
	if strings.Contains(got[0].Reason, secret) {
		t.Fatalf("reason %q leaks a registered secret", got[0].Reason)
	}
	if strings.Contains(got[0].DedupeKey, secret) {
		t.Fatalf("dedupe key %q leaks a registered secret", got[0].DedupeKey)
	}
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		for _, message := range handler.messagesAtLevel(level) {
			if strings.Contains(message, secret) {
				t.Fatalf("log message %q leaks a registered secret", message)
			}
		}
	}
}

// --- cross-cutting: cooldown, independence, cancellation ------------------

func TestAlertEvaluatorCrossAgentCooldownIsolation(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha", "beta"}, sinkEnabled: true})
	app.alertEvaluator.cooldown = time.Hour
	ctx := context.Background()
	ev := app.alertEvaluator

	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))
	upsertAgent(t, app.store, "beta", staleAfter, staleAfter.Add(-time.Hour))

	ev.now = func() time.Time { return staleAfter.Add(-time.Minute) }
	ev.evaluate(ctx) // both online baselines

	ev.now = func() time.Time { return staleAfter.Add(time.Minute) }
	ev.evaluate(ctx) // both go offline in the same evaluation

	got := pendingAlertEvents(t, app.store)
	if len(got) != 2 {
		t.Fatalf("events after both agents go offline = %v, want exactly two (one cooldown must not suppress the other agent)", got)
	}
	agents := map[string]bool{}
	for _, event := range got {
		agents[event.Agent] = true
	}
	if !agents["alpha"] || !agents["beta"] {
		t.Fatalf("events = %v, want one agent.offline for each of alpha and beta", got)
	}
}

func TestAlertEvaluatorProducersEstablishIndependently(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{
		agentTargets: []string{"alpha"}, repositories: []string{"repo"}, sinkEnabled: true,
	})
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))
	finishScan(t, app.store, "repo", nil)

	ev := app.alertEvaluator
	ev.now = func() time.Time { return staleAfter.Add(-time.Minute) }

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// An already-canceled context fails the agent producer's read
	// immediately; the scan producer must still establish its own baseline
	// independently when called with a good context afterward.
	ev.evaluateAgents(canceled)
	if ev.agents["alpha"] != nil && ev.agents["alpha"].established {
		t.Fatal("agent baseline established despite a canceled context, want the agent producer to have failed")
	}
	ev.evaluateScans(context.Background())
	if ev.scans["repo"] == nil || !ev.scans["repo"].established {
		t.Fatal("scan baseline did not establish independently after the agent producer failed")
	}
}

func TestAlertEvaluatorParentCancellationExitsQuietly(t *testing.T) {
	t.Parallel()
	app, handler, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, repositories: []string{"repo"}, sinkEnabled: true})
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))
	finishScan(t, app.store, "repo", nil)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app.alertEvaluator.evaluate(canceled)

	if got := handler.messagesAtLevel(slog.LevelWarn); len(got) != 0 {
		t.Fatalf("warn log messages after parent cancellation = %v, want none (cancellation exits quietly)", got)
	}
	if got := handler.messagesAtLevel(slog.LevelError); len(got) != 0 {
		t.Fatalf("error log messages after parent cancellation = %v, want none", got)
	}
}

func TestAlertEvaluatorTimeoutStopsPromptlyAndLaterEvaluationContinues(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{agentTargets: []string{"alpha"}, repositories: []string{"repo"}, sinkEnabled: true})
	ctx := context.Background()
	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	upsertAgent(t, app.store, "alpha", staleAfter, staleAfter.Add(-time.Hour))
	finishScan(t, app.store, "repo", nil)

	ev := app.alertEvaluator
	ev.now = func() time.Time { return staleAfter.Add(-time.Minute) }

	// A parent context that is already past its deadline stands in for the
	// shared five-second budget being exhausted mid-evaluation: later work
	// (here, the entire evaluation) must stop promptly rather than block.
	exhausted, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	time.Sleep(time.Millisecond)
	ev.evaluate(exhausted)
	if ev.agents["alpha"] != nil && ev.agents["alpha"].established {
		t.Fatal("evaluation proceeded past an exhausted deadline instead of stopping promptly")
	}

	// The next scheduled evaluation, with a fresh budget, must proceed
	// normally and is unaffected by the earlier timeout.
	ev.evaluate(ctx)
	if ev.agents["alpha"] == nil || !ev.agents["alpha"].established {
		t.Fatal("later evaluation did not establish a baseline after an earlier evaluation timed out")
	}
	if ev.scans["repo"] == nil || !ev.scans["repo"].established {
		t.Fatal("later evaluation's scan producer did not run after an earlier evaluation timed out")
	}
}

// --- E2E ------------------------------------------------------------------

// TestAlertEvaluatorPersistedEdgesE2E drives the evaluator directly (not
// through the scheduler, so nothing races the alerter delivery worker) over
// a real configured sink and a real store: baseline, then all four edges,
// then a simulated restart to prove baselines are process-local and lost on
// restart by design.
func TestAlertEvaluatorPersistedEdgesE2E(t *testing.T) {
	t.Parallel()
	app, _, _ := newAlertEvaluatorTestApp(t, alertEvaluatorTestOptions{
		agentTargets: []string{"alpha"}, repositories: []string{"repo"}, sinkEnabled: true,
	})
	ctx := context.Background()
	ev := app.alertEvaluator

	staleAfter := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	firstSeenAt := staleAfter.Add(-time.Hour)
	upsertAgent(t, app.store, "alpha", staleAfter, firstSeenAt)
	finishScan(t, app.store, "repo", nil)

	// Baseline one online agent and one successful repository: silent, no
	// events.
	ev.now = func() time.Time { return staleAfter.Add(-time.Minute) }
	ev.evaluate(ctx)
	if got := pendingAlertEvents(t, app.store); len(got) != 0 {
		t.Fatalf("events after baseline = %v, want none", got)
	}

	// Advance through offline, scan error, online, and scan success.
	ev.now = func() time.Time { return staleAfter.Add(time.Minute) }
	ev.evaluate(ctx) // agent offline

	failID := finishScan(t, app.store, "repo", errors.New("clone failed: exit status 128"))
	ev.evaluate(ctx) // scan error

	recoveredStaleAfter := staleAfter.Add(3 * time.Hour)
	recoveredSeenAt := staleAfter.Add(2 * time.Hour)
	upsertAgent(t, app.store, "alpha", recoveredStaleAfter, recoveredSeenAt)
	ev.now = func() time.Time { return recoveredSeenAt }
	ev.evaluate(ctx) // agent online (recovery)

	recoverID := finishScan(t, app.store, "repo", nil)
	ev.evaluate(ctx) // scan success (recovery)

	got := pendingAlertEvents(t, app.store)
	want := []capturedAlertEvent{
		{
			Kind: "agent.offline", Agent: "alpha", OldState: "online", NewState: "offline",
			Reason:    "agent report is stale",
			DedupeKey: "agent:alpha:agent.offline:" + staleAfter.UTC().Format(time.RFC3339Nano),
		},
		{
			Kind: "scan.failure", Repository: "repo", OldState: "ok", NewState: "error",
			Reason:    "clone failed: exit status 128",
			DedupeKey: fmt.Sprintf("repository:repo:scan.failure:%d", failID),
		},
		{
			Kind: "agent.recovery", Agent: "alpha", OldState: "offline", NewState: "online",
			Reason:    "agent report received",
			DedupeKey: "agent:alpha:agent.recovery:" + recoveredSeenAt.UTC().Format(time.RFC3339Nano),
		},
		{
			Kind: "scan.recovery", Repository: "repo", OldState: "error", NewState: "ok",
			Reason:    "repository scan recovered",
			DedupeKey: fmt.Sprintf("repository:repo:scan.recovery:%d", recoverID),
		},
	}
	if len(got) != 4 {
		t.Fatalf("events = %v (%d), want exactly 4", got, len(got))
	}
	for i, event := range want {
		if got[i] != event {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], event)
		}
	}

	// Restart limitation: persist another offline/error state, construct a
	// brand new evaluator over the same store (its baselines start empty,
	// as they would after a process restart), let it silently baseline the
	// already-failing state, then persist online/ok again. No restart
	// recovery rows may be added; the total must remain exactly 4.
	restartStaleAfter := recoveredStaleAfter.Add(time.Hour)
	upsertAgent(t, app.store, "alpha", restartStaleAfter, recoveredSeenAt)
	finishScan(t, app.store, "repo", errors.New("clone failed again"))

	restarted := newAlertEvaluator(app.cfg, app.store, app.logger)
	restarted.now = func() time.Time { return restartStaleAfter.Add(time.Minute) } // offline relative to restartStaleAfter
	restarted.evaluate(ctx)                                                        // silent startup baseline: offline + error

	if got := pendingAlertEvents(t, app.store); len(got) != 4 {
		t.Fatalf("events after restarted evaluator's silent baseline = %d, want still 4 (no events from baselining)", len(got))
	}

	postRestartStaleAfter := restartStaleAfter.Add(2 * time.Hour)
	postRestartSeenAt := restartStaleAfter.Add(time.Hour)
	upsertAgent(t, app.store, "alpha", postRestartStaleAfter, postRestartSeenAt)
	finishScan(t, app.store, "repo", nil)
	restarted.now = func() time.Time { return postRestartSeenAt }
	restarted.evaluate(ctx)

	final := pendingAlertEvents(t, app.store)
	if len(final) != 4 {
		t.Fatalf("events after post-restart online/ok = %v (%d), want still exactly 4 (restart baseline loss means no recovery is possible)", final, len(final))
	}
}
