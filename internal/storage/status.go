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

func (store *Store) StatusResults(ctx context.Context) ([]core.StatusResult, error) {
	parentRouteOverrides, err := store.parentRouteOverrideServices(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
SELECT rowid, service_id, target, health, message, checked_at, observed_images_json
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
		var health, checkedAt, observedImages string
		if err := rows.Scan(&rowID, &status.ServiceID, &status.Target, &health, &status.Message, &checkedAt, &observedImages); err != nil {
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
		if parentRouteOverrides[status.ServiceID] && strings.HasPrefix(status.Target, routeTargetPrefix) {
			continue
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
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
	return tx.Commit()
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
	return tx.Commit()
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
	return tx.Commit()
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
	}
	checkedAt := status.CheckedAt.UTC().Format(time.RFC3339)
	message := store.redact(status.Message)
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at, observed_images_json)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at,
  observed_images_json=excluded.observed_images_json
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt, toJSON(status.ObservedImages))
	if err != nil {
		return fmt.Errorf("upsert status %s/%s: %w", status.ServiceID, status.Target, err)
	}
	if status.Health == core.HealthNotApplicable {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM status_history WHERE service_id=? AND target=?
`, status.ServiceID, status.Target); err != nil {
			return fmt.Errorf("clear not applicable status history %s/%s: %w", status.ServiceID, status.Target, err)
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_history(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt)
	if err != nil {
		return fmt.Errorf("insert status history %s/%s: %w", status.ServiceID, status.Target, err)
	}
	return tx.Commit()
}

func (store *Store) PruneStatusHistory(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-statusHistoryWindow).Format(time.RFC3339)
	if _, err := store.db.ExecContext(ctx, `DELETE FROM status_history WHERE checked_at < ?`, cutoff); err != nil {
		return fmt.Errorf("prune status history: %w", err)
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
