package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/alerter"
	"github.com/example/gitops-dashboard/internal/auth"
	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/monitor"
	"github.com/example/gitops-dashboard/internal/sanitizer"
	"github.com/example/gitops-dashboard/internal/scanner"
	"github.com/example/gitops-dashboard/internal/storage"
	"github.com/example/gitops-dashboard/internal/ui"
	"github.com/example/gitops-dashboard/internal/version"
	"github.com/gorilla/websocket"
)

type App struct {
	cfg       config.Config
	store     *storage.Store
	scanner   scanner.Scanner
	monitor   monitor.Monitor
	alerter   *alerter.Worker
	auth      auth.BasicAuth
	agentAuth auth.AgentTokenAuthenticator
	logger    *slog.Logger
	upgrader  websocket.Upgrader
	scanAll   func(context.Context) error
	checkAll  func(context.Context) error

	actionCtx     context.Context
	actionCancel  context.CancelFunc
	actionsWG     sync.WaitGroup
	actionsMu     sync.Mutex
	actions       map[string]dashboardAction
	activeActions map[string]string
	nextActionID  int64

	readinessMu    sync.Mutex
	readinessCache readinessStatus
	readinessTTL   time.Duration
	readinessNow   func() time.Time
	readinessProbe func(context.Context) error
}

const (
	agentTokenHeader           = "X-Agent-Token"
	agentTokenQuery            = "token"
	stateChangingRequestHeader = "X-GitOps-Dashboard-CSRF"
	readinessTimeout           = 2 * time.Second
	readinessCacheTTL          = 30 * time.Second
	readinessJSONSampleLimit   = 5
)

var (
	agentWSReadLimit  int64 = 1 << 20
	agentWSPongWait         = 60 * time.Second
	agentWSPingPeriod       = 54 * time.Second
	agentWSWriteWait        = 10 * time.Second
)

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	dbPath := filepath.Join(cfg.Server.DataDir, "gitops-dashboard.db")
	redactionValues := append(repositoryRedactionValues(cfg.Repositories), config.AlertingRedactionValues(cfg.Alerting)...)
	store, err := storage.OpenWithOptions(dbPath, storage.OpenOptions{
		Logger:                      logger,
		RedactionValues:             redactionValues,
		ResetAlertStateOnMissingKey: cfg.Alerting.ResetOnMissingKey,
		ResetAlertStateToken:        cfg.Alerting.ResetToken,
		AlertSinkNames:              cfg.Alerting.EnabledSinkNames(),
		AlertSinkAllowlist:          true,
		HealthAlerts: storage.HealthAlertProducerConfig{
			Enabled: cfg.Alerting.Enabled(), Sinks: cfg.Alerting.ActiveSinkNames(),
			Debounce: mustAlertDebounce(cfg.Alerting), Cooldown: mustAlertCooldown(cfg.Alerting), StabilitySamples: cfg.Alerting.StabilitySamples,
		},
	})
	if err != nil {
		return nil, err
	}
	store.AddRedactionValues(redactionValues...)
	if err := store.CanonicalizeHTTPRouteTargets(context.Background(), cfg.Runtime.HTTP); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.EnsureRepositories(context.Background(), cfg.Repositories); err != nil {
		_ = store.Close()
		return nil, err
	}
	actionCtx, actionCancel := context.WithCancel(context.Background())
	app := &App{
		cfg:           cfg,
		store:         store,
		auth:          auth.New(cfg.Auth),
		agentAuth:     auth.NewAgentTokenAuthenticator(cfg),
		logger:        logger,
		actionCtx:     actionCtx,
		actionCancel:  actionCancel,
		actions:       map[string]dashboardAction{},
		activeActions: map[string]string{},
	}
	app.upgrader.CheckOrigin = app.checkAgentOrigin
	app.scanner = scanner.New(cfg, store, logger)
	app.monitor = monitor.New(cfg, store, logger)
	if err := app.monitor.SyncPingTargets(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	app.scanAll = app.scanner.ScanAll
	app.checkAll = app.monitor.CheckAll
	app.readinessTTL = readinessCacheTTL
	app.readinessNow = time.Now
	app.readinessProbe = app.storageReadinessProbe
	alerterWorker, err := alerter.New(cfg.Alerting, store, logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("alerter: %w", err)
	}
	app.alerter = alerterWorker
	return app, nil
}

