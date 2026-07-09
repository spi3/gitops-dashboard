package agent

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/dockerapi"
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
	containers, err := dockerapi.ListContainers(ctx, cfg.Docker.Host)
	if err != nil {
		return core.AgentMessage{}, err
	}
	imageInspector, err := dockerapi.NewImageInspector(cfg.Docker.Host)
	if err != nil {
		imageInspector = nil
	}
	message := core.AgentMessage{Target: cfg.Target, CheckedAt: time.Now().UTC()}
	for _, item := range containers {
		name := ""
		if len(item.Names) > 0 {
			name = item.Names[0]
		}
		repoDigests := item.RepoDigests
		if imageInspector != nil && dockerapi.LiveContainer(item.State, item.Status) {
			repoDigests = imageInspector.RepoDigests(ctx, item)
		}
		message.Containers = append(message.Containers, core.ContainerStatus{
			ID:           item.ID,
			Name:         name,
			Image:        item.Image,
			ImageID:      item.ImageID,
			RepoDigests:  repoDigests,
			Labels:       core.FilterDockerComposeLabels(item.Labels),
			State:        item.State,
			Status:       item.Status,
			Health:       inferContainerHealth(item.State, item.Status),
			RestartCount: item.RestartCount,
		})
	}
	return message, nil
}

func inferContainerHealth(state, status string) string {
	normalizedState := strings.ToLower(strings.TrimSpace(state))
	normalizedStatus := strings.ToLower(strings.TrimSpace(status))
	switch normalizedState {
	case "restarting":
		return "starting"
	case "running":
		if health := containerHealthFromStatus(normalizedStatus); health != "" {
			return health
		}
		return "healthy"
	case "paused":
		return "starting"
	case "":
		if strings.HasPrefix(normalizedStatus, "up") {
			if health := containerHealthFromStatus(normalizedStatus); health != "" {
				return health
			}
			return "healthy"
		}
		if strings.HasPrefix(normalizedStatus, "restarting") {
			return "starting"
		}
	}
	if normalizedState == "exited" {
		return "unhealthy"
	}

	if health := containerHealthFromStatus(normalizedStatus); health == "unhealthy" || health == "starting" || health == "none" {
		return health
	}
	return "unhealthy"
}

func containerHealthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(health: unhealthy)") || strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)") || strings.Contains(status, "(starting)"):
		return "starting"
	case strings.Contains(status, "(health: none)") || strings.Contains(status, "(health: no healthcheck)"):
		return "none"
	default:
		return ""
	}
}
