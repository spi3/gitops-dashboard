package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
)

const (
	alertEvaluatorTimeout = 5 * time.Second

	agentOfflineReason       = "agent report is stale"
	agentRecoveredReason     = "agent report received"
	scanFailedFallbackReason = "repository scan failed"
	scanRecoveredReason      = "repository scan recovered"
)

// agentBaseline is the process-local state the evaluator tracks for one
// configured agent target across evaluations. offlineHandled is true only
// once an offline sample has been successfully handled by this process
// (enqueued without error): a startup baseline observed already-offline, or
// an offline sample whose enqueue failed, must not produce a recovery event
// once the agent reports again.
type agentBaseline struct {
	established    bool
	online         bool
	offlineHandled bool
}

// scanBaseline is the scan-side counterpart of agentBaseline.
type scanBaseline struct {
	established    bool
	ok             bool
	failureHandled bool
}

// alertEvaluator emits agent offline/recovered and repository scan
// failed/recovered alert events from periodic comparisons of already
// persisted state. It holds only process-local baselines and performs no
// cross-process coordination whatsoever, and baselines are lost on restart
// by design (T-064 replaces the cancelled T-023 exclusive-proof/write-epoch
// redesign).
type alertEvaluator struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger
	now    func() time.Time

	sinks    []string
	cooldown time.Duration

	agents map[string]*agentBaseline
	scans  map[string]*scanBaseline
}

func newAlertEvaluator(cfg config.Config, store *storage.Store, logger *slog.Logger) *alertEvaluator {
	if logger == nil {
		logger = slog.Default()
	}
	return &alertEvaluator{
		cfg:      cfg,
		store:    store,
		logger:   logger,
		now:      time.Now,
		sinks:    cfg.Alerting.ActiveSinkNames(),
		cooldown: mustAlertCooldown(cfg.Alerting),
		agents:   map[string]*agentBaseline{},
		scans:    map[string]*scanBaseline{},
	}
}

// evaluate runs one sequential agent-then-scan evaluation inside one shared
// five-second context. Both producers establish their persisted-state
// snapshot independently: an ordinary failed agent read does not prevent
// the scan producer from running with the remaining budget, and an ordinary
// failed scan read never rolls back agent progress already made. Every
// failure here is advisory -- it is logged (except plain parent
// cancellation, which exits quietly) and never blocks a later interval.
func (e *alertEvaluator) evaluate(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, alertEvaluatorTimeout)
	defer cancel()
	e.evaluateAgents(ctx)
	if ctx.Err() != nil {
		return
	}
	e.evaluateScans(ctx)
}

func (e *alertEvaluator) evaluateAgents(ctx context.Context) {
	targets := configuredAgentTargets(e.cfg.Runtime.Docker)
	if len(targets) == 0 || ctx.Err() != nil {
		return
	}
	agents, err := e.store.Agents(ctx)
	if err != nil {
		e.logAdvisory(err, "alert evaluator agent read failed")
		return
	}
	byTarget := make(map[string]core.AgentInfo, len(agents))
	for _, agent := range agents {
		byTarget[agent.Target] = agent
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		info, ok := byTarget[target]
		e.evaluateAgent(ctx, target, info, ok)
	}
}

func (e *alertEvaluator) evaluateAgent(ctx context.Context, target string, info core.AgentInfo, reported bool) {
	baseline := e.agents[target]
	if baseline == nil {
		baseline = &agentBaseline{}
		e.agents[target] = baseline
	}
	if !reported {
		e.logger.Debug("alert evaluator agent classification unknown", "target", target, "classification", "missing_row")
		return
	}
	staleAfter, err := time.Parse(time.RFC3339, info.StaleAfter)
	if err != nil {
		e.logger.Debug("alert evaluator agent classification unknown", "target", target, "classification", "malformed_stale_after")
		return
	}
	onlineNow := e.now().Before(staleAfter)

	if !baseline.established {
		baseline.established = true
		baseline.online = onlineNow
		return
	}
	if onlineNow == baseline.online {
		return
	}
	if !onlineNow {
		e.handleAgentOffline(ctx, target, baseline, staleAfter)
		return
	}
	e.handleAgentOnline(ctx, target, info, baseline)
}

