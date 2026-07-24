package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
)

func TestMergeAgentsCombinesReportedAndConfigured(t *testing.T) {
	t.Parallel()
	reported := []core.AgentInfo{
		{Target: "serenity", LastSeenAt: "2026-07-07T16:00:00Z", Containers: []core.ContainerStatus{{Name: "web"}}},
		{Target: "unmanaged", LastSeenAt: "2026-07-07T16:05:00Z", Containers: []core.ContainerStatus{}},
	}
	docker := []config.DockerTarget{
		{Name: "serenity", Kind: "agent"},
		{Name: "albert", Kind: "agent"},
		{Name: "local", Kind: "socket"},
	}
	merged := mergeAgents(reported, docker)
	if len(merged) != 3 {
		t.Fatalf("merged = %#v, want 3 entries", merged)
	}
	byTarget := map[string]core.AgentInfo{}
	for _, agent := range merged {
		byTarget[agent.Target] = agent
	}
	if !byTarget["serenity"].Configured || byTarget["serenity"].LastSeenAt != "2026-07-07T16:00:00Z" {
		t.Fatalf("serenity = %#v, want configured with reported lastSeenAt", byTarget["serenity"])
	}
	if len(byTarget["serenity"].Containers) != 1 {
		t.Fatalf("serenity containers = %#v", byTarget["serenity"].Containers)
	}
	if byTarget["unmanaged"].Configured {
		t.Fatalf("unmanaged = %#v, want unconfigured", byTarget["unmanaged"])
	}
	albert, ok := byTarget["albert"]
	if !ok || !albert.Configured || albert.LastSeenAt != "" {
		t.Fatalf("albert = %#v, want configured-never-reported", albert)
	}
	if albert.Containers == nil || len(albert.Containers) != 0 {
		t.Fatalf("albert containers = %#v, want empty non-nil", albert.Containers)
	}
	for i := 1; i < len(merged); i++ {
		if merged[i-1].Target >= merged[i].Target {
			t.Fatalf("merged not sorted by target: %#v", merged)
		}
	}
}

func TestMergeAgentsAlwaysNonNil(t *testing.T) {
	t.Parallel()
	merged := mergeAgents(nil, nil)
	if merged == nil {
		t.Fatal("merged is nil, want empty slice")
	}
	if len(merged) != 0 {
		t.Fatalf("merged = %#v, want empty", merged)
	}
}

