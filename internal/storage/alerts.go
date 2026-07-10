package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	AlertEventStatusPending   = "pending"
	AlertEventStatusDelivered = "delivered"
	AlertEventStatusFailed    = "failed"
	AlertEventStatusPartial   = "partial"
	AlertEventStatusReset     = "reset"

	AlertDispatchStatusPending      = "pending"
	AlertDispatchStatusInFlight     = "in_flight"
	AlertDispatchStatusDelivered    = "delivered"
	AlertDispatchStatusDeadLettered = "dead_lettered"
	AlertDispatchStatusReset        = "reset"

	alertSQLiteBusyMaxRetryDuration = 5 * time.Second

	defaultAlertMaxAttempts     = 3
	defaultAlertInitialInterval = 10 * time.Second
	defaultAlertMaxInterval     = 5 * time.Minute

	alertDispatchLeaseExpiredError = "alert dispatch lease expired"
	alertDispatchRetryNoDiagnostic = "retry requested by worker without diagnostic"
	alertDispatchDeadNoDiagnostic  = "dead-lettered by worker without diagnostic"
)

var (
	ErrAlertEventNotFound        = errors.New("alert event not found")
	ErrAlertDispatchNotFound     = errors.New("alert dispatch not found")
	ErrAlertDispatchClaimNotHeld = errors.New("alert dispatch claim not held")
	ErrAlertDispatchLeaseExpired = errors.New(alertDispatchLeaseExpiredError)
	ErrAlertStateLocked          = errors.New("alert state locked: dedupe key missing")
)

type AlertEvent struct {
	ID         int64
	Kind       string
	ServiceID  string
	Target     string
	Repository string
	Agent      string
	OldState   string
	NewState   string
	Reason     string
	DedupeKey  string
	DedupeHash string
	CreatedAt  time.Time
	Status     string
}

type AlertDispatch struct {
	ID             int64
	EventID        int64
	Sink           string
	Status         string
	WorkerID       string
	ClaimID        string
	Attempts       int
	LastError      string
	DeliveredAt    *time.Time
	LeaseExpiresAt *time.Time
	NextAttemptAt  *time.Time
	UpdatedAt      time.Time
}

func (dispatch AlertDispatch) DeliveryKey() string {
	if dispatch.ID <= 0 {
		return ""
	}
	return fmt.Sprint(dispatch.ID)
}

type AlertDelivery struct {
	Event    AlertEvent
	Dispatch AlertDispatch
}

type AlertRetryPolicy struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
}

func DefaultAlertRetryPolicy() AlertRetryPolicy {
	return AlertRetryPolicy{
		MaxAttempts:     defaultAlertMaxAttempts,
		InitialInterval: defaultAlertInitialInterval,
		MaxInterval:     defaultAlertMaxInterval,
	}
}

func (store *Store) EnqueueAlertEvent(ctx context.Context, event AlertEvent, sinks []string, cooldown time.Duration) (AlertEvent, bool, error) {
	if err := store.ensureAlertStateUnlocked(); err != nil {
		return AlertEvent{}, false, err
	}
	result, err := retryAlertSQLiteBusy(ctx, func() (enqueueAlertResult, error) {
		stored, inserted, err := store.enqueueAlertEventOnce(ctx, event, sinks, cooldown)
		return enqueueAlertResult{event: stored, inserted: inserted}, err
	})
	return result.event, result.inserted, err
}

type enqueueAlertResult struct {
	event    AlertEvent
	inserted bool
}