func (e *alertEvaluator) handleAgentOffline(ctx context.Context, target string, baseline *agentBaseline, staleAfter time.Time) {
	event := storage.AlertEvent{
		Kind:        "agent_offline",
		Agent:       target,
		OldState:    "online",
		NewState:    "offline",
		Reason:      agentOfflineReason,
		DedupeKey:   "agent:" + target + ":agent_offline:" + staleAfter.UTC().Format(time.RFC3339Nano),
		CooldownKey: "agent:" + target + ":agent_offline",
	}
	if _, _, err := e.store.EnqueueAlertEvent(ctx, event, e.sinks, e.cooldown); err != nil {
		e.logAdvisory(err, "alert evaluator agent offline enqueue failed")
		return
	}
	baseline.online = false
	baseline.offlineHandled = true
}

func (e *alertEvaluator) handleAgentOnline(ctx context.Context, target string, info core.AgentInfo, baseline *agentBaseline) {
	if !baseline.offlineHandled {
		// The prior offline state was never a handled failure (either the
		// process's startup baseline, or a failure whose enqueue never
		// succeeded): recovery requires a handled failure edge, so this
		// transition is a silent baseline advance instead.
		baseline.online = true
		return
	}
	lastSeenAt, err := time.Parse(time.RFC3339, info.LastSeenAt)
	if err != nil {
		e.logger.Debug("alert evaluator agent classification unknown", "target", target, "classification", "malformed_last_seen_at")
		return
	}
	event := storage.AlertEvent{
		Kind:        "agent_recovered",
		Agent:       target,
		OldState:    "offline",
		NewState:    "online",
		Reason:      agentRecoveredReason,
		DedupeKey:   "agent:" + target + ":agent_recovered:" + lastSeenAt.UTC().Format(time.RFC3339Nano),
		CooldownKey: "agent:" + target + ":agent_recovered",
	}
	if _, _, err := e.store.EnqueueAlertEvent(ctx, event, e.sinks, e.cooldown); err != nil {
		e.logAdvisory(err, "alert evaluator agent recovered enqueue failed")
		return
	}
	baseline.online = true
	baseline.offlineHandled = false
}

func (e *alertEvaluator) evaluateScans(ctx context.Context) {
	names := configuredRepositoryNames(e.cfg.Repositories)
	if len(names) == 0 || ctx.Err() != nil {
		return
	}
	latest, err := e.store.LatestTerminalScansByRepository(ctx, names)
	if err != nil {
		e.logAdvisory(err, "alert evaluator scan read failed")
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		scan, ok := latest[name]
		e.evaluateScan(ctx, name, scan, ok)
	}
}

func (e *alertEvaluator) evaluateScan(ctx context.Context, name string, scan storage.LatestTerminalScan, hasTerminal bool) {
	baseline := e.scans[name]
	if baseline == nil {
		baseline = &scanBaseline{}
		e.scans[name] = baseline
	}
	if !hasTerminal {
		e.logger.Debug("alert evaluator scan classification unknown", "repository", name, "classification", "no_terminal_scan")
		return
	}
	currentOK := scan.Status == "ok"

	if !baseline.established {
		baseline.established = true
		baseline.ok = currentOK
		return
	}
	if currentOK == baseline.ok {
		return
	}
	if !currentOK {
		e.handleScanFailed(ctx, name, scan, baseline)
		return
	}
	e.handleScanRecovered(ctx, name, scan, baseline)
}