func TestHandlerServesSummaryAndFrontend(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	scanID, err := app.store.StartScan(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.FinishScan(context.Background(), scanID, "repo", "abc123", []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Environment:  "production",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("summary status = %d", res.Code)
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("version status = %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"version"`) || !strings.Contains(res.Body.String(), `"commit"`) || !strings.Contains(res.Body.String(), `"buildDate"`) {
		t.Fatalf("version body = %q, want version metadata fields", res.Body.String())
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("frontend status = %d", res.Code)
	}
}

func TestNewRegistersAlertingSecretsForStorageRedaction(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Alerting: config.AlertingConfig{
			Sinks: config.AlertingSinksConfig{
				Webhook: config.WebhookAlertSinkConfig{
					URL:          "https://hooks.example.test/api/services/webhook-secret?token=123456&id=services",
					RedactValues: []string{"abc123", "x6", "webhook-secret"},
					Headers: map[string]string{
						"Authorization": "Bearer 654321",
						"Content-Type":  "application/json",
					},
				},
				Discord: config.DiscordAlertSinkConfig{
					WebhookURL: "https://discord.example.test/api/webhooks/123/discord-secret-42",
				},
				HomeAssistant: config.HomeAssistantAlertSinkConfig{
					Token:     "ha-token-secret",
					WebhookID: "ha-webhook-secret",
				},
			},
		},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	event, _, err := app.store.EnqueueAlertEvent(context.Background(), storage.AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "failed webhook-secret abc123 x6 123456 654321 discord-secret-42 ha-token-secret ha-webhook-secret id services application/json",
		DedupeKey: "svc:webhook-secret:abc123:x6",
	}, []string{"webhook"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := app.store.ClaimPendingAlertDeliveries(context.Background(), "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	if _, err := app.store.RecordAlertDispatchResult(context.Background(), claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, storage.AlertDispatchStatusDeadLettered, "abc123 x6 123456 654321 id services application/json"); err != nil {
		t.Fatal(err)
	}
	var reason, dedupeKey, lastError string
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(context.Background(), `SELECT reason, dedupe_key FROM alert_events WHERE id=?`, event.ID).Scan(&reason, &dedupeKey); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT last_error FROM alert_dispatches WHERE event_id=?`, event.ID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"webhook-secret", "abc123", "x6", "123456", "654321", "discord-secret-42", "ha-token-secret", "ha-webhook-secret"} {
		if strings.Contains(reason, secret) || strings.Contains(dedupeKey, secret) || strings.Contains(lastError, secret) {
			t.Fatalf("alert event persisted secret %q: reason=%q dedupe=%q last_error=%q", secret, reason, dedupeKey, lastError)
		}
	}
	for _, nonSecret := range []string{"id", "services", "application/json"} {
		if !strings.Contains(reason, nonSecret) || !strings.Contains(lastError, nonSecret) {
			t.Fatalf("non-secret %q was redacted unexpectedly: reason=%q last_error=%q", nonSecret, reason, lastError)
		}
	}
}

func TestNewDoesNotRegisterBareAlertURLUsernameForGlobalRedaction(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Alerting: config.AlertingConfig{
			Sinks: config.AlertingSinksConfig{
				Webhook: config.WebhookAlertSinkConfig{
					URL: "https://git@example.test/hooks",
				},
			},
		},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	event, _, err := app.store.EnqueueAlertEvent(context.Background(), storage.AlertEvent{
		Kind:      "health_transition",
		ServiceID: "github.com-service",
		NewState:  "unhealthy",
		Reason:    "github.com repository identity preserved",
		DedupeKey: "github.com:identity",
	}, []string{"webhook"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := app.store.ClaimPendingAlertDeliveries(context.Background(), "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	if _, err := app.store.RecordAlertDispatchResult(context.Background(), claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, storage.AlertDispatchStatusDeadLettered, "github.com failed"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var serviceID, reason, dedupeKey, lastError string
	if err := db.QueryRowContext(context.Background(), `SELECT service_id, reason, dedupe_key FROM alert_events WHERE id=?`, event.ID).Scan(&serviceID, &reason, &dedupeKey); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT last_error FROM alert_dispatches WHERE event_id=?`, event.ID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]string{"service_id": serviceID, "reason": reason, "dedupe_key": dedupeKey, "last_error": lastError} {
		if !strings.Contains(value, "github.com") || strings.Contains(value, "[REDACTED]hub.com") {
			t.Fatalf("%s = %q, want bare URL username not globally redacted", field, value)
		}
	}
}

func TestNewRegistersRawEscapedAlertURLSecretsForStorageRedaction(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	pathToken := "s%65cretTok%65n42"
	queryToken := "q%75eryTok%65n42"
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Alerting: config.AlertingConfig{
			Sinks: config.AlertingSinksConfig{
				Webhook: config.WebhookAlertSinkConfig{
					URL: "https://hooks.example.test/api/" + pathToken + "?token=" + queryToken,
					// Path-embedded secrets are no longer auto-detected (see
					// T-024 requirement 11): the decoded value must be
					// declared explicitly. The query token is still covered
					// automatically because "token" is a recognized secret
					// parameter name.
					RedactValues: []string{"secretToken42"},
				},
			},
		},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	event, _, err := app.store.EnqueueAlertEvent(context.Background(), storage.AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "failed " + pathToken + " secretToken42 " + queryToken + " queryToken42",
		DedupeKey: "health:svc:" + pathToken + ":" + queryToken,
	}, []string{"webhook"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := app.store.ClaimPendingAlertDeliveries(context.Background(), "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	if _, err := app.store.RecordAlertDispatchResult(context.Background(), claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, storage.AlertDispatchStatusDeadLettered, "failed "+pathToken+" "+queryToken); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reason, dedupeKey, lastError string
	if err := db.QueryRowContext(context.Background(), `SELECT reason, dedupe_key FROM alert_events WHERE id=?`, event.ID).Scan(&reason, &dedupeKey); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT last_error FROM alert_dispatches WHERE event_id=?`, event.ID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{pathToken, "secretToken42", queryToken, "queryToken42"} {
		if strings.Contains(reason, secret) || strings.Contains(dedupeKey, secret) || strings.Contains(lastError, secret) {
			t.Fatalf("persisted alert text contains escaped secret %q: reason=%q dedupe=%q last_error=%q", secret, reason, dedupeKey, lastError)
		}
	}
}

func TestNewRedactsAlertSecretsBeforeCanonicalizeFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "gitops-dashboard.db")
	secret := "ha-token-secret"
	seed, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	event, inserted, err := seed.EnqueueAlertEvent(ctx, storage.AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "home assistant token " + secret,
		DedupeKey: "health:svc:" + secret,
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"home-assistant"}, time.Hour)
	if err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = seed.Close()
		t.Fatal("inserted = false, want seed alert event")
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
DROP TABLE monitor_overrides;
CREATE TABLE monitor_overrides (id TEXT NOT NULL);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Alerting: config.AlertingConfig{
			Sinks: config.AlertingSinksConfig{
				HomeAssistant: config.HomeAssistantAlertSinkConfig{
					Token:     secret,
					WebhookID: "gitops-alert",
				},
			},
		},
	}
	app, err := New(cfg, slog.Default())
	if err == nil {
		app.Close()
		t.Fatal("New succeeded, want injected canonicalize failure")
	}

	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reason, dedupeKey string
	if err := db.QueryRowContext(ctx, `SELECT reason, dedupe_key FROM alert_events WHERE id=?`, event.ID).Scan(&reason, &dedupeKey); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reason, secret) || strings.Contains(dedupeKey, secret) {
		t.Fatalf("startup failure left alert secret unredacted: reason=%q dedupe=%q", reason, dedupeKey)
	}
}

func TestNewRedactsDisabledAlertingSinkSecrets(t *testing.T) {
	t.Setenv("GITOPS_DISABLED_HA_TOKEN", "disabled-ha-env-token")
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "gitops-dashboard.db")
	seed, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	event, inserted, err := seed.EnqueueAlertEvent(ctx, storage.AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "disabled sink tokens disabled-ha-literal-token disabled-ha-env-token",
		DedupeKey: "health:svc:disabled-ha-literal-token:disabled-ha-env-token",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"home-assistant"}, time.Hour)
	if err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = seed.Close()
		t.Fatal("inserted = false, want seed alert event")
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
server:
  dataDir: %q
  repoCacheDir: %q
auth:
  mode: dev-no-auth
alerting:
  sinks:
    homeAssistant:
      enabled: false
      token: disabled-ha-literal-token
      tokenEnv: GITOPS_DISABLED_HA_TOKEN
`, dataDir, filepath.Join(t.TempDir(), "repos"))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reason, dedupeKey string
	if err := db.QueryRowContext(ctx, `SELECT reason, dedupe_key FROM alert_events WHERE id=?`, event.ID).Scan(&reason, &dedupeKey); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"disabled-ha-literal-token", "disabled-ha-env-token"} {
		if strings.Contains(reason, secret) || strings.Contains(dedupeKey, secret) {
			t.Fatalf("disabled sink token %q remained in persisted alert text: reason=%q dedupe=%q", secret, reason, dedupeKey)
		}
	}
}

func TestNewEnablesAlerterWorkerWhenASinkIsConfigured(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Alerting: config.AlertingConfig{
			Sinks: config.AlertingSinksConfig{
				Discord: config.DiscordAlertSinkConfig{
					Enabled:    true,
					Timeout:    "10s",
					WebhookURL: "https://discord.example.test/api/webhooks/123/token",
				},
			},
		},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.alerter == nil || !app.alerter.Enabled() {
		t.Fatalf("alerter = %#v, want an enabled worker when a sink is configured", app.alerter)
	}
}

func TestNewLeavesAlerterWorkerDisabledWithoutSinks(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.alerter == nil || app.alerter.Enabled() {
		t.Fatalf("alerter = %#v, want a disabled worker when no sink is configured", app.alerter)
	}
}

func TestAppDefersRepositoryTokenResolutionUntilScanner(t *testing.T) {
	t.Parallel()
	os.Unsetenv("GITOPS_DASHBOARD_T060_APP_MISSING_TOKEN")
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Repositories: []config.RepositoryConfig{
			{
				Name:      "unreadable-token-file",
				URL:       "https://example.test/private/repo.git",
				TokenFile: filepath.Join(t.TempDir(), "missing-token-file-t060"),
			},
			{
				Name:     "unset-token-env",
				URL:      "https://example.test/other/repo.git",
				TokenEnv: "GITOPS_DASHBOARD_T060_APP_MISSING_TOKEN",
			},
		},
	}
	// App.New must not call RepositoryConfig.Token() (which would read the
	// missing token file or resolve the unset env) or otherwise resolve
	// repository credentials; those checks belong to the scanner, deferred
	// until after any existing repository cache has been scrubbed.
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatalf("New returned error, want repository token resolution deferred to the scanner: %v", err)
	}
	defer app.Close()
}

