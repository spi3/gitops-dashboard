// Package alerter delivers persisted alert events (internal/storage) to
// configured sinks asynchronously. It never blocks or delays the monitor
// loop: delivery runs on its own goroutines against its own claimed batch of
// work, and a hanging sink only holds up its own delivery attempt.
package alerter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

const (
	defaultPollInterval        = 5 * time.Second
	defaultLeaseDuration       = 2 * time.Minute
	defaultClaimBatchSize      = 20
	defaultSinkRequestTimeout  = 10 * time.Second
	alertRetentionPruneTimeout = 30 * time.Second
)

// Worker consumes undelivered alert_events, routes each to every enabled
// sink whose filters match, and records the outcome as an alert_dispatches
// attempt. Retry backoff, dead-lettering, and idempotent redelivery after a
// restart are implemented by the storage layer (T-021); Worker only decides
// *when* to claim and *how* to deliver.
type Worker struct {
	store  *storage.Store
	logger *slog.Logger
	sinks  map[string]Sink

	retryPolicy   storage.AlertRetryPolicy
	pollInterval  time.Duration
	leaseDuration time.Duration
	claimBatch    int
	workerID      string

	retentionHorizon  time.Duration
	retentionInterval time.Duration
	retentionBatch    int
}

// New builds a Worker from alerting config. Every enabled sink is
// constructed eagerly, so a malformed body template or other invalid sink
// configuration is reported here (process/worker startup) rather than at
// first delivery. New always succeeds structurally when the sinks
// themselves are valid; Run is a no-op when no sink ended up enabled.
func New(cfg config.AlertingConfig, store *storage.Store, logger *slog.Logger) (*Worker, error) {
	if logger == nil {
		logger = slog.Default()
	}
	sinks, err := buildSinks(cfg)
	if err != nil {
		return nil, err
	}

	retryPolicy := storage.DefaultAlertRetryPolicy()
	if cfg.Retry.MaxAttempts > 0 {
		retryPolicy.MaxAttempts = cfg.Retry.MaxAttempts
	}
	if initial, err := cfg.RetryInitialIntervalDuration(); err == nil && initial > 0 {
		retryPolicy.InitialInterval = initial
	}
	if maximum, err := cfg.RetryMaxIntervalDuration(); err == nil && maximum > 0 {
		retryPolicy.MaxInterval = maximum
	}

	retentionBatch := cfg.Retention.BatchSize
	if retentionBatch <= 0 {
		retentionBatch = storage.DefaultAlertRetentionBatchSize
	}
	retentionHorizon, _ := cfg.RetentionHorizonDuration()
	retentionInterval, _ := cfg.RetentionIntervalDuration()

	workerID, err := randomWorkerID()
	if err != nil {
		return nil, fmt.Errorf("alerter: %w", err)
	}

	return &Worker{
		store:             store,
		logger:            logger,
		sinks:             sinks,
		retryPolicy:       retryPolicy,
		pollInterval:      defaultPollInterval,
		leaseDuration:     defaultLeaseDuration,
		claimBatch:        defaultClaimBatchSize,
		workerID:          workerID,
		retentionHorizon:  retentionHorizon,
		retentionInterval: retentionInterval,
		retentionBatch:    retentionBatch,
	}, nil
}

func buildSinks(cfg config.AlertingConfig) (map[string]Sink, error) {
	sinks := map[string]Sink{}
	client := &http.Client{}
	if cfg.Sinks.Webhook.Enabled {
		sink, err := newWebhookSink(cfg.Sinks.Webhook, client)
		if err != nil {
			return nil, err
		}
		sinks[sink.Name()] = sink
	}
	if cfg.Sinks.Discord.Enabled {
		sink, err := newDiscordSink(cfg.Sinks.Discord, client)
		if err != nil {
			return nil, err
		}
		sinks[sink.Name()] = sink
	}
	if cfg.Sinks.HomeAssistant.Enabled {
		sink, err := newHomeAssistantSink(cfg.Sinks.HomeAssistant, client)
		if err != nil {
			return nil, err
		}
		sinks[sink.Name()] = sink
	}
	return sinks, nil
}

func randomWorkerID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate worker id: %w", err)
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "gitops-dashboard"
	}
	return fmt.Sprintf("alerter-%s-%d-%s", host, os.Getpid(), hex.EncodeToString(buf)), nil
}