func (store *Store) enqueueAlertEventOnce(ctx context.Context, event AlertEvent, sinks []string, cooldown time.Duration) (AlertEvent, bool, error) {
	rawDedupeKey := strings.TrimSpace(event.DedupeKey)
	event = store.prepareAlertEvent(event)
	if event.Kind == "" {
		return AlertEvent{}, false, fmt.Errorf("alert event kind is required")
	}
	if event.ServiceID == "" && event.Repository == "" && event.Agent == "" {
		return AlertEvent{}, false, fmt.Errorf("alert event requires service_id, repository, or agent")
	}
	if event.NewState == "" {
		return AlertEvent{}, false, fmt.Errorf("alert event new_state is required")
	}
	if rawDedupeKey == "" {
		return AlertEvent{}, false, fmt.Errorf("alert event dedupe_key is required")
	}
	event.DedupeKey = strings.TrimSpace(store.redact(rawDedupeKey))
	event.DedupeHash = store.alertDedupeHash(rawDedupeKey)
	if event.Status == "" {
		event.Status = AlertEventStatusPending
	}
	if event.Status != AlertEventStatusPending {
		return AlertEvent{}, false, fmt.Errorf("alert event status must be empty or pending for enqueue")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	} else {
		event.CreatedAt = event.CreatedAt.UTC()
	}
	sinks, err := store.prepareAlertSinks(sinks)
	if err != nil {
		return AlertEvent{}, false, err
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return AlertEvent{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.ensureAlertDedupeKeyCurrent(ctx, tx); err != nil {
		return AlertEvent{}, false, err
	}

	if existing, ok, err := store.pendingAlertEventForDedupeHash(ctx, tx, event.DedupeHash); err != nil {
		return AlertEvent{}, false, err
	} else if ok {
		if err := store.ensurePendingAlertDispatches(ctx, tx, existing.ID, sinks, event.CreatedAt); err != nil {
			return AlertEvent{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return AlertEvent{}, false, err
		}
		return existing, false, nil
	}
	if existing, suppressed, err := store.cooldownSuppressedAlertEvent(ctx, tx, event.DedupeHash, event.CreatedAt, cooldown); err != nil {
		return AlertEvent{}, false, err
	} else if suppressed {
		return existing, false, nil
	}

	createdAtText := event.CreatedAt.Format(time.RFC3339Nano)
	createdAtNS := event.CreatedAt.UnixNano()
	result, err := tx.ExecContext(ctx, `
INSERT INTO alert_events(
  kind, service_id, target, repository, agent, old_state, new_state, reason,
  dedupe_key, dedupe_hash, created_at, created_at_ns, status
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING
`, event.Kind, event.ServiceID, event.Target, event.Repository, event.Agent, event.OldState, event.NewState, event.Reason, event.DedupeKey, event.DedupeHash, createdAtText, createdAtNS, AlertEventStatusPending)
	if err != nil {
		return AlertEvent{}, false, fmt.Errorf("enqueue alert event %s: %w", event.DedupeKey, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return AlertEvent{}, false, err
	}
	if rowsAffected == 0 {
		existing, ok, err := store.pendingAlertEventForDedupeHash(ctx, tx, event.DedupeHash)
		if err != nil {
			return AlertEvent{}, false, err
		}
		if !ok {
			return AlertEvent{}, false, fmt.Errorf("alert event conflict for dedupe hash %s without pending row", event.DedupeHash)
		}
		if err := store.ensurePendingAlertDispatches(ctx, tx, existing.ID, sinks, event.CreatedAt); err != nil {
			return AlertEvent{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return AlertEvent{}, false, err
		}
		return existing, false, nil
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return AlertEvent{}, false, err
	}
	event.ID = eventID
	event.Status = AlertEventStatusPending
	if err := store.ensurePendingAlertDispatches(ctx, tx, event.ID, sinks, event.CreatedAt); err != nil {
		return AlertEvent{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AlertEvent{}, false, err
	}
	return event, true, nil
}

func (store *Store) ensurePendingAlertDispatches(ctx context.Context, tx *sql.Tx, eventID int64, sinks []string, updatedAt time.Time) error {
	updatedAt = updatedAt.UTC()
	updatedAtText := updatedAt.Format(time.RFC3339Nano)
	updatedAtNS := updatedAt.UnixNano()
	for _, sink := range sinks {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO alert_dispatches(event_id, sink, status, worker_id, claim_id, lease_expires_at, lease_expires_at_ns, next_attempt_at_ns, attempts, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns)
VALUES(?, ?, ?, '', '', NULL, NULL, 0, 0, '', NULL, NULL, ?, ?)
ON CONFLICT(event_id, sink) DO NOTHING
`, eventID, sink, AlertDispatchStatusPending, updatedAtText, updatedAtNS); err != nil {
			return fmt.Errorf("enqueue alert dispatch %d/%s: %w", eventID, sink, err)
		}
	}
	return nil
}

func (store *Store) ListUndeliveredAlertDeliveries(ctx context.Context, limit int) ([]AlertDelivery, error) {
	query := `
SELECT
  e.id, e.kind, e.service_id, e.target, e.repository, e.agent, e.old_state, e.new_state, e.reason, e.dedupe_key, e.dedupe_hash, e.created_at, e.status,
  d.id, d.event_id, d.sink, d.status, d.worker_id, d.claim_id, d.attempts, d.last_error, d.delivered_at, d.lease_expires_at, d.next_attempt_at_ns, d.updated_at
FROM alert_events e
JOIN alert_dispatches d ON d.event_id=e.id
WHERE d.status IN (?, ?)
ORDER BY e.created_at_ns ASC, e.id ASC, d.sink ASC
`
	args := []any{AlertDispatchStatusPending, AlertDispatchStatusInFlight}
	if limit > 0 {
		query += `LIMIT ?`
		args = append(args, limit)
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []AlertDelivery
	for rows.Next() {
		delivery, err := store.scanAlertDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (store *Store) ListUndeliveredAlertEvents(ctx context.Context, limit int) ([]AlertEvent, error) {
	deliveries, err := store.ListUndeliveredAlertDeliveries(ctx, limit)
	if err != nil {
		return nil, err
	}
	seen := map[int64]struct{}{}
	var events []AlertEvent
	for _, delivery := range deliveries {
		if _, ok := seen[delivery.Event.ID]; ok {
			continue
		}
		seen[delivery.Event.ID] = struct{}{}
		events = append(events, delivery.Event)
	}
	return events, nil
}

// ClaimPendingAlertDeliveries atomically leases dispatches for at-least-once delivery.
// Sinks must use AlertDispatch.DeliveryKey() as the idempotency key. The key is
// the dispatch ID, stable across claims and retries for the same event/sink.
// claim_id is only an internal lease/completion token.
func (store *Store) ClaimPendingAlertDeliveries(ctx context.Context, workerID string, leaseDuration time.Duration, limit int) ([]AlertDelivery, error) {
	return store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, workerID, leaseDuration, limit, DefaultAlertRetryPolicy())
}

func (store *Store) ClaimPendingAlertDeliveriesWithRetryPolicy(ctx context.Context, workerID string, leaseDuration time.Duration, limit int, policy AlertRetryPolicy) ([]AlertDelivery, error) {
	if err := store.ensureAlertStateUnlocked(); err != nil {
		return nil, err
	}
	policy = policy.normalized()
	return retryAlertSQLiteBusy(ctx, func() ([]AlertDelivery, error) {
		return store.claimPendingAlertDeliveriesOnce(ctx, workerID, leaseDuration, limit, policy)
	})
}

func (store *Store) claimPendingAlertDeliveriesOnce(ctx context.Context, workerID string, leaseDuration time.Duration, limit int, policy AlertRetryPolicy) ([]AlertDelivery, error) {
	workerID = strings.TrimSpace(store.redact(workerID))
	if workerID == "" {
		return nil, fmt.Errorf("alert claim worker_id is required")
	}
	if leaseDuration <= 0 {
		return nil, fmt.Errorf("alert claim lease duration must be greater than zero")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("alert claim limit must be greater than zero")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.ensureAlertDedupeKeyCurrent(ctx, tx); err != nil {
		return nil, err
	}

	claimID, err := randomAlertToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	nowNS := now.UnixNano()
	leaseExpiresAt := now.Add(leaseDuration)
	leaseExpiresAtText := leaseExpiresAt.Format(time.RFC3339Nano)
	leaseExpiresAtNS := leaseExpiresAt.UnixNano()
	deadLetteredEventIDs, err := store.deadLetterExpiredAlertDispatches(ctx, tx, nowText, nowNS, policy.MaxAttempts)
	if err != nil {
		return nil, err
	}
	for _, eventID := range deadLetteredEventIDs {
		if err := store.syncAlertEventStatus(ctx, tx, eventID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE alert_dispatches
SET status=?,
    worker_id=?,
    claim_id=?,
    lease_expires_at=?,
    lease_expires_at_ns=?,
    next_attempt_at_ns=0,
    attempts=attempts + CASE WHEN status=? THEN 1 ELSE 0 END,
    last_error=CASE WHEN status=? THEN ? ELSE last_error END,
    updated_at=?,
    updated_at_ns=?
WHERE id IN (
  SELECT d.id
  FROM alert_dispatches d
  JOIN alert_events e ON e.id=d.event_id
  WHERE (d.status=? AND COALESCE(d.next_attempt_at_ns, 0) <= ?)
     OR (d.status=? AND COALESCE(d.lease_expires_at_ns, 0) <= ? AND d.attempts + 1 < ?)
  ORDER BY e.created_at_ns ASC, e.id ASC, d.sink ASC
  LIMIT ?
)
`, AlertDispatchStatusInFlight, workerID, claimID, leaseExpiresAtText, leaseExpiresAtNS, AlertDispatchStatusInFlight, AlertDispatchStatusInFlight, alertDispatchLeaseExpiredError, nowText, nowNS, AlertDispatchStatusPending, nowNS, AlertDispatchStatusInFlight, nowNS, policy.MaxAttempts, limit); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT
  e.id, e.kind, e.service_id, e.target, e.repository, e.agent, e.old_state, e.new_state, e.reason, e.dedupe_key, e.dedupe_hash, e.created_at, e.status,
  d.id, d.event_id, d.sink, d.status, d.worker_id, d.claim_id, d.attempts, d.last_error, d.delivered_at, d.lease_expires_at, d.next_attempt_at_ns, d.updated_at
FROM alert_events e
JOIN alert_dispatches d ON d.event_id=e.id
WHERE d.status=? AND d.worker_id=? AND d.claim_id=?
ORDER BY e.created_at_ns ASC, e.id ASC, d.sink ASC
`, AlertDispatchStatusInFlight, workerID, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []AlertDelivery
	for rows.Next() {
		delivery, err := store.scanAlertDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deliveries, nil
}

func (store *Store) deadLetterExpiredAlertDispatches(ctx context.Context, tx *sql.Tx, nowText string, nowNS int64, maxAttempts int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT event_id
FROM alert_dispatches
WHERE status=?
  AND COALESCE(lease_expires_at_ns, 0) <= ?
  AND attempts + 1 >= ?
`, AlertDispatchStatusInFlight, nowNS, maxAttempts)
	if err != nil {
		return nil, err
	}
	var eventIDs []int64
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		eventIDs = append(eventIDs, eventID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(eventIDs) == 0 {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE alert_dispatches
SET attempts=attempts + 1,
    status=?,
    worker_id='',
    claim_id='',
    lease_expires_at=NULL,
    lease_expires_at_ns=NULL,
    next_attempt_at_ns=0,
    last_error=?,
    updated_at=?,
    updated_at_ns=?
WHERE status=?
  AND COALESCE(lease_expires_at_ns, 0) <= ?
  AND attempts + 1 >= ?
`, AlertDispatchStatusDeadLettered, alertDispatchLeaseExpiredError, nowText, nowNS, AlertDispatchStatusInFlight, nowNS, maxAttempts); err != nil {
		return nil, err
	}
	return eventIDs, nil
}

// RecordAlertDispatchResult completes or requeues a claimed at-least-once
// dispatch. Completion is conditional on the claim_id returned by claim.
func (store *Store) RecordAlertDispatchResult(ctx context.Context, dispatchID int64, workerID, claimID, status, lastError string) (AlertDispatch, error) {
	return store.RecordAlertDispatchResultWithRetryPolicy(ctx, dispatchID, workerID, claimID, status, lastError, DefaultAlertRetryPolicy())
}

func (store *Store) RecordAlertDispatchResultWithRetryPolicy(ctx context.Context, dispatchID int64, workerID, claimID, status, lastError string, policy AlertRetryPolicy) (AlertDispatch, error) {
	if err := store.ensureAlertStateUnlocked(); err != nil {
		return AlertDispatch{}, err
	}
	policy = policy.normalized()
	return retryAlertSQLiteBusy(ctx, func() (AlertDispatch, error) {
		return store.recordAlertDispatchResultOnce(ctx, dispatchID, workerID, claimID, status, lastError, policy)
	})
}

func retryAlertSQLiteBusy[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	startedAt := time.Now()
	for attempt := 0; ; attempt++ {
		result, err := fn()
		if err == nil || !isSQLiteBusy(err) {
			return result, err
		}
		lastErr = err
		if time.Since(startedAt) >= alertSQLiteBusyMaxRetryDuration {
			return zero, lastErr
		}
		delay := time.Duration(attempt+1) * 50 * time.Millisecond
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (store *Store) recordAlertDispatchResultOnce(ctx context.Context, dispatchID int64, workerID, claimID, status, lastError string, policy AlertRetryPolicy) (AlertDispatch, error) {
	if dispatchID <= 0 {
		return AlertDispatch{}, fmt.Errorf("alert dispatch id is required")
	}
	workerID = strings.TrimSpace(store.redact(workerID))
	if workerID == "" {
		return AlertDispatch{}, fmt.Errorf("alert dispatch worker_id is required")
	}
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return AlertDispatch{}, fmt.Errorf("alert dispatch claim_id is required")
	}
	status = strings.TrimSpace(status)
	if !validAlertDispatchResultStatus(status) {
		return AlertDispatch{}, fmt.Errorf("alert dispatch result status must be pending, delivered, or dead_lettered")
	}
	lastError = strings.TrimSpace(store.redact(lastError))
	switch {
	case status == AlertDispatchStatusDelivered:
		lastError = ""
	case status == AlertDispatchStatusPending && lastError == "":
		lastError = alertDispatchRetryNoDiagnostic
	case status == AlertDispatchStatusDeadLettered && lastError == "":
		lastError = alertDispatchDeadNoDiagnostic
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return AlertDispatch{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.ensureAlertDedupeKeyCurrent(ctx, tx); err != nil {
		return AlertDispatch{}, err
	}

	var dispatch AlertDispatch
	var scannedDeliveredAt sql.NullString
	var scannedLeaseExpiresAt sql.NullString
	var scannedNextAttemptAtNS int64
	var updatedAt string
	if err := tx.QueryRowContext(ctx, `
SELECT d.id, d.event_id, d.sink, d.status, d.worker_id, d.claim_id, d.attempts, d.last_error, d.delivered_at, d.lease_expires_at, d.next_attempt_at_ns, d.updated_at
FROM alert_dispatches d
JOIN alert_events e ON e.id=d.event_id
WHERE d.id=?
`, dispatchID).Scan(
		&dispatch.ID,
		&dispatch.EventID,
		&dispatch.Sink,
		&dispatch.Status,
		&dispatch.WorkerID,
		&dispatch.ClaimID,
		&dispatch.Attempts,
		&dispatch.LastError,
		&scannedDeliveredAt,
		&scannedLeaseExpiresAt,
		&scannedNextAttemptAtNS,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AlertDispatch{}, ErrAlertDispatchNotFound
		}
		return AlertDispatch{}, err
	}
	dispatch = store.prepareScannedAlertDispatch(dispatch, scannedDeliveredAt, scannedLeaseExpiresAt, scannedNextAttemptAtNS, updatedAt)
	if terminalAlertDispatchStatus(dispatch.Status) {
		if dispatch.WorkerID == workerID && dispatch.ClaimID == claimID {
			if err := tx.Commit(); err != nil {
				return AlertDispatch{}, err
			}
			return dispatch, nil
		}
		return AlertDispatch{}, ErrAlertDispatchClaimNotHeld
	}
	if dispatch.Status == AlertDispatchStatusPending && dispatch.WorkerID == workerID && dispatch.ClaimID == claimID && status == AlertDispatchStatusPending {
		if err := tx.Commit(); err != nil {
			return AlertDispatch{}, err
		}
		return dispatch, nil
	}
	if dispatch.Status != AlertDispatchStatusInFlight || dispatch.WorkerID != workerID || dispatch.ClaimID != claimID {
		return AlertDispatch{}, ErrAlertDispatchClaimNotHeld
	}

	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	nowNS := nowTime.UnixNano()
	nextAttempts := dispatch.Attempts + 1
	resultStatus := status
	if status == AlertDispatchStatusPending && nextAttempts >= policy.MaxAttempts {
		resultStatus = AlertDispatchStatusDeadLettered
		if lastError == "" {
			lastError = "maximum alert dispatch attempts reached"
		}
	}
	var resultDeliveredAt any
	var resultDeliveredAtNS any
	if resultStatus == AlertDispatchStatusDelivered {
		resultDeliveredAt = now
		resultDeliveredAtNS = nowNS
	}
	var leaseExpiresAt any
	var leaseExpiresAtNS any
	nextAttemptAtNS := int64(0)
	storedWorkerID := workerID
	storedClaimID := claimID
	if resultStatus == AlertDispatchStatusPending {
		nextAttemptAtNS = nowTime.Add(policy.backoff(nextAttempts)).UnixNano()
	}
	result, err := tx.ExecContext(ctx, `
UPDATE alert_dispatches
SET attempts=attempts + 1,
    status=?,
    worker_id=?,
    claim_id=?,
    lease_expires_at=?,
    lease_expires_at_ns=?,
    next_attempt_at_ns=?,
    last_error=?,
    delivered_at=?,
    delivered_at_ns=?,
    updated_at=?,
    updated_at_ns=?
WHERE id=? AND status=? AND worker_id=? AND claim_id=?
  AND COALESCE(lease_expires_at_ns, 0)>?
`, resultStatus, storedWorkerID, storedClaimID, leaseExpiresAt, leaseExpiresAtNS, nextAttemptAtNS, lastError, resultDeliveredAt, resultDeliveredAtNS, now, nowNS, dispatchID, AlertDispatchStatusInFlight, workerID, claimID, nowNS)
	if err != nil {
		return AlertDispatch{}, fmt.Errorf("record alert dispatch %d: %w", dispatchID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return AlertDispatch{}, err
	}
	if rowsAffected != 1 {
		expired, err := store.noteExpiredAlertDispatchCompletion(ctx, tx, dispatchID, workerID, claimID, now, nowNS)
		if err != nil {
			return AlertDispatch{}, err
		}
		if expired {
			if err := tx.Commit(); err != nil {
				return AlertDispatch{}, err
			}
			return AlertDispatch{}, ErrAlertDispatchLeaseExpired
		}
		return AlertDispatch{}, ErrAlertDispatchClaimNotHeld
	}
	if err := store.syncAlertEventStatus(ctx, tx, dispatch.EventID); err != nil {
		return AlertDispatch{}, err
	}
	updated, err := store.scanAlertDispatch(tx.QueryRowContext(ctx, `
SELECT id, event_id, sink, status, worker_id, claim_id, attempts, last_error, delivered_at, lease_expires_at, next_attempt_at_ns, updated_at
FROM alert_dispatches
WHERE id=?
`, dispatchID))
	if err != nil {
		return AlertDispatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return AlertDispatch{}, err
	}
	return updated, nil
}

func (store *Store) noteExpiredAlertDispatchCompletion(ctx context.Context, tx *sql.Tx, dispatchID int64, workerID, claimID, now string, nowNS int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `
UPDATE alert_dispatches
SET last_error=?,
    updated_at=?,
    updated_at_ns=?
WHERE id=? AND status=? AND worker_id=? AND claim_id=?
  AND COALESCE(lease_expires_at_ns, 0)<=?
`, alertDispatchLeaseExpiredError, now, nowNS, dispatchID, AlertDispatchStatusInFlight, workerID, claimID, nowNS)
	if err != nil {
		return false, fmt.Errorf("record expired alert dispatch completion %d: %w", dispatchID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rowsAffected == 1, nil
}

func (store *Store) cooldownSuppressedAlertEvent(ctx context.Context, tx *sql.Tx, dedupeHash string, occurrenceAt time.Time, cooldown time.Duration) (AlertEvent, bool, error) {
	if cooldown <= 0 {
		return AlertEvent{}, false, nil
	}
	event, closedAt, ok, err := store.latestCooldownAlertEventForDedupeHash(ctx, tx, dedupeHash)
	if err != nil || !ok {
		return AlertEvent{}, false, err
	}
	if occurrenceAt.Sub(closedAt) < cooldown {
		return event, true, nil
	}
	return AlertEvent{}, false, nil
}

func (store *Store) pendingAlertEventForDedupeHash(ctx context.Context, tx *sql.Tx, dedupeHash string) (AlertEvent, bool, error) {
	event, err := store.scanAlertEvent(tx.QueryRowContext(ctx, `
SELECT e.id, e.kind, e.service_id, e.target, e.repository, e.agent, e.old_state, e.new_state, e.reason, e.dedupe_key, e.dedupe_hash, e.created_at, e.status
FROM alert_events e
WHERE e.dedupe_hash=? AND e.status=?
  AND EXISTS (
    SELECT 1
    FROM alert_dispatches d
    WHERE d.event_id=e.id AND d.status IN (?, ?)
  )
ORDER BY e.created_at_ns DESC, e.id DESC
LIMIT 1
`, dedupeHash, AlertEventStatusPending, AlertDispatchStatusPending, AlertDispatchStatusInFlight))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AlertEvent{}, false, nil
		}
		return AlertEvent{}, false, err
	}
	return event, true, nil
}

func (store *Store) latestCooldownAlertEventForDedupeHash(ctx context.Context, tx *sql.Tx, dedupeHash string) (AlertEvent, time.Time, bool, error) {
	var closedAtNS int64
	event, err := store.scanAlertEventWithExtraInt64(tx.QueryRowContext(ctx, `
SELECT
  e.id, e.kind, e.service_id, e.target, e.repository, e.agent, e.old_state, e.new_state, e.reason, e.dedupe_key, e.dedupe_hash, e.created_at, e.status,
  MAX(COALESCE(d.delivered_at_ns, d.updated_at_ns, 0)) AS closed_at_ns
FROM alert_events e
JOIN alert_dispatches d ON d.event_id=e.id
WHERE e.dedupe_hash=? AND e.status IN (?, ?)
GROUP BY e.id
HAVING SUM(CASE WHEN d.status IN (?, ?) THEN 1 ELSE 0 END)=0
   AND MAX(COALESCE(d.delivered_at_ns, d.updated_at_ns, 0)) > 0
ORDER BY closed_at_ns DESC, e.id DESC
LIMIT 1
`, dedupeHash, AlertEventStatusDelivered, AlertEventStatusPartial, AlertDispatchStatusPending, AlertDispatchStatusInFlight), &closedAtNS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AlertEvent{}, time.Time{}, false, nil
		}
		return AlertEvent{}, time.Time{}, false, err
	}
	return event, time.Unix(0, closedAtNS).UTC(), true, nil
}

func (store *Store) syncAlertEventStatus(ctx context.Context, tx *sql.Tx, eventID int64) error {
	var pending, delivered, deadLettered, reset int
	if err := tx.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(CASE WHEN status IN (?, ?) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0)
FROM alert_dispatches
WHERE event_id=?
`, AlertDispatchStatusPending, AlertDispatchStatusInFlight, AlertDispatchStatusDelivered, AlertDispatchStatusDeadLettered, AlertDispatchStatusReset, eventID).Scan(&pending, &delivered, &deadLettered, &reset); err != nil {
		return err
	}
	status := AlertEventStatusPending
	if pending == 0 {
		switch {
		case delivered > 0 && deadLettered == 0:
			status = AlertEventStatusDelivered
		case delivered > 0 && deadLettered > 0:
			status = AlertEventStatusPartial
		case delivered == 0 && deadLettered > 0:
			status = AlertEventStatusFailed
		case delivered == 0 && deadLettered == 0 && reset > 0:
			status = AlertEventStatusReset
		default:
			status = AlertEventStatusFailed
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE alert_events SET status=? WHERE id=?`, status, eventID); err != nil {
		return fmt.Errorf("update alert event status %d: %w", eventID, err)
	}
	return nil
}

func (store *Store) alertDispatchByID(ctx context.Context, dispatchID int64) (AlertDispatch, error) {
	return store.scanAlertDispatch(store.db.QueryRowContext(ctx, `
SELECT id, event_id, sink, status, worker_id, claim_id, attempts, last_error, delivered_at, lease_expires_at, next_attempt_at_ns, updated_at
FROM alert_dispatches
WHERE id=?
`, dispatchID))
}

func (store *Store) ensureAlertStateUnlocked() error {
	if store == nil {
		return nil
	}
	store.alertStateMu.RLock()
	defer store.alertStateMu.RUnlock()
	if !store.alertStateLocked {
		return nil
	}
	if store.alertStateLockMsg != "" {
		return fmt.Errorf("%w; %s", ErrAlertStateLocked, store.alertStateLockMsg)
	}
	return ErrAlertStateLocked
}

func (store *Store) ensureAlertDedupeKeyCurrent(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	if len(store.alertDedupeKey) == 0 {
		message := "alert state locked: alert dedupe key is unavailable"
		store.lockAlertState(message)
		return fmt.Errorf("%w; %s", ErrAlertStateLocked, message)
	}
	key, err := readAlertDedupeKey(store.alertDedupeKeyPath)
	if err != nil || alertDedupeKeyFingerprint(key) != alertDedupeKeyFingerprint(store.alertDedupeKey) {
		message := "alert state locked: alert dedupe key sidecar changed or is unavailable while this process was running; restore it and restart all dashboard processes before writing alert state"
		store.lockAlertState(message)
		return fmt.Errorf("%w; %s", ErrAlertStateLocked, message)
	}
	var fingerprint string
	err = queryer.QueryRowContext(ctx, `SELECT value FROM storage_metadata WHERE key=?`, alertDedupeKeyFingerprintMetadata).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		message := "alert state locked: alert dedupe key fingerprint is missing; restart all dashboard processes after repairing alert state"
		store.lockAlertState(message)
		return fmt.Errorf("%w; %s", ErrAlertStateLocked, message)
	}
	if err != nil {
		return err
	}
	if fingerprint != alertDedupeKeyFingerprint(store.alertDedupeKey) {
		message := "alert state locked: alert dedupe key fingerprint changed while this process was running; restart all dashboard processes before writing alert state"
		store.lockAlertState(message)
		return fmt.Errorf("%w; %s", ErrAlertStateLocked, message)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (store *Store) scanAlertDelivery(scanner rowScanner) (AlertDelivery, error) {
	var event AlertEvent
	var dispatch AlertDispatch
	var createdAt string
	var deliveredAt sql.NullString
	var leaseExpiresAt sql.NullString
	var nextAttemptAtNS int64
	var updatedAt string
	if err := scanner.Scan(
		&event.ID,
		&event.Kind,
		&event.ServiceID,
		&event.Target,
		&event.Repository,
		&event.Agent,
		&event.OldState,
		&event.NewState,
		&event.Reason,
		&event.DedupeKey,
		&event.DedupeHash,
		&createdAt,
		&event.Status,
		&dispatch.ID,
		&dispatch.EventID,
		&dispatch.Sink,
		&dispatch.Status,
		&dispatch.WorkerID,
		&dispatch.ClaimID,
		&dispatch.Attempts,
		&dispatch.LastError,
		&deliveredAt,
		&leaseExpiresAt,
		&nextAttemptAtNS,
		&updatedAt,
	); err != nil {
		return AlertDelivery{}, err
	}
	event = store.prepareAlertEvent(event)
	if parsed, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		event.CreatedAt = parsed
	}
	dispatch = store.prepareScannedAlertDispatch(dispatch, deliveredAt, leaseExpiresAt, nextAttemptAtNS, updatedAt)
	return AlertDelivery{Event: event, Dispatch: dispatch}, nil
}

func (store *Store) scanAlertEvent(scanner rowScanner) (AlertEvent, error) {
	var event AlertEvent
	var createdAt string
	if err := scanner.Scan(
		&event.ID,
		&event.Kind,
		&event.ServiceID,
		&event.Target,
		&event.Repository,
		&event.Agent,
		&event.OldState,
		&event.NewState,
		&event.Reason,
		&event.DedupeKey,
		&event.DedupeHash,
		&createdAt,
		&event.Status,
	); err != nil {
		return AlertEvent{}, err
	}
	event = store.prepareAlertEvent(event)
	if parsed, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		event.CreatedAt = parsed
	}
	return event, nil
}

func (store *Store) scanAlertEventWithExtraInt64(scanner rowScanner, extra *int64) (AlertEvent, error) {
	var event AlertEvent
	var createdAt string
	if err := scanner.Scan(
		&event.ID,
		&event.Kind,
		&event.ServiceID,
		&event.Target,
		&event.Repository,
		&event.Agent,
		&event.OldState,
		&event.NewState,
		&event.Reason,
		&event.DedupeKey,
		&event.DedupeHash,
		&createdAt,
		&event.Status,
		extra,
	); err != nil {
		return AlertEvent{}, err
	}
	event = store.prepareAlertEvent(event)
	if parsed, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		event.CreatedAt = parsed
	}
	return event, nil
}

func (store *Store) scanAlertDispatch(scanner rowScanner) (AlertDispatch, error) {
	var dispatch AlertDispatch
	var deliveredAt sql.NullString
	var leaseExpiresAt sql.NullString
	var nextAttemptAtNS int64
	var updatedAt string
	if err := scanner.Scan(
		&dispatch.ID,
		&dispatch.EventID,
		&dispatch.Sink,
		&dispatch.Status,
		&dispatch.WorkerID,
		&dispatch.ClaimID,
		&dispatch.Attempts,
		&dispatch.LastError,
		&deliveredAt,
		&leaseExpiresAt,
		&nextAttemptAtNS,
		&updatedAt,
	); err != nil {
		return AlertDispatch{}, err
	}
	return store.prepareScannedAlertDispatch(dispatch, deliveredAt, leaseExpiresAt, nextAttemptAtNS, updatedAt), nil
}

func (store *Store) prepareScannedAlertDispatch(dispatch AlertDispatch, deliveredAt, leaseExpiresAt sql.NullString, nextAttemptAtNS int64, updatedAt string) AlertDispatch {
	dispatch.Sink = strings.TrimSpace(dispatch.Sink)
	dispatch.Status = strings.TrimSpace(dispatch.Status)
	dispatch.WorkerID = strings.TrimSpace(store.redact(dispatch.WorkerID))
	dispatch.ClaimID = strings.TrimSpace(dispatch.ClaimID)
	dispatch.LastError = strings.TrimSpace(store.redact(dispatch.LastError))
	if deliveredAt.Valid {
		if parsed, err := time.Parse(time.RFC3339Nano, deliveredAt.String); err == nil {
			dispatch.DeliveredAt = &parsed
		}
	}
	if leaseExpiresAt.Valid {
		if parsed, err := time.Parse(time.RFC3339Nano, leaseExpiresAt.String); err == nil {
			dispatch.LeaseExpiresAt = &parsed
		}
	}
	if nextAttemptAtNS > 0 {
		nextAttemptAt := time.Unix(0, nextAttemptAtNS).UTC()
		dispatch.NextAttemptAt = &nextAttemptAt
	}
	if parsed, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		dispatch.UpdatedAt = parsed
	}
	return dispatch
}

func (store *Store) prepareAlertEvent(event AlertEvent) AlertEvent {
	event.Kind = strings.TrimSpace(store.redact(event.Kind))
	event.ServiceID = strings.TrimSpace(store.redact(event.ServiceID))
	event.Target = strings.TrimSpace(store.redact(event.Target))
	event.Repository = strings.TrimSpace(store.redact(event.Repository))
	event.Agent = strings.TrimSpace(store.redact(event.Agent))
	event.OldState = strings.TrimSpace(store.redact(event.OldState))
	event.NewState = strings.TrimSpace(store.redact(event.NewState))
	event.Reason = strings.TrimSpace(store.redact(event.Reason))
	event.DedupeKey = strings.TrimSpace(store.redact(event.DedupeKey))
	event.DedupeHash = strings.TrimSpace(event.DedupeHash)
	event.Status = strings.TrimSpace(event.Status)
	return event
}

func (store *Store) prepareAlertSinks(sinks []string) ([]string, error) {
	seen := map[string]struct{}{}
	prepared := make([]string, 0, len(sinks))
	for _, sink := range sinks {
		sink = strings.TrimSpace(sink)
		if sink == "" {
			return nil, fmt.Errorf("alert event sinks must not contain empty values")
		}
		if err := store.validateAlertSinkIdentity(sink); err != nil {
			return nil, err
		}
		if _, ok := seen[sink]; ok {
			continue
		}
		seen[sink] = struct{}{}
		prepared = append(prepared, sink)
	}
	if len(prepared) == 0 {
		return nil, fmt.Errorf("alert event requires at least one sink")
	}
	return prepared, nil
}

func (store *Store) validateAlertSinkIdentity(sink string) error {
	if store.redact(sink) != sink {
		return fmt.Errorf("alert sink identity contains a registered secret")
	}
	if store.alertSinkAllowlist {
		if _, ok := store.alertSinkNames[sink]; !ok {
			return fmt.Errorf("alert sink identity %q is not configured", sink)
		}
	}
	return nil
}

func validAlertEventStatus(status string) bool {
	return status == AlertEventStatusPending ||
		status == AlertEventStatusDelivered ||
		status == AlertEventStatusFailed ||
		status == AlertEventStatusPartial ||
		status == AlertEventStatusReset
}

func validAlertDispatchStatus(status string) bool {
	return status == AlertDispatchStatusPending ||
		status == AlertDispatchStatusInFlight ||
		status == AlertDispatchStatusReset ||
		terminalAlertDispatchStatus(status)
}

func validAlertDispatchResultStatus(status string) bool {
	return status == AlertDispatchStatusPending || terminalAlertDispatchStatus(status)
}

func terminalAlertDispatchStatus(status string) bool {
	return status == AlertDispatchStatusDelivered || status == AlertDispatchStatusDeadLettered
}

func (policy AlertRetryPolicy) normalized() AlertRetryPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = defaultAlertMaxAttempts
	}
	if policy.InitialInterval <= 0 {
		policy.InitialInterval = defaultAlertInitialInterval
	}
	if policy.MaxInterval <= 0 {
		policy.MaxInterval = defaultAlertMaxInterval
	}
	if policy.MaxInterval < policy.InitialInterval {
		policy.MaxInterval = policy.InitialInterval
	}
	return policy
}

func (policy AlertRetryPolicy) backoff(attempts int) time.Duration {
	policy = policy.normalized()
	if attempts <= 1 {
		return policy.InitialInterval
	}
	backoff := policy.InitialInterval
	for i := 1; i < attempts; i++ {
		if backoff >= policy.MaxInterval {
			return policy.MaxInterval
		}
		if backoff > policy.MaxInterval/2 {
			return policy.MaxInterval
		}
		backoff *= 2
	}
	if backoff > policy.MaxInterval {
		return policy.MaxInterval
	}
	return backoff
}

func (store *Store) alertDedupeHash(raw string) string {
	if len(store.alertDedupeKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, store.alertDedupeKey)
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomAlertToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate alert claim id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") ||
		strings.Contains(text, "database table is locked") ||
		strings.Contains(text, "sqlite_busy") ||
		strings.Contains(text, "busy")
}
