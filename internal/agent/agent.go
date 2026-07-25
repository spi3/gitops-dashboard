package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/agentprotocol"
	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/dockerapi"
	"github.com/gorilla/websocket"
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.Agent.ServerURL == "" || cfg.Agent.Token == "" {
		return errors.New("agent.serverUrl and agent.token are required")
	}
	interval, err := cfg.Agent.IntervalDuration()
	if err != nil {
		return err
	}
	if interval == 0 {
		interval = 30 * time.Second
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

// sendOnce collects and normalizes a Docker report before ever constructing
// or dialing the WebSocket, then dials, writes exactly one text report
// message, and reads exactly one acknowledgement. Every returned error wraps
// one of the classification sentinels in this package so callers can test
// the category with errors.Is regardless of the underlying cause, and no
// returned error ever includes the agent token, a response body, or raw
// handshake headers.
func sendOnce(ctx context.Context, cfg config.Config) error {
	message, err := collectDocker(ctx, cfg.Agent)
	if err != nil {
		return classifyAgentError(errAgentCollectionFailure, err)
	}
	normalizeAgentMessage(&message)

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = agentDialHandshakeTimeout

	header := http.Header{"X-Agent-Token": []string{cfg.Agent.Token}}
	conn, resp, err := dialer.DialContext(ctx, cfg.Agent.ServerURL, header)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return classifyAgentError(errAgentServerRejection, err)
		}
		return classifyAgentError(errAgentConnectionFailure, err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(agentReportWriteTimeout)); err != nil {
		return classifyAgentError(errAgentConnectionFailure, err)
	}
	if err := conn.WriteJSON(message); err != nil {
		return classifyAgentError(errAgentConnectionFailure, err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(agentAckReadTimeout)); err != nil {
		return classifyAgentError(errAgentConnectionFailure, err)
	}
	conn.SetReadLimit(agentAckReadLimit)

	messageType, data, err := conn.ReadMessage()
	if err != nil {
		if isAgentReadTimeout(err) {
			return classifyAgentError(errAgentAcknowledgementTimeout, err)
		}
		return classifyAgentError(errAgentProtocolFailure, err)
	}
	if messageType != websocket.TextMessage {
		return classifyAgentError(errAgentProtocolFailure, errBinaryAcknowledgement)
	}

	ack, err := decodeAgentReportAck(data)
	if err != nil {
		return classifyAgentError(errAgentProtocolFailure, err)
	}

	switch {
	case ack.Status == agentprotocol.AckStatusOK && ack.Code == agentprotocol.AckCodePersisted:
		return nil
	case ack.Status == agentprotocol.AckStatusError && isAgentAckErrorCode(ack.Code):
		return classifyAgentErrorCode(errAgentServerRejection, ack.Code)
	default:
		return classifyAgentError(errAgentProtocolFailure, errInvalidAcknowledgementPairing)
	}
}

// normalizeAgentMessage applies the collect-before-dial normalization
// contract: Containers is always non-null, and every container's
// RepoDigests is always non-null.
func normalizeAgentMessage(message *core.AgentMessage) {
	if message.Containers == nil {
		message.Containers = []core.ContainerStatus{}
	}
	for i := range message.Containers {
		if message.Containers[i].RepoDigests == nil {
			message.Containers[i].RepoDigests = []string{}
		}
	}
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
