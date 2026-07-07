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
)

func main() {
	var (
		mode       = flag.String("mode", "server", "server or agent")
		configPath = flag.String("config", "config.yaml", "path to configuration file")
	)
	flag.Parse()

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
