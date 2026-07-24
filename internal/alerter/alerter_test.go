package alerter

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

func newAlertTestStore(t *testing.T) (*storage.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dbPath
}

func seedEvent(t *testing.T, store *storage.Store, event storage.AlertEvent, sinks []string) storage.AlertEvent {
	t.Helper()
	stored, _, err := store.EnqueueAlertEvent(context.Background(), event, sinks, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// alertEventStatus reads alert_events.status directly from the SQLite file.
// The alerter package only sees storage's public API, which (by design)
// stops surfacing a dispatch once it reaches a terminal status, so tests
// that assert on terminal outcomes must inspect the database directly
// rather than through ListUndeliveredAlertDeliveries.
func alertEventStatus(t *testing.T, dbPath string, eventID int64) string {
	t.Helper()
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var status string
	if err := raw.QueryRowContext(context.Background(), `SELECT status FROM alert_events WHERE id=?`, eventID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestWorkerRoutesOnlyToMatchingSinksAndSkipsExcluded covers requirement 8's
// routing/filtering item: an event enqueued for a sink is only actually
// delivered when it passes that sink's include/exclude filter; a filtered
// event is completed without a network call.
func TestWorkerRoutesOnlyToMatchingSinksAndSkipsExcluded(t *testing.T) {
	t.Parallel()
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, dbPath := newAlertTestStore(t)
	cfg := config.AlertingConfig{
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "webhook", URL: server.URL, Method: "POST", Timeout: "2s",
				Include: config.AlertSinkFilterConfig{Services: []string{"included-svc"}},
			},
		},
	}
	worker, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	matching := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "included-svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "match",
	}, []string{"webhook"})
	excluded := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "other-svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "no-match",
	}, []string{"webhook"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.pollOnce(ctx)

	waitFor(t, 2*time.Second, func() bool {
		return alertEventStatus(t, dbPath, matching.ID) == storage.AlertEventStatusDelivered &&
			alertEventStatus(t, dbPath, excluded.ID) == storage.AlertEventStatusDelivered
	})
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests = %d, want exactly one delivery (excluded event must not reach the sink)", got)
	}
}

// TestWorkerRetriesWithBackoffAndDeadLetters covers requirement 8's
// retry/backoff/dead-letter item end to end through the worker.
func TestWorkerRetriesWithBackoffAndDeadLetters(t *testing.T) {
	t.Parallel()
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	store, dbPath := newAlertTestStore(t)
	cfg := config.AlertingConfig{
		Retry: config.AlertRetryConfig{MaxAttempts: 2, InitialInterval: "1ms", MaxInterval: "2ms"},
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "webhook", URL: server.URL, Method: "POST", Timeout: "2s",
			},
		},
	}
	worker, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	event := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "retry-then-dead-letter",
	}, []string{"webhook"})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		worker.pollOnce(ctx)
		if alertEventStatus(t, dbPath, event.ID) == storage.AlertEventStatusFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := alertEventStatus(t, dbPath, event.ID); got != storage.AlertEventStatusFailed {
		t.Fatalf("event status = %q, want failed after exhausting retries", got)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("requests = %d, want exactly maxAttempts (2) delivery attempts", got)
	}
}

// TestWorkerIdempotentRedeliveryAfterRestart covers requirement 8's restart
// idempotency item: a second Worker instance opened against the same store
// (simulating a process restart) must not redeliver an already-completed
// dispatch.
func TestWorkerIdempotentRedeliveryAfterRestart(t *testing.T) {
	t.Parallel()
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.AlertingConfig{
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "webhook", URL: server.URL, Method: "POST", Timeout: "2s",
			},
		},
	}
	workerA, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	event := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "restart-idempotent",
	}, []string{"webhook"})

	ctx := context.Background()
	workerA.pollOnce(ctx)
	waitFor(t, 2*time.Second, func() bool {
		return alertEventStatus(t, dbPath, event.ID) == storage.AlertEventStatusDelivered
	})
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests after first delivery = %d, want 1", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workerB, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	workerB.pollOnce(ctx)
	workerB.pollOnce(ctx)
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests after restart = %d, want still 1 (no redelivery)", got)
	}
}

