package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/gorilla/websocket"
)

func TestAgentReportAppearsInSummary(t *testing.T) {
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
		Runtime: config.RuntimeConfig{
			Docker: []config.DockerTarget{
				{Name: "serenity", Kind: "agent"},
				{Name: "albert", Kind: "agent"},
			},
		},
	}
	app, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/agents/connect"
	header := http.Header{"X-Agent-Token": []string{"valid"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	message := core.AgentMessage{
		Target: "serenity",
		Containers: []core.ContainerStatus{{
			Name:  "/stack-web-1",
			Image: "example/web:v1",
			State: "running",
		}},
	}
	if err := conn.WriteJSON(message); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	if err := waitForAgentReport(context.Background(), app, "serenity"); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("summary status = %d", res.Code)
	}
	var summary core.DashboardSummary
	if err := json.Unmarshal(res.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Agents == nil {
		t.Fatal("summary.Agents is nil, want non-nil slice")
	}
	byTarget := map[string]core.AgentInfo{}
	for _, agent := range summary.Agents {
		byTarget[agent.Target] = agent
	}
	serenity, ok := byTarget["serenity"]
	if !ok || serenity.LastSeenAt == "" || !serenity.Configured {
		t.Fatalf("serenity = %#v, want configured and reported", serenity)
	}
	if len(serenity.Containers) != 1 || serenity.Containers[0].Name != "/stack-web-1" {
		t.Fatalf("serenity containers = %#v", serenity.Containers)
	}
	albert, ok := byTarget["albert"]
	if !ok || albert.LastSeenAt != "" || !albert.Configured {
		t.Fatalf("albert = %#v, want configured-never-reported", albert)
	}
}

// waitForAgentReport polls until the agent's websocket report has been
// persisted, since the server applies it asynchronously in agentConnect.
func waitForAgentReport(ctx context.Context, app *App, target string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		agents, err := app.store.Agents(ctx)
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if agent.Target == target {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for agent %q to be persisted", target)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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