func mustAlertCooldown(alerting config.AlertingConfig) time.Duration {
	cooldown, err := alerting.CooldownDuration()
	if err != nil {
		return 0
	}
	return cooldown
}

func mustAlertDebounce(alerting config.AlertingConfig) time.Duration {
	debounce, err := alerting.DebounceDuration()
	if err != nil {
		return 0
	}
	return debounce
}

func repositoryRedactionValues(repos []config.RepositoryConfig) []string {
	values := []string{}
	for _, repo := range repos {
		values = append(values, sanitizer.URLUserinfoValues(repo.URL)...)
		token, err := repo.Token()
		if err == nil && token != "" {
			values = append(values, token)
		}
	}
	return values
}

func (app *App) Close() {
	if app.actionCancel != nil {
		app.actionCancel()
	}
	app.actionsWG.Wait()
	if app.store != nil {
		_ = app.store.Close()
	}
}

func (app *App) RunBackground(ctx context.Context) {
	app.scanner.RunScheduled(ctx)
	app.monitor.Run(ctx)
	app.alerter.Run(ctx)
}

func (app *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /readyz", app.ready)
	mux.HandleFunc("GET /api/version", app.version)
	mux.HandleFunc("GET /api/summary", app.summary)
	mux.HandleFunc("POST /api/scan", app.requireStateChangingRequest(app.scan))
	mux.HandleFunc("POST /api/monitor", app.requireStateChangingRequest(app.checkMonitor))
	mux.HandleFunc("GET /api/actions/{id}", app.actionStatus)
	mux.HandleFunc("POST /api/monitor-overrides", app.requireStateChangingRequest(app.monitorOverride))
	mux.HandleFunc("GET /api/agents/connect", app.agentConnect)
	mux.Handle("GET /", app.staticHandler())
	return app.auth.Middleware(mux)
}