func (e *alertEvaluator) handleScanFailed(ctx context.Context, name string, scan storage.LatestTerminalScan, baseline *scanBaseline) {
	reason := scan.Error
	if reason == "" {
		reason = scanFailedFallbackReason
	}
	event := storage.AlertEvent{
		Kind:        "scan_failed",
		Repository:  name,
		OldState:    "ok",
		NewState:    "error",
		Reason:      reason,
		DedupeKey:   fmt.Sprintf("repository:%s:scan_failed:%d", name, scan.ID),
		CooldownKey: "repository:" + name + ":scan_failed",
	}
	if _, _, err := e.store.EnqueueAlertEvent(ctx, event, e.sinks, e.cooldown); err != nil {
		e.logAdvisory(err, "alert evaluator scan failed enqueue failed")
		return
	}
	baseline.ok = false
	baseline.failureHandled = true
}

func (e *alertEvaluator) handleScanRecovered(ctx context.Context, name string, scan storage.LatestTerminalScan, baseline *scanBaseline) {
	if !baseline.failureHandled {
		// Symmetric with the agent case: an unhandled prior failure (startup
		// baseline observed already-erroring, or a failed enqueue) means
		// this transition is a silent baseline advance, not a recovery.
		baseline.ok = true
		return
	}
	event := storage.AlertEvent{
		Kind:        "scan_recovered",
		Repository:  name,
		OldState:    "error",
		NewState:    "ok",
		Reason:      scanRecoveredReason,
		DedupeKey:   fmt.Sprintf("repository:%s:scan_recovered:%d", name, scan.ID),
		CooldownKey: "repository:" + name + ":scan_recovered",
	}
	if _, _, err := e.store.EnqueueAlertEvent(ctx, event, e.sinks, e.cooldown); err != nil {
		e.logAdvisory(err, "alert evaluator scan recovered enqueue failed")
		return
	}
	baseline.ok = true
	baseline.failureHandled = false
}

// logAdvisory logs a producer failure as advisory. Plain parent cancellation
// is expected during ordinary shutdown and exits quietly rather than
// logging.
func (e *alertEvaluator) logAdvisory(err error, message string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	e.logger.Warn(message, "error", err)
}

func configuredAgentTargets(docker []config.DockerTarget) []string {
	targets := make([]string, 0, len(docker))
	for _, target := range docker {
		if target.Kind != "agent" {
			continue
		}
		name := strings.TrimSpace(target.Name)
		if name == "" {
			continue
		}
		targets = append(targets, name)
	}
	return targets
}

func configuredRepositoryNames(repos []config.RepositoryConfig) []string {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		name := strings.TrimSpace(repo.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// runAlertEvaluatorLoop runs evaluate once immediately, then waits one full
// interval measured from each evaluation's completion before running it
// again, so evaluations never overlap. It returns as soon as ctx is done,
// whether that happens before, during, or between evaluations.
func runAlertEvaluatorLoop(ctx context.Context, interval time.Duration, evaluate func(context.Context)) {
	evaluate(ctx)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			evaluate(ctx)
			timer.Reset(interval)
		}
	}
}

// startAlertEvaluatorScheduler starts the single long-lived alert evaluator
// scheduler goroutine for this App.RunBackground lifecycle, when alerting is
// enabled and the configured default interval is usable. An invalid, zero,
// or negative interval disables only this evaluator: it logs one
// alerting-only error and returns without affecting the scanner, monitor,
// HTTP, or alert delivery behavior already started by RunBackground.
func (app *App) startAlertEvaluatorScheduler(ctx context.Context) {
	if !app.cfg.Alerting.Enabled() {
		return
	}
	interval, err := app.cfg.DefaultInterval()
	if err != nil {
		app.logger.Error("alert evaluator scheduler disabled", "error", err)
		return
	}
	if interval <= 0 {
		app.logger.Error("alert evaluator scheduler disabled", "error", fmt.Errorf("monitoring.defaultInterval must be greater than zero, got %s", interval))
		return
	}
	evaluate := app.alertEvaluate
	if evaluate == nil {
		evaluate = app.alertEvaluator.evaluate
	}
	go runAlertEvaluatorLoop(ctx, interval, evaluate)
}