// Enabled reports whether at least one sink is configured for delivery.
func (w *Worker) Enabled() bool {
	return len(w.sinks) > 0
}

// Run starts the delivery and retention loops on their own goroutines and
// returns immediately; it never blocks the caller. It is a no-op when no
// sink is enabled.
func (w *Worker) Run(ctx context.Context) {
	if !w.Enabled() {
		w.logger.Info("alerter disabled: no sinks configured")
		return
	}
	go w.runDeliveryLoop(ctx)
	if w.retentionHorizon > 0 && w.retentionInterval > 0 {
		go w.runRetentionLoop(ctx)
	}
}

func (w *Worker) runDeliveryLoop(ctx context.Context) {
	w.pollOnce(ctx)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

// pollOnce claims one batch of due dispatches and delivers each
// concurrently, so a sink that hangs for its full timeout does not delay
// delivery to the other sinks or events claimed in the same batch.
func (w *Worker) pollOnce(ctx context.Context) {
	deliveries, err := w.store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, w.workerID, w.leaseDuration, w.claimBatch, w.retryPolicy)
	if err != nil {
		if errors.Is(err, storage.ErrAlertStateLocked) {
			return
		}
		w.logger.Error("alert claim failed", "error", err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, delivery := range deliveries {
		wg.Add(1)
		go func(delivery storage.AlertDelivery) {
			defer wg.Done()
			w.deliverOne(ctx, delivery)
		}(delivery)
	}
	wg.Wait()
}

func (w *Worker) deliverOne(ctx context.Context, delivery storage.AlertDelivery) {
	sink, ok := w.sinks[delivery.Dispatch.Sink]
	if !ok || !sink.Matches(delivery.Event) {
		w.logger.Info("alert dispatch skipped", "sink", delivery.Dispatch.Sink, "event", delivery.Event.DedupeKey, "reason", "sink disabled or excluded by filter")
		w.complete(ctx, delivery, storage.AlertDispatchStatusDelivered, "")
		return
	}
	deliverCtx, cancel := context.WithTimeout(ctx, sink.Timeout())
	defer cancel()
	if err := sink.Deliver(deliverCtx, delivery.Event); err != nil {
		w.logger.Warn("alert dispatch attempt failed", "sink", sink.Name(), "event", delivery.Event.DedupeKey, "attempt", delivery.Dispatch.Attempts+1)
		w.complete(ctx, delivery, storage.AlertDispatchStatusPending, fmt.Sprintf("%s: %v", sink.Name(), err))
		return
	}
	w.logger.Info("alert dispatch delivered", "sink", sink.Name(), "event", delivery.Event.DedupeKey)
	w.complete(ctx, delivery, storage.AlertDispatchStatusDelivered, "")
}

// complete records the outcome of a claimed dispatch. Only the sink name,
// event dedupe key, and outcome are logged -- never request/response bodies
// or URLs -- and lastError is passed through the store's own redaction
// (backed by the alerting secrets registered at config load) before it is
// ever persisted.
func (w *Worker) complete(ctx context.Context, delivery storage.AlertDelivery, status, lastError string) {
	if _, err := w.store.RecordAlertDispatchResultWithRetryPolicy(ctx, delivery.Dispatch.ID, w.workerID, delivery.Dispatch.ClaimID, status, lastError, w.retryPolicy); err != nil {
		if errors.Is(err, storage.ErrAlertDispatchLeaseExpired) || errors.Is(err, storage.ErrAlertDispatchClaimNotHeld) {
			// Another worker (or a lease timeout) already took over this
			// dispatch; that worker's own completion call is authoritative.
			return
		}
		w.logger.Error("alert dispatch result record failed", "sink", delivery.Dispatch.Sink, "error", err)
	}
}

func (w *Worker) runRetentionLoop(ctx context.Context) {
	w.pruneOnce(ctx)
	ticker := time.NewTicker(w.retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pruneOnce(ctx)
		}
	}
}

func (w *Worker) pruneOnce(ctx context.Context) {
	pruneCtx, cancel := context.WithTimeout(ctx, alertRetentionPruneTimeout)
	defer cancel()
	pruned, err := w.store.PruneTerminalAlertEvents(pruneCtx, w.retentionHorizon, w.retentionBatch)
	if err != nil {
		w.logger.Error("alert retention prune failed", "error", err)
		return
	}
	if pruned > 0 {
		w.logger.Info("alert retention pruned terminal rows", "count", pruned)
	}
}