func TestReadyzReportsClosedDatabase(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	handler := app.Handler()

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", res.Code)
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503; body=%q", res.Code, res.Body.String())
	}
	if body := res.Body.String(); !strings.Contains(body, "not ready: storage") {
		t.Fatalf("readyz body = %q, want generic storage readiness reason", body)
	} else if strings.Contains(body, "sqlite ping") || strings.Contains(body, "database is closed") {
		t.Fatalf("readyz body = %q, want no storage internals", body)
	}
}

func TestReadyzReportsStorageDecodeFailure(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ctx := context.Background()
	if err := app.store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := app.store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.FinishScan(ctx, scanID, "repo", "abc123", []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", "svc"); err != nil {
		t.Fatal(err)
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", res.Code)
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503; body=%q", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if !strings.Contains(body, "not ready: storage") {
		t.Fatalf("readyz body = %q, want generic storage failure", body)
	}
	for _, leaked := range []string{"table=services", "key=svc", "column=images_json"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("readyz body leaked %q: %q", leaked, body)
		}
	}
	for _, want := range []string{"storage decode probe", "table=services", "key=svc", "column=images_json"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("readiness logs = %q, want %q", logs.String(), want)
		}
	}
}

func TestReadyzRejectsEmptyPersistedJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ctx := context.Background()
	if err := app.store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := app.store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.FinishScan(ctx, scanID, "repo", "abc123", []core.Service{{
		ID:           "svc-empty",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "   ", "svc-empty"); err != nil {
		t.Fatal(err)
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty-json readyz status = %d, want 503; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "not ready: storage") {
		t.Fatalf("empty-json readyz body = %q, want generic storage failure", res.Body.String())
	}

	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, `["example/api:v2"]`, "svc-empty"); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healed empty-json readyz status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestReadyzUsesDecodeFailureRegistryPastSample(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ctx := context.Background()
	if err := app.store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := app.store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	services := make([]core.Service, 0, readinessJSONSampleLimit+1)
	for i := 0; i < readinessJSONSampleLimit+1; i++ {
		id := fmt.Sprintf("svc-%02d", i)
		services = append(services, core.Service{
			ID:           id,
			Name:         id,
			Repository:   "repo",
			SourceCommit: "abc123",
			SourcePath:   "prod/compose.yaml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/" + id + ":v1"},
		})
	}
	if err := app.store.FinishScan(ctx, scanID, "repo", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	corruptID := fmt.Sprintf("svc-%02d", readinessJSONSampleLimit)
	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", corruptID); err != nil {
		t.Fatal(err)
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("initial readyz status = %d, want 200 with corrupt row past first sample; body=%q", res.Code, res.Body.String())
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("summary status = %d, want strict decode 500; body=%q", res.Code, res.Body.String())
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("registry readyz status = %d, want 503 despite cached sample success; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "not ready: storage") {
		t.Fatalf("registry readyz body = %q, want generic storage failure", res.Body.String())
	}

	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, `["example/svc-05:v2"]`, corruptID); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healed readyz status = %d, want immediate recovery; body=%q", res.Code, res.Body.String())
	}
}

