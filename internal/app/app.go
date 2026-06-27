package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/example/gitops-dashboard/internal/auth"
	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/monitor"
	"github.com/example/gitops-dashboard/internal/scanner"
	"github.com/example/gitops-dashboard/internal/storage"
	"github.com/example/gitops-dashboard/internal/ui"
	"github.com/gorilla/websocket"
)

type App struct {
	cfg      config.Config
	store    *storage.Store
	scanner  scanner.Scanner
	monitor  monitor.Monitor
	auth     auth.BasicAuth
	logger   *slog.Logger
	upgrader websocket.Upgrader
}

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	dbPath := filepath.Join(cfg.Server.DataDir, "gitops-dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureRepositories(context.Background(), cfg.Repositories); err != nil {
		_ = store.Close()
		return nil, err
	}
	app := &App{
		cfg:    cfg,
		store:  store,
		auth:   auth.New(cfg.Auth),
		logger: logger,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	app.scanner = scanner.New(cfg, store, logger)
	app.monitor = monitor.New(cfg, store, logger)
	return app, nil
}

func (app *App) Close() {
	if app.store != nil {
		_ = app.store.Close()
	}
}

func (app *App) RunBackground(ctx context.Context) {
	app.scanner.RunScheduled(ctx)
	app.monitor.Run(ctx)
}

func (app *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /readyz", app.ready)
	mux.HandleFunc("GET /api/summary", app.summary)
	mux.HandleFunc("POST /api/scan", app.scan)
	mux.HandleFunc("POST /api/monitor", app.checkMonitor)
	mux.HandleFunc("GET /api/agents/connect", app.agentConnect)
	mux.Handle("GET /", app.staticHandler())
	return app.auth.Middleware(mux)
}

func (app *App) health(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

func (app *App) ready(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ready\n"))
}

func (app *App) summary(w http.ResponseWriter, r *http.Request) {
	summary, err := app.store.Summary(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, summary)
}

func (app *App) scan(w http.ResponseWriter, r *http.Request) {
	if err := app.scanner.ScanAll(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (app *App) checkMonitor(w http.ResponseWriter, r *http.Request) {
	if err := app.monitor.CheckAll(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (app *App) agentConnect(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Agent-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if !app.validAgentToken(token) {
		http.Error(w, "agent authentication failed", http.StatusUnauthorized)
		return
	}
	conn, err := app.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var message core.AgentMessage
		if err := conn.ReadJSON(&message); err != nil {
			app.logger.Info("agent disconnected", "error", err)
			return
		}
		if err := app.store.UpsertAgent(r.Context(), message); err != nil {
			app.logger.Error("agent status persist failed", "error", err)
			return
		}
	}
}

func (app *App) validAgentToken(token string) bool {
	if token == "" {
		return false
	}
	for _, candidate := range app.cfg.Auth.Agent.Tokens {
		if candidate == token {
			return true
		}
	}
	for _, target := range app.cfg.Runtime.Docker {
		if target.AgentToken == token {
			return true
		}
	}
	return false
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

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