// TestStallingSinkDoesNotBlockOtherDeliveriesOrTheStore covers requirement
// 8's isolation item: a sink that hangs for its full timeout must not delay
// delivery to a different, healthy sink, and must not hold up unrelated
// store operations (standing in for the monitor loop, which shares the same
// *storage.Store).
func TestStallingSinkDoesNotBlockOtherDeliveriesOrTheStore(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	stalling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer stalling.Close()
	defer close(release)

	var healthyRequests int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&healthyRequests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	store, dbPath := newAlertTestStore(t)
	cfg := config.AlertingConfig{
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "stalling", URL: stalling.URL, Method: "POST", Timeout: time.Hour.String(),
			},
			Discord: config.DiscordAlertSinkConfig{
				Enabled: true, Name: "healthy", WebhookURL: healthy.URL, Timeout: "2s",
			},
		},
	}
	worker, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc-a", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "stall",
	}, []string{"stalling"})
	healthyEvent := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc-b", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "healthy-sibling",
	}, []string{"healthy"})

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		worker.pollOnce(ctx)
		close(done)
	}()

	// The healthy sink's delivery, and an unrelated store write, must both
	// complete promptly even while the stalling sink's request is in flight.
	waitFor(t, 2*time.Second, func() bool {
		return alertEventStatus(t, dbPath, healthyEvent.ID) == storage.AlertEventStatusDelivered
	})
	if got := atomic.LoadInt32(&healthyRequests); got != 1 {
		t.Fatalf("healthy sink requests = %d, want 1", got)
	}
	independentStart := time.Now()
	if err := store.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(independentStart); elapsed > time.Second {
		t.Fatalf("independent store operation took %s while a sink was stalled, want near-instant", elapsed)
	}

	select {
	case <-done:
		t.Fatal("pollOnce returned before the stalling sink's request was released")
	default:
	}
}

// TestWorkerDoesNotStartWhenNoSinkIsConfigured covers requirement 8's
// disabled-worker item.
func TestWorkerDoesNotStartWhenNoSinkIsConfigured(t *testing.T) {
	t.Parallel()
	store, dbPath := newAlertTestStore(t)
	worker, err := New(config.AlertingConfig{}, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if worker.Enabled() {
		t.Fatal("Enabled() = true, want false with no sinks configured")
	}
	event := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "disabled-worker",
	}, []string{"webhook"})

	ctx, cancel := context.WithCancel(context.Background())
	worker.Run(ctx)
	cancel()

	if got := alertEventStatus(t, dbPath, event.ID); got != storage.AlertEventStatusPending {
		t.Fatalf("event status = %q, want pending (worker must never claim when disabled)", got)
	}
}

func TestNewWebhookSinkRejectsInvalidBodyTemplateAtConstruction(t *testing.T) {
	t.Parallel()
	store, _ := newAlertTestStore(t)
	cfg := config.AlertingConfig{
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "webhook", URL: "https://hooks.example.test", Method: "POST", Timeout: "2s",
				BodyTemplate: `{{ .NotAField }}`,
			},
		},
	}
	if _, err := New(cfg, store, testLogger()); err == nil {
		t.Fatal("New succeeded, want bodyTemplate validation error at construction")
	}
}

func TestWebhookSinkSendsMethodHeadersAndRenderedTemplateBody(t *testing.T) {
	t.Parallel()
	var gotMethod, gotContentType, gotCustomHeader string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotCustomHeader = r.Header.Get("X-Custom")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, dbPath := newAlertTestStore(t)
	cfg := config.AlertingConfig{
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "webhook", URL: server.URL, Method: "PUT", Timeout: "2s",
				Headers:      map[string]string{"X-Custom": "value"},
				BodyTemplate: `{"service":{{.ServiceID | printf "%q"}},"summary":{{.Summary | printf "%q"}}}`,
			},
		},
	}
	worker, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	event := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "webhook-shape",
	}, []string{"webhook"})

	worker.pollOnce(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		return alertEventStatus(t, dbPath, event.ID) == storage.AlertEventStatusDelivered
	})

	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotContentType)
	}
	if gotCustomHeader != "value" {
		t.Fatalf("X-Custom header = %q, want value", gotCustomHeader)
	}
	if gotBody["service"] != "svc" {
		t.Fatalf("body = %#v, want service=svc from rendered template", gotBody)
	}
}