func TestReadyzUsesStartupDecodeFailureRegistryPastSample(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "gitops-dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	services := make([]core.Service, 0, readinessJSONSampleLimit+1)
	for i := 0; i < readinessJSONSampleLimit+1; i++ {
		id := fmt.Sprintf("boot-svc-%02d", i)
		services = append(services, core.Service{
			ID:           id,
			Name:         id,
			Repository:   "repo",
			SourceCommit: "abc123",
			SourcePath:   "prod/compose.yaml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/" + id + ":v1"},
		})
	}
	if err := store.FinishScan(ctx, scanID, "repo", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DELETE FROM services WHERE id=?`, "boot-svc-00"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE services SET exposure_json=? WHERE id=?`, `["https://user:pass@app.example.test"]`, "boot-svc-01"); err != nil {
		t.Fatal(err)
	}
	corruptID := fmt.Sprintf("boot-svc-%02d", readinessJSONSampleLimit)
	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", corruptID); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	handler := app.Handler()

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("startup registry readyz status = %d, want 503; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "not ready: storage") {
		t.Fatalf("startup registry readyz body = %q, want generic storage failure", res.Body.String())
	}

	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, `["example/boot-svc:v2"]`, corruptID); err != nil {
		t.Fatal(err)
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healed startup registry readyz status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
}