func (app *App) requireStateChangingRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get(stateChangingRequestHeader)) == "" {
			http.Error(w, "state-changing request header required", http.StatusForbidden)
			return
		}
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			app.logger.Warn("state-changing request fetch metadata rejected", "site", r.Header.Get("Sec-Fetch-Site"), "path", r.URL.Path)
			http.Error(w, "state-changing request fetch metadata rejected", http.StatusForbidden)
			return
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" {
			parsed, ok := parseHTTPOrigin(origin)
			if !ok || !app.stateChangingOriginAllowed(parsed, r.Host) {
				app.logger.Warn("state-changing request origin rejected", "origin", origin, "host", r.Host, "path", r.URL.Path)
				http.Error(w, "state-changing request origin rejected", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func (app *App) stateChangingOriginAllowed(origin *url.URL, requestHost string) bool {
	if originMatchesRequestHost(origin, requestHost) {
		return true
	}
	for _, allowedOrigin := range app.cfg.Server.AllowedOrigins {
		allowed, ok := parseHTTPOrigin(allowedOrigin)
		if !ok {
			continue
		}
		if sameOrigin(origin, allowed) {
			return true
		}
	}
	return false
}

func (app *App) health(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

func (app *App) ready(w http.ResponseWriter, _ *http.Request) {
	status := app.cachedReadiness()
	if status.err != nil {
		http.Error(w, "not ready: storage", http.StatusServiceUnavailable)
		return
	}
	if len(status.warnings) > 0 {
		_, _ = w.Write([]byte("ready: startup storage warnings present\n"))
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

type readinessStatus struct {
	checkedAt time.Time
	err       error
	warnings  []string
}

func (app *App) cachedReadiness() readinessStatus {
	now := app.now()
	status := readinessStatus{
		checkedAt: now,
		warnings:  app.store.StartupWarnings(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), readinessTimeout)
	defer cancel()
	if err := app.store.Ping(ctx); err != nil {
		status.err = err
		app.logger.Warn("readiness check failed", "error", err)
		return status
	}
	if err := app.store.CheckPersistedJSONFailures(ctx); err != nil {
		status.err = fmt.Errorf("storage decode registry: %w", err)
		app.logger.Warn("readiness storage decode registry failed", "error", status.err)
		return status
	}

	app.readinessMu.Lock()
	defer app.readinessMu.Unlock()

	if !app.readinessCache.checkedAt.IsZero() && app.readinessTTL > 0 && now.Sub(app.readinessCache.checkedAt) < app.readinessTTL {
		cached := app.readinessCache
		cached.warnings = status.warnings
		return cached
	}

	probe := app.readinessProbe
	if probe == nil {
		probe = app.storageReadinessProbe
	}
	if err := probe(ctx); err != nil {
		status.err = fmt.Errorf("storage decode probe: %w", err)
		app.logger.Warn("readiness storage decode probe failed", "error", status.err)
		return status
	}
	app.readinessCache = status
	return status
}

func transientReadinessError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (app *App) now() time.Time {
	if app.readinessNow == nil {
		return time.Now()
	}
	return app.readinessNow()
}

func (app *App) storageReadinessProbe(ctx context.Context) error {
	return app.store.ProbePersistedJSON(ctx, readinessJSONSampleLimit)
}

func (app *App) version(w http.ResponseWriter, _ *http.Request) {
	app.writeJSON(w, version.Current())
}

func (app *App) summary(w http.ResponseWriter, r *http.Request) {
	summary, err := app.store.Summary(r.Context())
	if err != nil {
		app.logger.Error("summary unavailable", "error", err)
		http.Error(w, "dashboard storage degraded: "+err.Error(), http.StatusInternalServerError)
		return
	}
	agents, err := app.store.Agents(r.Context())
	if err != nil {
		app.logger.Error("agents unavailable", "error", err)
		http.Error(w, "dashboard storage degraded: "+err.Error(), http.StatusInternalServerError)
		return
	}
	summary.Agents = mergeAgents(agents, app.cfg.Runtime.Docker)
	summary.Version = version.Current()
	app.writeJSON(w, summary)
}

// mergeAgents combines reported agents with configured agent targets: reported
// agents are marked Configured if their target matches a configured entry, and
// configured targets that have never reported are appended with an empty
// LastSeenAt/Containers. The result is sorted by target and always non-nil.
func mergeAgents(reported []core.AgentInfo, docker []config.DockerTarget) []core.AgentInfo {
	configuredTargets := map[string]struct{}{}
	for _, target := range docker {
		if target.Kind != "agent" {
			continue
		}
		configuredTargets[target.Name] = struct{}{}
	}
	seen := map[string]struct{}{}
	merged := make([]core.AgentInfo, 0, len(reported)+len(configuredTargets))
	for _, agent := range reported {
		_, agent.Configured = configuredTargets[agent.Target]
		if agent.Containers == nil {
			agent.Containers = []core.ContainerStatus{}
		}
		merged = append(merged, agent)
		seen[agent.Target] = struct{}{}
	}
	for target := range configuredTargets {
		if _, ok := seen[target]; ok {
			continue
		}
		merged = append(merged, core.AgentInfo{
			Target:     target,
			LastSeenAt: "",
			Configured: true,
			Containers: []core.ContainerStatus{},
		})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Target < merged[j].Target })
	return merged
}

func (app *App) scan(w http.ResponseWriter, r *http.Request) {
	scanAll := app.scanAll
	if scanAll == nil {
		scanAll = app.scanner.ScanAll
	}
	action := app.startAction("scan", scanAll)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	app.writeJSON(w, action)
}

func (app *App) checkMonitor(w http.ResponseWriter, r *http.Request) {
	checkAll := app.checkAll
	if checkAll == nil {
		checkAll = app.monitor.CheckAll
	}
	action := app.startAction("monitor", checkAll)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	app.writeJSON(w, action)
}

type monitorOverrideRequest struct {
	ServiceID     string `json:"serviceId"`
	Target        string `json:"target"`
	NotApplicable bool   `json:"notApplicable"`
}

func (app *App) monitorOverride(w http.ResponseWriter, r *http.Request) {
	var request monitorOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if request.ServiceID == "" || request.Target == "" {
		http.Error(w, "serviceId and target are required", http.StatusBadRequest)
		return
	}
	if err := app.store.SetMonitorNotApplicable(r.Context(), request.ServiceID, request.Target, request.NotApplicable); err != nil {
		if errors.Is(err, storage.ErrStatusNotFound) {
			http.Error(w, "monitor target not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app.writeJSON(w, map[string]string{"status": "ok"})
}

func (app *App) agentConnect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has(agentTokenQuery) {
		app.logger.Warn("agent authentication rejected query-string token")
		http.Error(w, "agent token must be supplied in X-Agent-Token", http.StatusUnauthorized)
		return
	}
	binding, ok := app.agentAuth.Authenticate(r.Header.Get(agentTokenHeader))
	if !ok {
		http.Error(w, "agent authentication failed", http.StatusUnauthorized)
		return
	}
	conn, err := app.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(agentWSReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(agentWSPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(agentWSPongWait))
	})
	stopPings := make(chan struct{})
	defer close(stopPings)
	go pingAgentConnection(conn, stopPings)

	authorizedTargets := binding.Targets()
	for {
		var message core.AgentMessage
		if err := conn.ReadJSON(&message); err != nil {
			app.logger.Info("agent disconnected", "error", err)
			return
		}
		if err := app.monitor.ApplyAgentReport(r.Context(), message, authorizedTargets); err != nil {
			if errors.Is(err, monitor.ErrAgentTargetUnauthorized) {
				app.logger.Warn("agent report rejected unauthorized target", "target", message.Target, "authorizedTargets", authorizedTargets)
				closeAgentConnection(conn, websocket.ClosePolicyViolation, "agent target is not authorized")
				return
			}
			app.logger.Error("agent status persist failed", "error", err)
			return
		}
	}
}

func pingAgentConnection(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(agentWSPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(agentWSWriteWait)); err != nil {
				return
			}
		}
	}
}

func closeAgentConnection(conn *websocket.Conn, code int, text string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(agentWSWriteWait))
}

func (app *App) checkAgentOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, ok := parseHTTPOrigin(origin)
	if !ok {
		app.logger.Warn("agent websocket origin rejected", "origin", origin, "host", r.Host)
		return false
	}
	if originMatchesRequestHost(parsed, r.Host) {
		return true
	}
	for _, allowedOrigin := range app.cfg.Auth.Agent.AllowedOrigins {
		allowed, ok := parseHTTPOrigin(allowedOrigin)
		if !ok {
			continue
		}
		if sameOrigin(parsed, allowed) {
			return true
		}
	}
	app.logger.Warn("agent websocket origin rejected", "origin", origin, "host", r.Host)
	return false
}

func parseHTTPOrigin(value string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" {
		return nil, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, false
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false
	}
	return parsed, true
}

func originMatchesRequestHost(origin *url.URL, requestHost string) bool {
	originHost := canonicalOriginHost(origin.Scheme, origin.Host)
	return originHost == canonicalOriginHost("http", requestHost) || originHost == canonicalOriginHost("https", requestHost)
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && canonicalOriginHost(left.Scheme, left.Host) == canonicalOriginHost(right.Scheme, right.Host)
}

func canonicalOriginHost(scheme, host string) string {
	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		hostname = strings.Trim(host, "[]")
		port = ""
	}
	hostname = strings.ToLower(strings.Trim(hostname, "[]"))
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return hostname
	}
	return hostname + ":" + port
}

func (app *App) staticHandler() http.Handler {
	dist, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			index, err := fs.ReadFile(dist, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func (app *App) writeJSON(w http.ResponseWriter, value any) {
	writeJSON(app.logger, w, value)
}

func writeJSON(logger *slog.Logger, w http.ResponseWriter, value any) {
	if logger == nil {
		logger = slog.Default()
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		logger.Error("write JSON response failed", "error", err)
	}
}
