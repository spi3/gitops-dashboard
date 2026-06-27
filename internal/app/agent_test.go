package app

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/example/gitops-dashboard/internal/config"
)

func TestAgentEndpointRejectsInvalidToken(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      t.TempDir(),
			RepoCacheDir: filepath.Join(t.TempDir(), "repos"),
		},
		Auth: config.AuthConfig{
			Mode:  "dev-no-auth",
			Agent: config.AgentAuthCfg{Tokens: []string{"valid"}},
		},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/agents/connect?token=invalid", nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
}