func TestDiscordSinkFormatsReadableMessagesForEventKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		event storage.AlertEvent
		want  []string
	}{
		{
			name: "health incident",
			event: storage.AlertEvent{
				Kind: "health.transition", ServiceID: "checkout", OldState: "healthy", NewState: "unhealthy",
				Reason: "3 consecutive failures",
			},
			want: []string{"checkout", "healthy", "unhealthy", "3 consecutive failures"},
		},
		{
			name: "health recovery",
			event: storage.AlertEvent{
				Kind: "health.recovery", ServiceID: "checkout", OldState: "unhealthy", NewState: "healthy",
				Reason: "service recovered after 2m0s",
			},
			want: []string{"checkout", "recovered"},
		},
		{
			name:  "agent offline (future producer kind)",
			event: storage.AlertEvent{Kind: "agent.disconnect", Agent: "serenity", NewState: "offline", Reason: "no heartbeat for 90s"},
			want:  []string{"serenity", "no heartbeat for 90s"},
		},
		{
			name:  "scan failure (future producer kind)",
			event: storage.AlertEvent{Kind: "scan.failure", Repository: "infra", NewState: "failed", Reason: "git fetch failed"},
			want:  []string{"infra", "git fetch failed"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var gotBody map[string]string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			store, dbPath := newAlertTestStore(t)
			cfg := config.AlertingConfig{
				Sinks: config.AlertingSinksConfig{
					Discord: config.DiscordAlertSinkConfig{Enabled: true, Name: "discord", WebhookURL: server.URL, Timeout: "2s"},
				},
			}
			worker, err := New(cfg, store, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			testCase.event.DedupeKey = testCase.name
			event := seedEvent(t, store, testCase.event, []string{"discord"})
			worker.pollOnce(context.Background())
			waitFor(t, 2*time.Second, func() bool {
				return alertEventStatus(t, dbPath, event.ID) == storage.AlertEventStatusDelivered
			})
			content := gotBody["content"]
			for _, want := range testCase.want {
				if !strings.Contains(content, want) {
					t.Fatalf("discord content = %q, want it to contain %q", content, want)
				}
			}
		})
	}
}

func TestHomeAssistantSinkPostsToWebhookRouteWithBearerToken(t *testing.T) {
	t.Parallel()
	var gotPath, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, dbPath := newAlertTestStore(t)
	cfg := config.AlertingConfig{
		Sinks: config.AlertingSinksConfig{
			HomeAssistant: config.HomeAssistantAlertSinkConfig{
				Enabled: true, Name: "ha", BaseURL: server.URL, Token: "ha-long-lived-token", WebhookID: "gitops-alerts", Timeout: "2s",
			},
		},
	}
	worker, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	event := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "ha-shape",
	}, []string{"ha"})

	worker.pollOnce(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		return alertEventStatus(t, dbPath, event.ID) == storage.AlertEventStatusDelivered
	})

	if gotPath != "/api/webhook/gitops-alerts" {
		t.Fatalf("path = %q, want /api/webhook/gitops-alerts", gotPath)
	}
	if gotAuth != "Bearer ha-long-lived-token" {
		t.Fatalf("authorization = %q, want Bearer ha-long-lived-token", gotAuth)
	}
	if gotBody["serviceId"] != "svc" {
		t.Fatalf("body = %#v, want serviceId=svc", gotBody)
	}
}

// TestWorkerNeverPersistsSecretMaterialInDispatchErrors covers N-2: a
// failing delivery's error text must be redacted before it reaches storage,
// even though the worker's own error formatting embeds the raw Go error
// (which, for a URL-based transport failure, can echo back the URL).
func TestWorkerNeverPersistsSecretMaterialInDispatchErrors(t *testing.T) {
	t.Parallel()
	const secretPathToken = "super-secret-webhook-path-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.AddRedactionValues(secretPathToken)
	cfg := config.AlertingConfig{
		Retry: config.AlertRetryConfig{MaxAttempts: 1, InitialInterval: "1ms", MaxInterval: "1ms"},
		Sinks: config.AlertingSinksConfig{
			Webhook: config.WebhookAlertSinkConfig{
				Enabled: true, Name: "webhook", URL: server.URL + "/" + secretPathToken, Method: "POST", Timeout: "2s",
			},
		},
	}
	worker, err := New(cfg, store, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	event := seedEvent(t, store, storage.AlertEvent{
		Kind: "health.transition", ServiceID: "svc", OldState: "healthy", NewState: "unhealthy",
		DedupeKey: "redact-dispatch-error",
	}, []string{"webhook"})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		worker.pollOnce(ctx)
		if alertEventStatus(t, dbPath, event.ID) == storage.AlertEventStatusFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := alertEventStatus(t, dbPath, event.ID); got != storage.AlertEventStatusFailed {
		t.Fatalf("event status = %q, want failed", got)
	}

	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var lastError string
	if err := raw.QueryRowContext(ctx, `SELECT last_error FROM alert_dispatches WHERE event_id=?`, event.ID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lastError, secretPathToken) {
		t.Fatalf("dispatch last_error = %q, want secret path token redacted", lastError)
	}
}