func TestReadyzUsesLiveProbeWithAdvisoryStartupWarnings(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "gitops-dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "abc123", []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", "svc"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	now := time.Date(2026, 7, 9, 8, 0, 0, 0, time.UTC)
	app.readinessNow = func() time.Time { return now }

	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt readyz status = %d, want 503; body=%q", res.Code, res.Body.String())
	}
	if body := res.Body.String(); !strings.Contains(body, "not ready: storage") {
		t.Fatalf("corrupt readyz body = %q, want generic storage failure", body)
	}
	for _, leaked := range []string{"storage decode probe", "column=images_json", "services.images_json", "key=svc"} {
		if strings.Contains(res.Body.String(), leaked) {
			t.Fatalf("corrupt readyz body leaked %q: %q", leaked, res.Body.String())
		}
	}

	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `UPDATE services SET images_json=? WHERE id=?`, `["example/api:v2"]`, "svc"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(readinessCacheTTL + time.Second)

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("healed readyz status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	for _, want := range []string{"ready:", "startup storage warnings present"} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("healed readyz body = %q, want %q", res.Body.String(), want)
		}
	}
	if strings.Contains(res.Body.String(), "services.images_json") {
		t.Fatalf("healed readyz body exposed startup warning details: %q", res.Body.String())
	}
}

func TestReadyzAdvisesWhenAlertStateLocked(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	keyPath := filepath.Join(dataDir, "alert-dedupe.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	warnings := strings.Join(app.store.StartupWarnings(), "\n")
	if !strings.Contains(warnings, "alert state locked") || !strings.Contains(warnings, "dedupe key unavailable") {
		t.Fatalf("startup warnings = %q, want alert locked detail", warnings)
	}
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200 advisory; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "startup storage warnings present") {
		t.Fatalf("readyz body = %q, want startup warning advisory", res.Body.String())
	}
}

func TestReadyzCachesDecodedStorageProbe(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	probeCalls := 0
	app.readinessProbe = func(context.Context) error {
		probeCalls++
		return nil
	}

	handler := app.Handler()
	for i := 0; i < 2; i++ {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if res.Code != http.StatusOK {
			t.Fatalf("readyz[%d] status = %d, want 200; body=%q", i, res.Code, res.Body.String())
		}
		if res.Body.String() != "ready\n" {
			t.Fatalf("readyz[%d] body = %q, want ready", i, res.Body.String())
		}
	}
	if probeCalls != 1 {
		t.Fatalf("readiness probe calls = %d, want 1 cached decoded storage probe", probeCalls)
	}
}

func TestReadyzPingsStorageBeforeReturningCachedDecodeProbe(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	probeCalls := 0
	app.readinessProbe = func(context.Context) error {
		probeCalls++
		return nil
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("first readyz status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	if probeCalls != 1 {
		t.Fatalf("readiness probe calls = %d, want initial decode probe", probeCalls)
	}
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed-db readyz status = %d, want 503 despite cached decode probe; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "not ready: storage") {
		t.Fatalf("closed-db readyz body = %q, want generic storage failure", res.Body.String())
	}
	if probeCalls != 1 {
		t.Fatalf("readiness probe calls = %d, want cached decode probe not rerun", probeCalls)
	}
}

