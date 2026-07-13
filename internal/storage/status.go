package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
)

// SetStatusTTL configures the freshness window for a monitor target. The
// monitor derives this from its configured work cadence; legacy callers that
// do not register a target keep their existing non-expiring compatibility.
func (store *Store) SetStatusTTL(target string, ttl time.Duration) {
	target = canonicalStatusTarget(target)
	if target == "" || ttl <= 0 {
		return
	}
	store.freshnessMu.Lock()
	if store.statusTTLs == nil {
		store.statusTTLs = map[string]time.Duration{}
	}
	store.statusTTLs[target] = ttl
	store.freshnessMu.Unlock()
}

func (store *Store) statusTTL(target string) time.Duration {
	target = canonicalStatusTarget(target)
	store.freshnessMu.RLock()
	defer store.freshnessMu.RUnlock()
	if ttl := store.statusTTLs[target]; ttl > 0 {
		return ttl
	}
	if parent, _, ok := strings.Cut(target, ": "); ok {
		return store.statusTTLs[parent]
	}
	return 0
}

func (store *Store) StatusResults(ctx context.Context) ([]core.StatusResult, error) {
	parentRouteOverrides, err := store.parentRouteOverrideServices(ctx)
	if err != nil {
		return nil, err
	}
	activeOverrides, err := store.activeMonitorOverrides(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
SELECT rowid, service_id, target, health, message, checked_at, expires_at, observed_images_json
FROM status_results ORDER BY checked_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []core.StatusResult
	for rows.Next() {
		var status core.StatusResult
		var rowID int64
		var health, checkedAt, expiresAt, observedImages string
		if err := rows.Scan(&rowID, &status.ServiceID, &status.Target, &health, &status.Message, &checkedAt, &expiresAt, &observedImages); err != nil {
			return nil, err
		}
		status.Health = core.HealthState(health)
		status.Message = store.redact(status.Message)
		if err := store.fromPersistedJSON(observedImages, &status.ObservedImages, "status_results", "observed_images_json", rowID, statusResultJSONKey(status.ServiceID, status.Target)); err != nil {
			return nil, err
		}
		if status.ObservedImages == nil {
			status.ObservedImages = []core.ObservedImage{}
		}
		if parsed, err := time.Parse(time.RFC3339, checkedAt); err == nil {
			status.CheckedAt = parsed
		}
		if parsed, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			status.ExpiresAt = parsed
		} else if !status.CheckedAt.IsZero() {
			// Pre-T-032 rows retain their data but gain the same configured
			// freshness policy as new rows after an upgrade.
			basis := status.CheckedAt
			// Legacy agent reports used agent-controlled checked_at. Prefer the
			// server receipt to prevent a preserved future clock from extending
			// health indefinitely after upgrade.
			var received, staleAfter string
			agentBacked := false
			if err := store.db.QueryRowContext(ctx, `SELECT last_seen_at, stale_after FROM agents WHERE target=?`, status.Target).Scan(&received, &staleAfter); err == nil {
				agentBacked = true
				if seen, parseErr := time.Parse(time.RFC3339, received); parseErr == nil {
					basis = seen
				}
			}
			if parsed, parseErr := time.Parse(time.RFC3339, staleAfter); parseErr == nil {
				status.ExpiresAt = parsed
			} else if ttl := store.statusTTL(status.Target); ttl > 0 {
				status.ExpiresAt = basis.Add(ttl)
			} else if agentBacked && !status.CheckedAt.Equal(basis) {
				// A removed/renamed agent has no current configured reporting
				// policy. A historical projection (whose agent-controlled
				// checked_at differs from the server receipt) must not remain
				// healthy. Newly accepted compatibility reports use receipt time
				// and retain their pre-T-032 non-expiring behavior.
				status.ExpiresAt = basis
			}
		}
		if !status.ExpiresAt.IsZero() {
			if !time.Now().UTC().Before(status.ExpiresAt) && status.Health != core.HealthNotApplicable {
				status.Health = core.HealthUnknown
				status.Message = "monitor result is stale; waiting for a current check"
				status.ObservedImages = []core.ObservedImage{}
				// This is a materialized stale view, not a future cache boundary.
				status.ExpiresAt = time.Time{}
			}
		}
		if parentRouteOverrides[status.ServiceID] && strings.HasPrefix(status.Target, routeTargetPrefix) {
			continue
		}
		if activeOverrides[status.ServiceID+"\x00"+status.Target] {
			status.Health = core.HealthNotApplicable
			status.Message = "not applicable"
			status.ObservedImages = []core.ObservedImage{}
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func (store *Store) activeMonitorOverrides(ctx context.Context) (map[string]bool, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT service_id, target FROM monitor_overrides WHERE not_applicable=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overrides := map[string]bool{}
	for rows.Next() {
		var serviceID, target string
		if err := rows.Scan(&serviceID, &target); err != nil {
			return nil, err
		}
		overrides[serviceID+"\x00"+target] = true
	}
	return overrides, rows.Err()
}

func (store *Store) SetMonitorNotApplicable(ctx context.Context, serviceID, target string, notApplicable bool) error {
	serviceID = strings.TrimSpace(serviceID)
	target = strings.TrimSpace(target)
	if serviceID == "" || target == "" {
		return fmt.Errorf("serviceId and target are required")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	resolved, err := resolveMonitorOverrideTarget(ctx, store, tx, serviceID, target)
	if err != nil {
		return err
	}
	target = resolved.target

	var exists int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM status_results WHERE service_id=? AND target=?
`, serviceID, target).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if !notApplicable {
			return ErrStatusNotFound
		}
		if !resolved.syntheticRouteTarget {
			return ErrStatusNotFound
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if notApplicable {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at)
VALUES(?, ?, 1, ?)
ON CONFLICT(service_id, target) DO UPDATE SET not_applicable=1, updated_at=excluded.updated_at
`, serviceID, target, now); err != nil {
			return fmt.Errorf("set monitor override %s/%s: %w", serviceID, target, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at,
  expires_at='',
  observed_images_json='[]'
`, serviceID, target, string(core.HealthNotApplicable), "not applicable", now); err != nil {
			return fmt.Errorf("update status override %s/%s: %w", serviceID, target, err)
		}
		if resolved.syntheticRouteParent {
			if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target LIKE ?`, serviceID, routeTargetPrefix+"%"); err != nil {
				return fmt.Errorf("clear child route statuses %s/%s: %w", serviceID, target, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target LIKE ?`, serviceID, routeTargetPrefix+"%"); err != nil {
				return fmt.Errorf("clear child route history %s/%s: %w", serviceID, target, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM status_history WHERE service_id=? AND target=?
`, serviceID, target); err != nil {
			return fmt.Errorf("clear ignored status history %s/%s: %w", serviceID, target, err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM monitor_overrides WHERE service_id=? AND target=?`, serviceID, target); err != nil {
			return fmt.Errorf("delete monitor override %s/%s: %w", serviceID, target, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE status_results
SET health=?, message=?, checked_at=?
WHERE service_id=? AND target=? AND health=?
`, string(core.HealthUnknown), "monitor enabled; waiting for next check", now, serviceID, target, string(core.HealthNotApplicable)); err != nil {
			return fmt.Errorf("reset status override %s/%s: %w", serviceID, target, err)
		}
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.observeHealthAlert(ctx, serviceID, time.Now().UTC())
	return nil
}

type resolvedMonitorOverrideTarget struct {
	target               string
	syntheticRouteTarget bool
	syntheticRouteParent bool
}

func resolveMonitorOverrideTarget(ctx context.Context, store *Store, tx *sql.Tx, serviceID, target string) (resolvedMonitorOverrideTarget, error) {
	resolved := resolvedMonitorOverrideTarget{target: strings.TrimSpace(target)}
	routes, err := serviceMonitorRoutes(ctx, store, tx, serviceID)
	if err != nil {
		return resolved, err
	}
	if resolved.target == routeMonitorTarget {
		if len(routes) > 0 {
			resolved.syntheticRouteTarget = true
			resolved.syntheticRouteParent = true
		}
		return resolved, nil
	}
	route, ok := routetarget.RouteFromTarget(resolved.target)
	if !ok {
		return resolved, nil
	}
	resolved.target = routetarget.Target(route)
	for _, configuredRoute := range routes {
		if configuredRoute == route {
			resolved.target = routetarget.Target(configuredRoute)
			resolved.syntheticRouteTarget = true
			return resolved, nil
		}
	}
	return resolved, nil
}

type serviceQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func serviceMonitorRoutes(ctx context.Context, store *Store, queryer serviceQuerier, serviceID string) ([]string, error) {
	var rowID int64
	var exposureJSON string
	err := queryer.QueryRowContext(ctx, `SELECT rowid, exposure_json FROM services WHERE id=?`, serviceID).Scan(&rowID, &exposureJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var exposure []string
	if err := store.fromPersistedJSON(exposureJSON, &exposure, "services", "exposure_json", rowID, serviceID); err != nil {
		return nil, err
	}
	return monitorRoutesFromExposure(exposure), nil
}

func monitorRoutesFromExposure(exposure []string) []string {
	return routetarget.Routes(exposure)
}

func (store *Store) MonitorNotApplicable(ctx context.Context, serviceID, target string) (bool, error) {
	serviceID = strings.TrimSpace(serviceID)
	target = canonicalStatusTarget(target)
	if serviceID == "" || target == "" {
		return false, nil
	}
	exact, parent, err := monitorOverrideState(ctx, store, store.db, serviceID, target)
	if err != nil {
		return false, err
	}
	return exact || parent, nil
}

func (store *Store) PruneStatusTargets(ctx context.Context, serviceID, exactTarget, prefix string, keep []string) error {
	keepTargets := map[string]struct{}{}
	for _, target := range keep {
		keepTargets[target] = struct{}{}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT target FROM status_results WHERE service_id=?`, serviceID)
	if err != nil {
		return err
	}
	var removeTargets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			_ = rows.Close()
			return err
		}
		if target != exactTarget && (prefix == "" || !strings.HasPrefix(target, prefix)) {
			continue
		}
		if _, ok := keepTargets[target]; ok {
			continue
		}
		removeTargets = append(removeTargets, target)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, target := range removeTargets {
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target=?`, serviceID, target); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target=?`, serviceID, target); err != nil {
			return err
		}
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.observeHealthAlert(ctx, serviceID, time.Now().UTC())
	return nil
}

type RouteMonitorLookup struct {
	Overrides     map[string]struct{}
	StatusTargets map[string]struct{}
}

func (store *Store) RouteMonitorLookup(ctx context.Context, serviceIDs []string, exactTarget, targetPrefix string) (map[string]RouteMonitorLookup, error) {
	serviceIDs = dedupeStrings(serviceIDs)
	if len(serviceIDs) == 0 {
		return map[string]RouteMonitorLookup{}, nil
	}

	result := make(map[string]RouteMonitorLookup, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		result[serviceID] = RouteMonitorLookup{
			Overrides:     map[string]struct{}{},
			StatusTargets: map[string]struct{}{},
		}
	}

	for _, batch := range chunkStrings(serviceIDs, routeMonitorLookupBatchSize) {
		if err := store.routeMonitorLookupChunk(ctx, batch, exactTarget, targetPrefix, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (store *Store) routeMonitorLookupChunk(ctx context.Context, serviceIDs []string, exactTarget, targetPrefix string, result map[string]RouteMonitorLookup) error {
	placeholders := sqlPlaceholders(len(serviceIDs))

	overrideArgs := make([]any, 0, len(serviceIDs)+2)
	for _, serviceID := range serviceIDs {
		overrideArgs = append(overrideArgs, serviceID)
	}
	overrideArgs = append(overrideArgs, exactTarget, targetPrefix+"%")
	overrideRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target
FROM monitor_overrides
WHERE service_id IN (`+placeholders+`) AND not_applicable = 1
  AND (target = ? OR target LIKE ?)
`, overrideArgs...)
	if err != nil {
		return err
	}
	for overrideRows.Next() {
		var serviceID, target string
		if err := overrideRows.Scan(&serviceID, &target); err != nil {
			_ = overrideRows.Close()
			return err
		}
		state, ok := result[serviceID]
		if !ok {
			continue
		}
		state.Overrides[target] = struct{}{}
		result[serviceID] = state
	}
	if err := overrideRows.Close(); err != nil {
		return err
	}

	statusArgs := make([]any, 0, len(serviceIDs)+2)
	for _, serviceID := range serviceIDs {
		statusArgs = append(statusArgs, serviceID)
	}
	statusArgs = append(statusArgs, exactTarget, targetPrefix+"%")
	statusRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target
FROM status_results
WHERE service_id IN (`+placeholders+`) AND (target = ? OR target LIKE ?)
`, statusArgs...)
	if err != nil {
		return err
	}
	for statusRows.Next() {
		var serviceID, target string
		if err := statusRows.Scan(&serviceID, &target); err != nil {
			_ = statusRows.Close()
			return err
		}
		state, ok := result[serviceID]
		if !ok {
			continue
		}
		state.StatusTargets[target] = struct{}{}
		result[serviceID] = state
	}
	if err := statusRows.Close(); err != nil {
		return err
	}
	return nil
}

func (store *Store) PruneStatusTargetsFromKnown(ctx context.Context, serviceID, exactTarget, prefix string, keep []string, statusTargets map[string]struct{}, keepExact bool) error {
	keepTargets := make(map[string]struct{}, len(keep))
	for _, target := range keep {
		keepTargets[target] = struct{}{}
	}

	// Ambiguity exclusions are consulted only for candidates this call would
	// otherwise remove. The leading service_id index keeps this bounded even for
	// large history tables.
	excludedRoutes := map[string]struct{}{}
	if prefix != "" {
		candidateRoutes := make([]string, 0, len(statusTargets))
		for target := range statusTargets {
			if strings.HasPrefix(target, prefix) {
				candidateRoutes = append(candidateRoutes, strings.TrimPrefix(target, prefix))
			}
		}
		if len(candidateRoutes) > 0 {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(candidateRoutes)), ",")
			args := make([]any, 0, len(candidateRoutes)+1)
			args = append(args, serviceID)
			for _, route := range candidateRoutes {
				args = append(args, route)
			}
			rows, err := store.db.QueryContext(ctx, `SELECT old_route FROM route_target_exclusions WHERE service_id=? AND old_route IN (`+placeholders+`)`, args...)
			if err != nil {
				return fmt.Errorf("query route target exclusions: %w", err)
			}
			for rows.Next() {
				var route string
				if err := rows.Scan(&route); err != nil {
					_ = rows.Close()
					return err
				}
				excludedRoutes[route] = struct{}{}
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
	}

	removeTargets := make([]string, 0, len(statusTargets))
	for target := range statusTargets {
		if target == exactTarget && exactTarget != "" {
			if keepExact {
				continue
			}
			removeTargets = append(removeTargets, target)
			continue
		}
		if prefix == "" || !strings.HasPrefix(target, prefix) {
			continue
		}
		if _, keep := keepTargets[target]; keep {
			continue
		}
		if _, excluded := excludedRoutes[strings.TrimPrefix(target, prefix)]; excluded {
			continue
		}
		removeTargets = append(removeTargets, target)
	}
	if len(removeTargets) == 0 {
		return nil
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteByServiceAndTargets(ctx, tx, "status_results", serviceID, removeTargets); err != nil {
		return err
	}
	if err := deleteByServiceAndTargets(ctx, tx, "status_history", serviceID, removeTargets); err != nil {
		return err
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.observeHealthAlert(ctx, serviceID, time.Now().UTC())
	return nil
}

func applyLatestStatus(services []core.Service, statuses []core.StatusResult) {
	latestByTarget := map[string]map[string]core.StatusResult{}
	for _, status := range statuses {
		targets, ok := latestByTarget[status.ServiceID]
		if !ok {
			targets = map[string]core.StatusResult{}
			latestByTarget[status.ServiceID] = targets
		}
		current, ok := targets[status.Target]
		if !ok || status.CheckedAt.After(current.CheckedAt) || (status.CheckedAt.Equal(current.CheckedAt) && healthPriority(status.Health) < healthPriority(current.Health)) {
			targets[status.Target] = status
		}
	}
	for i := range services {
		targets, ok := latestByTarget[services[i].ID]
		if !ok {
			continue
		}
		statuses := make([]core.StatusResult, 0, len(targets))
		for _, status := range targets {
			statuses = append(statuses, status)
		}
		services[i].Health = aggregateTargetHealth(statuses)
	}
}

func aggregateTargetHealth(statuses []core.StatusResult) core.HealthState {
	applicable := make([]core.StatusResult, 0, len(statuses))
	for _, status := range statuses {
		if status.Health != core.HealthNotApplicable {
			applicable = append(applicable, status)
		}
	}
	statuses = applicable
	if len(statuses) == 0 {
		return core.HealthUnknown
	}
	if len(statuses) == 1 {
		return statuses[0].Health
	}
	allHealthy := true
	anyHealthy := false
	allUnknown := true
	worst := core.HealthUnknown
	for _, status := range statuses {
		if status.Health == core.HealthHealthy {
			anyHealthy = true
		} else {
			allHealthy = false
		}
		if status.Health != core.HealthUnknown {
			allUnknown = false
		}
		if healthPriority(status.Health) < healthPriority(worst) {
			worst = status.Health
		}
	}
	if allHealthy {
		return core.HealthHealthy
	}
	if anyHealthy {
		return core.HealthDegraded
	}
	if allUnknown {
		return core.HealthUnknown
	}
	return worst
}

func healthPriority(health core.HealthState) int {
	switch health {
	case core.HealthError:
		return 0
	case core.HealthUnhealthy:
		return 1
	case core.HealthDegraded:
		return 2
	case core.HealthHealthy:
		return 3
	case core.HealthNotApplicable:
		return 5
	default:
		return 4
	}
}

func (store *Store) UpsertStatus(ctx context.Context, status core.StatusResult) error {
	status.ServiceID = strings.TrimSpace(status.ServiceID)
	status.Target = canonicalStatusTarget(status.Target)
	if status.ObservedImages == nil {
		status.ObservedImages = []core.ObservedImage{}
	}
	if status.ExpiresAt.IsZero() {
		ttl := store.statusTTL(status.Target)
		if ttl > 0 && !status.CheckedAt.IsZero() {
			status.ExpiresAt = status.CheckedAt.UTC().Add(ttl)
		}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	exactOverride, parentOverride, err := monitorOverrideState(ctx, store, tx, status.ServiceID, status.Target)
	if err != nil {
		return err
	}
	if parentOverride && routetarget.IsChildTarget(status.Target) {
		return tx.Commit()
	}
	if exactOverride {
		status.Health = core.HealthNotApplicable
		status.Message = "not applicable"
		status.ObservedImages = []core.ObservedImage{}
		status.ExpiresAt = time.Time{}
	}
	checkedAt := status.CheckedAt.UTC().Format(time.RFC3339)
	expiresAt := ""
	if !status.ExpiresAt.IsZero() {
		expiresAt = status.ExpiresAt.UTC().Format(time.RFC3339)
	}
	message := store.redact(status.Message)
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at, expires_at, observed_images_json)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at, expires_at=excluded.expires_at,
  observed_images_json=excluded.observed_images_json
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt, expiresAt, toJSON(status.ObservedImages))
	if err != nil {
		return fmt.Errorf("upsert status %s/%s: %w", status.ServiceID, status.Target, err)
	}
	if status.Health == core.HealthNotApplicable {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM status_history WHERE service_id=? AND target=?
`, status.ServiceID, status.Target); err != nil {
			return fmt.Errorf("clear not applicable status history %s/%s: %w", status.ServiceID, status.Target, err)
		}
		if err := store.commitAndInvalidateSummary(tx); err != nil {
			return err
		}
		store.observeHealthAlert(ctx, status.ServiceID, status.CheckedAt.UTC())
		return nil
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_history(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt)
	if err != nil {
		return fmt.Errorf("insert status history %s/%s: %w", status.ServiceID, status.Target, err)
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.observeHealthAlert(ctx, status.ServiceID, status.CheckedAt.UTC())
	return nil
}

// observeHealthAlert is deliberately outside the monitoring transaction. A
// canceled or unavailable alert store must never roll back a monitor result;
// the alert state and outbox event themselves remain one transaction.
func (store *Store) observeHealthAlert(ctx context.Context, serviceID string, observedAt time.Time) {
	if err := store.produceHealthAlert(ctx, serviceID, observedAt); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrAlertStateLocked) {
		store.logger.Warn("health alert production deferred", "service", store.redact(serviceID), "error", store.redact(err.Error()))
	}
}

func (store *Store) observeHealthAlerts(ctx context.Context, serviceIDs []string, observedAt time.Time) {
	for _, serviceID := range serviceIDs {
		store.observeHealthAlert(ctx, serviceID, observedAt)
	}
}

func (store *Store) produceHealthAlert(ctx context.Context, serviceID string, observedAt time.Time) error {
	config := store.healthAlerts
	if !config.Enabled || store.isAlertStateLocked() {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var serviceIncarnation string
	if err := tx.QueryRowContext(ctx, `SELECT incarnation FROM services WHERE id=?`, serviceID).Scan(&serviceIncarnation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Status-only callers predate inventory rows. Keep their producer
			// behavior stable, while inventory-backed services always use their
			// durable incarnation below.
			serviceIncarnation = "legacy:" + serviceID
		} else {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT target, health, checked_at FROM status_results WHERE service_id=?`, serviceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var statuses []core.StatusResult
	for rows.Next() {
		var status core.StatusResult
		var health, checkedAt string
		if err := rows.Scan(&status.Target, &health, &checkedAt); err != nil {
			return err
		}
		status.ServiceID = serviceID
		status.Health = core.HealthState(health)
		status.CheckedAt, _ = time.Parse(time.RFC3339, checkedAt)
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rollup := aggregateTargetHealth(statuses)
	observationID := rollupObservationID(statuses)

	var stable, candidate, candidateStarted, candidateObservation, failureStarted string
	var samples int
	var stateIncarnation string
	err = tx.QueryRowContext(ctx, `SELECT stable_health, candidate_health, candidate_samples, candidate_started_at, candidate_observation_id, failure_started_at, service_incarnation FROM health_alert_states WHERE service_id=?`, serviceID).Scan(&stable, &candidate, &samples, &candidateStarted, &candidateObservation, &failureStarted, &stateIncarnation)
	if errors.Is(err, sql.ErrNoRows) || stateIncarnation != serviceIncarnation {
		failureStarted := ""
		if rollup != core.HealthHealthy && rollup != core.HealthNotApplicable {
			failureStarted = observedAt.Format(time.RFC3339Nano)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO health_alert_states(service_id, stable_health, candidate_health, candidate_samples, failure_started_at, service_incarnation) VALUES(?, ?, ?, 0, ?, ?)
ON CONFLICT(service_id) DO UPDATE SET stable_health=excluded.stable_health, candidate_health=excluded.candidate_health, candidate_samples=0, candidate_started_at='', candidate_observation_id='', failure_started_at=excluded.failure_started_at, service_incarnation=excluded.service_incarnation`, serviceID, string(rollup), string(rollup), failureStarted, serviceIncarnation)
		if err != nil {
			return err
		}
		return tx.Commit() // first observation is silent, even if failing
	}
	if err != nil {
		return err
	}
	if rollup == core.HealthNotApplicable {
		return tx.Commit()
	}
	if stable == string(rollup) {
		// A candidate that returns to the stable state was abandoned. If that
		// stable state is healthy, its failure start belongs to the abandoned
		// failure rather than a future incident.
		if stable == string(core.HealthHealthy) {
			failureStarted = ""
		}
		_, err = tx.ExecContext(ctx, `UPDATE health_alert_states SET candidate_health=?, candidate_samples=0, candidate_started_at='', candidate_observation_id='', failure_started_at=? WHERE service_id=?`, stable, failureStarted, serviceID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if candidate == string(rollup) {
		if candidateObservation != observationID {
			samples++
			candidateObservation = observationID
		}
	} else {
		candidate, samples, candidateObservation = string(rollup), 1, observationID
		candidateStarted = observedAt.Format(time.RFC3339Nano)
	}
	if failureStarted == "" && rollup != core.HealthHealthy {
		failureStarted = observedAt.Format(time.RFC3339Nano)
	}
	started, _ := time.Parse(time.RFC3339Nano, candidateStarted)
	continuous := config.Debounce <= 0 || (!started.IsZero() && !observedAt.Before(started) && observedAt.Sub(started) >= config.Debounce)
	if samples < config.StabilitySamples || !continuous {
		_, err = tx.ExecContext(ctx, `UPDATE health_alert_states SET candidate_health=?, candidate_samples=?, candidate_started_at=?, candidate_observation_id=?, failure_started_at=? WHERE service_id=?`, candidate, samples, candidateStarted, candidateObservation, failureStarted, serviceID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}

	newFailureStarted := failureStarted
	if rollup == core.HealthHealthy {
		newFailureStarted = ""
	} else if failureStarted == "" {
		newFailureStarted = observedAt.Format(time.RFC3339Nano)
	}
	reason := "service health transitioned"
	kind := "health.transition"
	if rollup == core.HealthHealthy {
		kind = "health.recovery"
		if started, parseErr := time.Parse(time.RFC3339Nano, failureStarted); parseErr == nil {
			reason = fmt.Sprintf("service recovered after %s", observedAt.Sub(started).Round(time.Second))
		}
	}
	occurrenceID := candidateStarted
	if rollup == core.HealthHealthy {
		occurrenceID = failureStarted
	}
	if occurrenceID == "" {
		occurrenceID = observedAt.Format(time.RFC3339Nano)
	}
	dedupeKey := "health:service:" + serviceID + ":" + stable + ":" + string(rollup) + ":" + occurrenceID
	stored, inserted, err := store.enqueueAlertEventInTx(ctx, tx, AlertEvent{Kind: kind, ServiceID: serviceID, OldState: stable, NewState: string(rollup), Reason: reason, DedupeKey: dedupeKey, CooldownKey: "health:service:" + serviceID + ":" + stable + ":" + string(rollup)}, config.Sinks, config.Cooldown)
	if err != nil {
		return err
	}
	if !inserted && stored.DedupeHash == store.alertDedupeHash(dedupeKey) {
		return fmt.Errorf("health edge was not persisted")
	}
	_, err = tx.ExecContext(ctx, `UPDATE health_alert_states SET stable_health=?, candidate_health=?, candidate_samples=0, candidate_started_at='', candidate_observation_id='', failure_started_at=? WHERE service_id=?`, string(rollup), string(rollup), newFailureStarted, serviceID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func rollupObservationID(statuses []core.StatusResult) string {
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Target < statuses[j].Target })
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, status.Target+"\x00"+string(status.Health)+"\x00"+status.CheckedAt.UTC().Format(time.RFC3339Nano))
	}
	return strings.Join(parts, "\x01")
}

func (store *Store) PruneStatusHistory(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-statusHistoryWindow).Format(time.RFC3339)
	result, err := store.db.ExecContext(ctx, `DELETE FROM status_history WHERE checked_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("prune status history: %w", err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows > 0 {
		store.invalidateSummary()
	}
	return nil
}

type overrideQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func monitorOverrideState(ctx context.Context, store *Store, queryer overrideQuerier, serviceID, target string) (bool, bool, error) {
	target = canonicalStatusTarget(target)
	childTarget := routetarget.IsChildTarget(target)
	query := `
SELECT target
FROM monitor_overrides
WHERE service_id=? AND not_applicable=1 AND target=?
`
	args := []any{serviceID, target}
	if childTarget {
		query = `
SELECT target
FROM monitor_overrides
WHERE service_id=? AND not_applicable=1 AND target IN (?, ?)
`
		args = []any{serviceID, target, routeMonitorTarget}
	}
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	exactOverride := false
	parentOverride := false
	for rows.Next() {
		var overrideTarget string
		if err := rows.Scan(&overrideTarget); err != nil {
			return false, false, err
		}
		switch {
		case overrideTarget == target:
			exactOverride = true
		case childTarget && overrideTarget == routeMonitorTarget:
			parentOverride = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	if parentOverride {
		routes, err := serviceMonitorRoutes(ctx, store, queryer, serviceID)
		if err != nil {
			return false, false, err
		}
		parentOverride = len(routes) > 0
	}
	return exactOverride, parentOverride, nil
}

func canonicalStatusTarget(target string) string {
	canonical, ok := routetarget.CanonicalTarget(target)
	if ok {
		return canonical
	}
	return strings.TrimSpace(target)
}

func (store *Store) parentRouteOverrideServices(ctx context.Context) (map[string]bool, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT s.rowid, s.id, s.exposure_json
FROM services s
JOIN monitor_overrides o ON o.service_id=s.id
WHERE o.target=? AND o.not_applicable=1
`, routeMonitorTarget)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	services := map[string]bool{}
	for rows.Next() {
		var rowID int64
		var serviceID string
		var exposureJSON string
		if err := rows.Scan(&rowID, &serviceID, &exposureJSON); err != nil {
			return nil, err
		}
		var exposure []string
		if err := store.fromPersistedJSON(exposureJSON, &exposure, "services", "exposure_json", rowID, serviceID); err != nil {
			return nil, err
		}
		if len(monitorRoutesFromExposure(exposure)) > 0 {
			services[serviceID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return services, nil
}

func (store *Store) UptimeStats(ctx context.Context) ([]core.UptimeStat, error) {
	windowStart := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	parentRouteOverrides, err := store.parentRouteOverrideServices(ctx)
	if err != nil {
		return nil, err
	}
	stats := map[string]*core.UptimeStat{}
	order := []string{}
	countRows, err := store.db.QueryContext(ctx, `
SELECT h.service_id, h.target,
       SUM(CASE WHEN h.health IN ('healthy', 'degraded') THEN 1 ELSE 0 END) AS up_count,
       COUNT(*) AS total_count
FROM status_history h
WHERE h.checked_at >= ?
  AND h.health != 'not_applicable'
  AND NOT EXISTS (
    SELECT 1 FROM monitor_overrides o
    WHERE o.service_id=h.service_id AND o.target=h.target AND o.not_applicable=1
  )
GROUP BY h.service_id, h.target
`, windowStart)
	if err != nil {
		return nil, fmt.Errorf("query uptime counts: %w", err)
	}
	defer countRows.Close()
	for countRows.Next() {
		var serviceID, target string
		var upCount, totalCount int
		if err := countRows.Scan(&serviceID, &target, &upCount, &totalCount); err != nil {
			return nil, err
		}
		if parentRouteOverrides[serviceID] && strings.HasPrefix(target, routeTargetPrefix) {
			continue
		}
		percent := 0.0
		if totalCount > 0 {
			percent = math.Round((100*float64(upCount)/float64(totalCount))*10) / 10
		}
		key := serviceID + "\x00" + target
		stats[key] = &core.UptimeStat{
			ServiceID:     serviceID,
			Target:        target,
			UptimePercent: percent,
			CheckCount:    totalCount,
			Samples:       []core.UptimeSample{},
		}
		order = append(order, key)
	}
	if err := countRows.Err(); err != nil {
		return nil, err
	}
	sampleRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at FROM (
  SELECT h.service_id, h.target, h.health, h.message, h.checked_at,
         ROW_NUMBER() OVER (PARTITION BY h.service_id, h.target ORDER BY h.checked_at DESC, h.id DESC) AS row_num
  FROM status_history h
  WHERE h.checked_at >= ?
    AND h.health != 'not_applicable'
    AND NOT EXISTS (
      SELECT 1 FROM monitor_overrides o
      WHERE o.service_id=h.service_id AND o.target=h.target AND o.not_applicable=1
    )
) WHERE row_num <= 40
ORDER BY service_id, target, checked_at ASC
`, windowStart)
	if err != nil {
		return nil, fmt.Errorf("query uptime samples: %w", err)
	}
	defer sampleRows.Close()
	for sampleRows.Next() {
		var serviceID, target, health, message, checkedAt string
		if err := sampleRows.Scan(&serviceID, &target, &health, &message, &checkedAt); err != nil {
			return nil, err
		}
		if parentRouteOverrides[serviceID] && strings.HasPrefix(target, routeTargetPrefix) {
			continue
		}
		stat, ok := stats[serviceID+"\x00"+target]
		if !ok {
			continue
		}
		sample := core.UptimeSample{
			Health:  core.HealthState(health),
			Message: store.redact(message),
		}
		if parsed, err := time.Parse(time.RFC3339, checkedAt); err == nil {
			sample.CheckedAt = parsed
		}
		stat.Samples = append(stat.Samples, sample)
	}
	if err := sampleRows.Err(); err != nil {
		return nil, err
	}
	result := make([]core.UptimeStat, 0, len(order))
	for _, key := range order {
		result = append(result, *stats[key])
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ServiceID != result[j].ServiceID {
			return result[i].ServiceID < result[j].ServiceID
		}
		return result[i].Target < result[j].Target
	})
	return result, nil
}
