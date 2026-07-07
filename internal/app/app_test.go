package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

func TestNewSyncsConfiguredPingInventory(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	inventory := filepath.Join(dataDir, "hosts.yml")
	if err := os.WriteFile(inventory, []byte(`
all:
  hosts:
    serenity:
      ansible_host: serenity.lan
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Server: config.ServerConfig{
			Listen:       ":0",
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Auth:       config.AuthConfig{Mode: "dev-no-auth"},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
		Runtime: config.RuntimeConfig{
			Ping: []config.PingTarget{{Name: "homelab", AnsibleInventory: inventory}},
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
}
