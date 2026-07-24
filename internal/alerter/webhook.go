package alerter

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

// defaultWebhookBodyTemplate is used when a webhook sink does not configure
// bodyTemplate: a plain JSON encoding of EventPayload.
const defaultWebhookBodyTemplate = `{"eventId":{{.EventID}},"kind":{{.Kind | printf "%q"}},"serviceId":{{.ServiceID | printf "%q"}},"target":{{.Target | printf "%q"}},"repository":{{.Repository | printf "%q"}},"agent":{{.Agent | printf "%q"}},"oldState":{{.OldState | printf "%q"}},"newState":{{.NewState | printf "%q"}},"reason":{{.Reason | printf "%q"}},"occurredAt":{{.OccurredAt.Format "2006-01-02T15:04:05Z07:00" | printf "%q"}}}`

type webhookSink struct {
	baseSink
	method  string
	url     string
	headers map[string]string
	body    *template.Template
}

// newWebhookSink parses the body template eagerly so a malformed template
// fails sink construction (i.e. process/worker startup) rather than being
// discovered only when the first matching event is delivered.
func newWebhookSink(cfg config.WebhookAlertSinkConfig, client *http.Client) (*webhookSink, error) {
	timeout, err := cfg.TimeoutDuration()
	if err != nil {
		return nil, fmt.Errorf("webhook sink %s: %w", cfg.Name, err)
	}
	rawTemplate := strings.TrimSpace(cfg.BodyTemplate)
	if rawTemplate == "" {
		rawTemplate = defaultWebhookBodyTemplate
	}
	parsed, err := template.New(cfg.Name).Parse(rawTemplate)
	if err != nil {
		return nil, fmt.Errorf("webhook sink %s: parse bodyTemplate: %w", cfg.Name, err)
	}
	// Fail fast on a template that parses but cannot execute against the
	// documented payload shape (e.g. references an undefined field).
	if err := parsed.Execute(new(bytes.Buffer), EventPayload{}); err != nil {
		return nil, fmt.Errorf("webhook sink %s: render bodyTemplate: %w", cfg.Name, err)
	}
	return &webhookSink{
		baseSink: baseSink{
			name:    cfg.Name,
			timeout: timeout,
			filters: newSinkFilters(cfg.Include, cfg.Exclude),
			client:  client,
		},
		method:  cfg.Method,
		url:     cfg.URL,
		headers: cfg.Headers,
		body:    parsed,
	}, nil
}

func (s *webhookSink) Deliver(ctx context.Context, event storage.AlertEvent) error {
	var buf bytes.Buffer
	if err := s.body.Execute(&buf, newEventPayload(event)); err != nil {
		return fmt.Errorf("render body template: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, s.method, s.url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range s.headers {
		req.Header.Set(name, value)
	}
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