func TestReadyzDoesNotCacheCanceledProbe(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	probeCalls := 0
	app.readinessProbe = func(context.Context) error {
		probeCalls++
		if probeCalls == 1 {
			return context.Canceled
		}
		return nil
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("first readyz status = %d, want 503; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "not ready: storage") {
		t.Fatalf("first readyz body = %q, want generic storage failure", res.Body.String())
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("second readyz status = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	if probeCalls != 2 {
		t.Fatalf("readiness probe calls = %d, want canceled probe to be retried", probeCalls)
	}
}

func TestScanEndpointStartsAsyncAction(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	app.scanAll = func(context.Context) error {
		close(started)
		<-release
		return nil
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	addStateChangingHeader(req)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("scan status = %d, body=%q", res.Code, res.Body.String())
	}
	var action dashboardAction
	if err := json.NewDecoder(res.Body).Decode(&action); err != nil {
		t.Fatal(err)
	}
	if action.ID == "" || action.Action != "scan" || action.Status != actionStatusRunning {
		t.Fatalf("action = %#v, want running scan action", action)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scan action did not start")
	}

	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/actions/"+action.ID, nil))
	if res.Code != http.StatusOK {
		t.Fatalf("running action status = %d, body=%q", res.Code, res.Body.String())
	}
	if err := json.NewDecoder(res.Body).Decode(&action); err != nil {
		t.Fatal(err)
	}
	if action.Status != actionStatusRunning {
		t.Fatalf("action status = %s, want running", action.Status)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		res = httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/actions/"+action.ID, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("finished action status = %d, body=%q", res.Code, res.Body.String())
		}
		if err := json.NewDecoder(res.Body).Decode(&action); err != nil {
			t.Fatal(err)
		}
		if action.Status == actionStatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("action = %#v, want ok", action)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSummaryLogsDecodeError(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ctx := context.Background()
	if err := app.store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := app.store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.FinishScan(ctx, scanID, "repo", "abc123", []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "gitops-dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", "svc"); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("summary status = %d, want 500; body=%q", res.Code, res.Body.String())
	}
	for _, want := range []string{"dashboard storage degraded", "table=services", "key=svc", "column=images_json"} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("summary body = %q, want %q", res.Body.String(), want)
		}
	}
	for _, want := range []string{"summary unavailable", "table=services", "key=svc", "column=images_json"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs = %q, want %q", logs.String(), want)
		}
	}
}

func TestStateChangingEndpointsRequireCSRFHeaders(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	handler := app.Handler()

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("read-only summary status = %d, want 200", res.Code)
	}

	for _, endpoint := range []string{"/api/scan", "/api/monitor", "/api/monitor-overrides"} {
		res = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusForbidden {
			t.Fatalf("%s without CSRF header status = %d, want 403", endpoint, res.Code)
		}
	}

	res = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://dashboard.example.test/api/monitor", nil)
	addStateChangingHeader(req)
	req.Header.Set("Origin", "https://attacker.example.test")
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", res.Code)
	}

	devOrigin := "http://127.0.0.1:5173"
	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18080/api/monitor", nil)
	addStateChangingHeader(req)
	req.Header.Set("Origin", devOrigin)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("unconfigured dev origin status = %d, want 403", res.Code)
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://dashboard.example.test/api/monitor", nil)
	addStateChangingHeader(req)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch metadata status = %d, want 403", res.Code)
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://dashboard.example.test/api/monitor", nil)
	addStateChangingHeader(req)
	req.Header.Set("Origin", "http://dashboard.example.test")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("same-origin status = %d, body=%q", res.Code, res.Body.String())
	}

	app.cfg.Server.AllowedOrigins = []string{devOrigin, "http://localhost:5173"}
	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18080/api/monitor", nil)
	addStateChangingHeader(req)
	req.Header.Set("Origin", devOrigin)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("configured dev origin status = %d, body=%q", res.Code, res.Body.String())
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18080/api/monitor", nil)
	req.Header.Set("Origin", devOrigin)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("configured dev origin without CSRF header status = %d, want 403", res.Code)
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/monitor", nil)
	addStateChangingHeader(req)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("non-browser status = %d, body=%q", res.Code, res.Body.String())
	}
}

