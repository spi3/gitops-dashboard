package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/gitops-dashboard/internal/agent"
	"github.com/example/gitops-dashboard/internal/app"
	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/version"
)

func main() {
	var (
		mode        = flag.String("mode", "server", "server or agent")
		configPath  = flag.String("config", "config.yaml", "path to configuration file")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *mode != config.ModeServer && *mode != config.ModeAgent {
		logger.Error("unknown mode", "mode", *mode)
		os.Exit(2)
	}

	cfg, err := config.LoadForMode(*configPath, *mode)
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	warnUnreadableConfiguredFiles(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch *mode {
	case config.ModeServer:
		if err := runServer(ctx, cfg, logger); err != nil {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	case config.ModeAgent:
		if err := agent.Run(ctx, cfg, logger); err != nil {
			logger.Error("agent failed", "error", err)
			os.Exit(1)
		}
	}
}

func warnUnreadableConfiguredFiles(cfg config.Config, logger *slog.Logger) {
	seen := map[string]struct{}{}
	for _, repo := range cfg.Repositories {
		warnUnreadableConfiguredFile(logger, seen, "repository ssh key", repo.Name, repo.SSHKeyPath)
		warnUnreadableConfiguredFile(logger, seen, "repository known hosts", repo.Name, repo.KnownHosts)
	}
	for _, target := range cfg.Runtime.Kubernetes {
		warnUnreadableConfiguredFile(logger, seen, "kubernetes kubeconfig", target.Name, target.Kubeconfig)
	}
}

func warnUnreadableConfiguredFile(logger *slog.Logger, seen map[string]struct{}, kind, name, path string) {
	if path == "" {
		return
	}
	key := kind + "\x00" + path
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	file, err := os.Open(path)
	if err == nil {
		_ = file.Close()
		return
	}
	logger.Warn(
		"configured file is not readable",
		"kind", kind,
		"name", name,
		"path", path,
		"error", err,
	)
}

func runServer(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	serverApp, err := app.New(cfg, logger)
	if err != nil {
		return err
	}
	defer serverApp.Close()
	serverApp.RunBackground(ctx)

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           serverApp.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("starting server", "listen", cfg.Server.Listen)
		errs <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errs:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("listen: %w", err)
	}
}
