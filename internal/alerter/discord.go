package alerter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

type discordSink struct {
	baseSink
	webhookURL string
}

func newDiscordSink(cfg config.DiscordAlertSinkConfig, client *http.Client) (*discordSink, error) {
	timeout, err := cfg.TimeoutDuration()
	if err != nil {
		return nil, fmt.Errorf("discord sink %s: %w", cfg.Name, err)
	}
	return &discordSink{
		baseSink: baseSink{
			name:    cfg.Name,
			timeout: timeout,
			filters: newSinkFilters(cfg.Include, cfg.Exclude),
			client:  client,
		},
		webhookURL: cfg.WebhookURL,
	}, nil
}

type discordMessage struct {
	Content string `json:"content"`
}

func (s *discordSink) Deliver(ctx context.Context, event storage.AlertEvent) error {
	body, err := json.Marshal(discordMessage{Content: discordContent(newEventPayload(event))})
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// discordContent renders a short, readable message body: an icon and
// subject line, the state transition (if any), the producer's reason text,
// and a timestamped footer identifying the event kind.
func discordContent(p EventPayload) string {
	icon := "\U0001F534" // red circle
	if p.IsRecovery() {
		icon = "✅" // check mark
	}
	lines := []string{fmt.Sprintf("%s **%s**", icon, p.Subject())}
	switch {
	case p.OldState != "" && p.NewState != "":
		lines = append(lines, fmt.Sprintf("%s → %s", p.OldState, p.NewState))
	case p.NewState != "":
		lines = append(lines, p.NewState)
	}
	if p.Reason != "" {
		lines = append(lines, p.Reason)
	}
	lines = append(lines, fmt.Sprintf("_%s at %s_", p.Kind, p.OccurredAt.UTC().Format(time.RFC3339)))
	return strings.Join(lines, "\n")
}
