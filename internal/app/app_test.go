package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
)

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
