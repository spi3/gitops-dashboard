package agent

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/gorilla/websocket"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.Agent.ServerURL == "" || cfg.Agent.Token == "" {
		return nil
	}
	interval := 30 * time.Second
	if cfg.Agent.Interval != "" {
		if parsed, err := time.ParseDuration(cfg.Agent.Interval); err == nil {
			interval = parsed
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := sendOnce(ctx, cfg); err != nil {
			logger.Error("agent report failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func sendOnce(ctx context.Context, cfg config.Config) error {
	header := http.Header{"X-Agent-Token": []string{cfg.Agent.Token}}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, cfg.Agent.ServerURL, header)
	if err != nil {
		return err
	}
	defer conn.Close()
	status, err := collectDocker(ctx, cfg.Agent)
	if err != nil {
		return err
	}
	return conn.WriteJSON(status)
}

func collectDocker(ctx context.Context, cfg config.AgentConfig) (core.AgentMessage, error) {
	containers, err := listDockerContainers(ctx, cfg.Docker.Host)
	if err != nil {
		return core.AgentMessage{}, err
	}
	message := core.AgentMessage{Target: cfg.Target, CheckedAt: time.Now().UTC()}
	for _, item := range containers {
		name := ""
		if len(item.Names) > 0 {
			name = item.Names[0]
		}
		message.Containers = append(message.Containers, core.ContainerStatus{
			ID:     item.ID,
			Name:   name,
			Image:  item.Image,
			State:  item.State,
			Status: item.Status,
		})
	}
	return message, nil
}
