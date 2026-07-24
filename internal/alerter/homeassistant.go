package alerter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

type homeAssistantSink struct {
	baseSink
	url   string
	token string
}

// newHomeAssistantSink builds the standard HA webhook trigger URL,
// `{baseURL}/api/webhook/{webhookID}`. The long-lived access token is sent
// as a Bearer Authorization header on every request, per HA's API
// conventions, even though the plain webhook trigger endpoint itself does
// not require it -- this also covers HA instances that sit behind a reverse
// proxy enforcing bearer auth on all `/api/*` routes.
func newHomeAssistantSink(cfg config.HomeAssistantAlertSinkConfig, client *http.Client) (*homeAssistantSink, error) {
	timeout, err := cfg.TimeoutDuration()
	if err != nil {
		return nil, fmt.Errorf("home assistant sink %s: %w", cfg.Name, err)
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	return &homeAssistantSink{
		baseSink: baseSink{
			name:    cfg.Name,
			timeout: timeout,
			filters: newSinkFilters(cfg.Include, cfg.Exclude),
			client:  client,
		},
		url:   base + "/api/webhook/" + url.PathEscape(cfg.WebhookID),
		token: cfg.Token,
	}, nil
}

func (s *homeAssistantSink) Deliver(ctx context.Context, event storage.AlertEvent) error {
	body, err := json.Marshal(newEventPayload(event))
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
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