func TestStateChangingEndpointAllowsDevConfigOrigin(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "config.dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.RepoCacheDir = filepath.Join(cfg.Server.DataDir, "repos")
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18080/api/monitor", nil)
	addStateChangingHeader(req)
	req.Header.Set("Origin", "http://regula1.lan:5173")
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("regula1.lan dev origin status = %d, body=%q", res.Code, res.Body.String())
	}
}

func TestMonitorOverrideEndpointMarksTargetNotApplicable(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.store.ReplaceConfiguredServices(context.Background(), "repo", "prod/compose.yaml", []core.Service{{
		ID:          "svc",
		Name:        "api",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"http://10.10.10.20"},
	}}); err != nil {
		t.Fatal(err)
	}
	target := "routes: http://10.10.10.20"
	if err := app.store.UpsertStatus(context.Background(), core.StatusResult{
		ServiceID: "svc",
		Target:    target,
		Health:    core.HealthError,
		Message:   "dial failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	handler := app.Handler()
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/monitor-overrides", strings.NewReader(`{"serviceId":"svc","target":"routes: http://10.10.10.20","notApplicable":true}`))
	req.Header.Set("Content-Type", "application/json")
	addStateChangingHeader(req)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("override status = %d, body=%q", res.Code, res.Body.String())
	}
	summary, err := app.store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Statuses[0].Health != core.HealthNotApplicable {
		t.Fatalf("status health = %s, want not_applicable", summary.Statuses[0].Health)
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/monitor-overrides", strings.NewReader(`{"serviceId":"svc","target":"routes: http://10.10.10.20","notApplicable":false}`))
	req.Header.Set("Content-Type", "application/json")
	addStateChangingHeader(req)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("enable override status = %d, body=%q", res.Code, res.Body.String())
	}
	summary, err = app.store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Statuses[0].Health != core.HealthUnknown {
		t.Fatalf("re-enabled status health = %s, want unknown", summary.Statuses[0].Health)
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/monitor-overrides", strings.NewReader(`{"serviceId":"svc","target":"routes","notApplicable":true}`))
	req.Header.Set("Content-Type", "application/json")
	addStateChangingHeader(req)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("all-routes override status = %d, body=%q", res.Code, res.Body.String())
	}
	summary, err = app.store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundParent := false
	for _, status := range summary.Statuses {
		if status.Target == "routes" {
			foundParent = true
			if status.Health != core.HealthNotApplicable {
				t.Fatalf("all-routes health = %s, want not_applicable", status.Health)
			}
		}
		if strings.HasPrefix(status.Target, "routes: ") {
			t.Fatalf("child route status %q remained after parent override", status.Target)
		}
	}
	if !foundParent {
		t.Fatalf("all-routes override did not create routes status: %#v", summary.Statuses)
	}

	res = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/monitor-overrides", strings.NewReader(`{"serviceId":"svc","target":"missing","notApplicable":true}`))
	req.Header.Set("Content-Type", "application/json")
	addStateChangingHeader(req)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing override status = %d, want 404", res.Code)
	}
}

func TestNewSyncsConfiguredPingInventory(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	source := filepath.Join(dataDir, "source")
	writeFile(t, filepath.Join(source, "infrastructure", "inventory", "hosts.yml"), `
all:
  hosts:
    serenity:
      ansible_host: serenity.lan
`)
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "fixture")
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
		Runtime: config.RuntimeConfig{
			Ping: []config.PingTarget{{
				Name:             "homelab",
				Repository:       "fixture",
				AnsibleInventory: "infrastructure/inventory/hosts.yml",
			}},
		},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	summary, err := app.store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 1 {
		t.Fatalf("services = %#v, want one host", summary.Services)
	}
	service := summary.Services[0]
	if service.Name != "serenity" || service.Runtime != "host" || service.ResourceName != "serenity.lan" {
		t.Fatalf("service = %#v", service)
	}
	if service.Repository != "fixture" || service.SourcePath != "infrastructure/inventory/hosts.yml" {
		t.Fatalf("service provenance = %#v", service)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}

func addStateChangingHeader(req *http.Request) {
	req.Header.Set(stateChangingRequestHeader, "1")
}
