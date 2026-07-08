package app

import (
	"context"
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
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("frontend status = %d", res.Code)
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
