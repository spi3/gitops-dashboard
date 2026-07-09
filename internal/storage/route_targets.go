package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
	"github.com/example/gitops-dashboard/internal/sanitizer"
)

func (store *Store) canonicalizeStoredRouteTargets(ctx context.Context) (bool, error) {
	return store.canonicalizeStoredRouteTargetsForNames(ctx, []string{routeMonitorTarget})
}

func (store *Store) CanonicalizeHTTPRouteTargets(ctx context.Context, targets []config.HTTPRouteTarget) error {
	changed, err := store.canonicalizeStoredRouteTargetsForNames(ctx, httpRouteTargetNames(targets))
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return store.compactRedactedPages(ctx)
}

func (store *Store) canonicalizeStoredRouteTargetsForNames(ctx context.Context, targetNames []string) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, targetName := range targetNames {
		monitorOverridesChanged, err := canonicalizeMonitorOverrides(ctx, tx, targetName)
		if err != nil {
			return false, err
		}
		changed = changed || monitorOverridesChanged
		statusResultsChanged, err := canonicalizeStatusResults(ctx, tx, targetName)
		if err != nil {
			return false, err
		}
		changed = changed || statusResultsChanged
		statusHistoryChanged, err := canonicalizeStatusHistory(ctx, tx, targetName)
		if err != nil {
			return false, err
		}
		changed = changed || statusHistoryChanged
		if err := enforceActiveRouteOverrideStatuses(ctx, tx, targetName); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func httpRouteTargetNames(targets []config.HTTPRouteTarget) []string {
	names := make([]string, 0, len(targets)+1)
	names = append(names, routeMonitorTarget)
	for _, target := range targets {
		name := strings.TrimSpace(target.Name)
		if name == "" {
			name = routeMonitorTarget
		}
		names = append(names, name)
	}
	return dedupeStrings(names)
}

func routeTargetPrefixForName(targetName string) string {
	targetName = strings.TrimSpace(targetName)
	if targetName == "" {
		targetName = routeMonitorTarget
	}
	return targetName + ": "
}

type monitorOverrideRouteAlias struct {
	serviceID     string
	target        string
	notApplicable int
	updatedAt     string
	canonical     string
}

func canonicalizeMonitorOverrides(ctx context.Context, tx *sql.Tx, targetName string) (bool, error) {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, not_applicable, updated_at
FROM monitor_overrides
WHERE target LIKE ?
`, targetPrefix+"%")
	if err != nil {
		return false, fmt.Errorf("query route override aliases: %w", err)
	}
	var aliases []monitorOverrideRouteAlias
	sensitiveChanged := false
	for rows.Next() {
		var alias monitorOverrideRouteAlias
		if err := rows.Scan(&alias.serviceID, &alias.target, &alias.notApplicable, &alias.updatedAt); err != nil {
			_ = rows.Close()
			return false, err
		}
		canonical, ok := routetarget.CanonicalTargetForName(alias.target, targetName)
		if !ok || canonical == alias.target {
			continue
		}
		sensitiveChanged = sensitiveChanged || sanitizer.StripURLUserinfo(alias.target) != alias.target
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, alias := range aliases {
		var existingNotApplicable int
		var existingUpdatedAt string
		err := tx.QueryRowContext(ctx, `
SELECT not_applicable, updated_at
FROM monitor_overrides
WHERE service_id=? AND target=?
`, alias.serviceID, alias.canonical).Scan(&existingNotApplicable, &existingUpdatedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
UPDATE monitor_overrides SET target=?
WHERE service_id=? AND target=?
`, alias.canonical, alias.serviceID, alias.target); err != nil {
				return false, fmt.Errorf("canonicalize route override %s/%s: %w", alias.serviceID, alias.target, err)
			}
		case err != nil:
			return false, err
		default:
			mergedNotApplicable := existingNotApplicable
			if alias.notApplicable > mergedNotApplicable {
				mergedNotApplicable = alias.notApplicable
			}
			mergedUpdatedAt := existingUpdatedAt
			if alias.updatedAt > mergedUpdatedAt {
				mergedUpdatedAt = alias.updatedAt
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE monitor_overrides SET not_applicable=?, updated_at=?
WHERE service_id=? AND target=?
`, mergedNotApplicable, mergedUpdatedAt, alias.serviceID, alias.canonical); err != nil {
				return false, fmt.Errorf("merge route override %s/%s: %w", alias.serviceID, alias.target, err)
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM monitor_overrides
WHERE service_id=? AND target=?
`, alias.serviceID, alias.target); err != nil {
				return false, fmt.Errorf("delete route override alias %s/%s: %w", alias.serviceID, alias.target, err)
			}
		}
	}
	return sensitiveChanged, nil
}

type statusResultRouteAlias struct {
	serviceID string
	target    string
	health    string
	message   string
	checkedAt string
	canonical string
}

func canonicalizeStatusResults(ctx context.Context, tx *sql.Tx, targetName string) (bool, error) {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at
FROM status_results
WHERE target LIKE ?
`, targetPrefix+"%")
	if err != nil {
		return false, fmt.Errorf("query route status aliases: %w", err)
	}
	var aliases []statusResultRouteAlias
	sensitiveChanged := false
	for rows.Next() {
		var alias statusResultRouteAlias
		if err := rows.Scan(&alias.serviceID, &alias.target, &alias.health, &alias.message, &alias.checkedAt); err != nil {
			_ = rows.Close()
			return false, err
		}
		canonical, ok := routetarget.CanonicalTargetForName(alias.target, targetName)
		if !ok || canonical == alias.target {
			continue
		}
		sensitiveChanged = sensitiveChanged || sanitizer.StripURLUserinfo(alias.target) != alias.target
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, alias := range aliases {
		var existing statusResultRouteAlias
		err := tx.QueryRowContext(ctx, `
SELECT service_id, target, health, message, checked_at
FROM status_results
WHERE service_id=? AND target=?
`, alias.serviceID, alias.canonical).Scan(&existing.serviceID, &existing.target, &existing.health, &existing.message, &existing.checkedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
UPDATE status_results SET target=?
WHERE service_id=? AND target=?
`, alias.canonical, alias.serviceID, alias.target); err != nil {
				return false, fmt.Errorf("canonicalize route status %s/%s: %w", alias.serviceID, alias.target, err)
			}
		case err != nil:
			return false, err
		default:
			chosen := existing
			if shouldReplaceCanonicalStatus(alias, existing) {
				chosen = alias
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE status_results SET health=?, message=?, checked_at=?
WHERE service_id=? AND target=?
`, chosen.health, chosen.message, chosen.checkedAt, alias.serviceID, alias.canonical); err != nil {
				return false, fmt.Errorf("merge route status %s/%s: %w", alias.serviceID, alias.target, err)
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM status_results
WHERE service_id=? AND target=?
`, alias.serviceID, alias.target); err != nil {
				return false, fmt.Errorf("delete route status alias %s/%s: %w", alias.serviceID, alias.target, err)
			}
		}
	}
	return sensitiveChanged, nil
}

func shouldReplaceCanonicalStatus(candidate, existing statusResultRouteAlias) bool {
	if candidate.checkedAt != existing.checkedAt {
		return candidate.checkedAt > existing.checkedAt
	}
	candidatePriority := healthPriority(core.HealthState(candidate.health))
	existingPriority := healthPriority(core.HealthState(existing.health))
	if candidatePriority != existingPriority {
		return candidatePriority < existingPriority
	}
	return candidate.target < existing.target
}

type statusHistoryRouteAlias struct {
	id        int64
	target    string
	canonical string
}

func canonicalizeStatusHistory(ctx context.Context, tx *sql.Tx, targetName string) (bool, error) {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT id, target
FROM status_history
WHERE target LIKE ?
`, targetPrefix+"%")
	if err != nil {
		return false, fmt.Errorf("query route history aliases: %w", err)
	}
	var aliases []statusHistoryRouteAlias
	sensitiveChanged := false
	for rows.Next() {
		var alias statusHistoryRouteAlias
		if err := rows.Scan(&alias.id, &alias.target); err != nil {
			_ = rows.Close()
			return false, err
		}
		canonical, ok := routetarget.CanonicalTargetForName(alias.target, targetName)
		if !ok || canonical == alias.target {
			continue
		}
		sensitiveChanged = sensitiveChanged || sanitizer.StripURLUserinfo(alias.target) != alias.target
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, alias := range aliases {
		if _, err := tx.ExecContext(ctx, `
UPDATE status_history SET target=?
WHERE id=?
`, alias.canonical, alias.id); err != nil {
			return false, fmt.Errorf("canonicalize route history %d/%s: %w", alias.id, alias.target, err)
		}
	}
	return sensitiveChanged, nil
}

type activeRouteOverride struct {
	serviceID string
	target    string
	updatedAt string
}

func enforceActiveRouteOverrideStatuses(ctx context.Context, tx *sql.Tx, targetName string) error {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, updated_at
FROM monitor_overrides
WHERE target LIKE ? AND not_applicable=1
`, targetPrefix+"%")
	if err != nil {
		return fmt.Errorf("query active route overrides: %w", err)
	}
	var overrides []activeRouteOverride
	for rows.Next() {
		var override activeRouteOverride
		if err := rows.Scan(&override.serviceID, &override.target, &override.updatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		overrides = append(overrides, override)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, override := range overrides {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at
`, override.serviceID, override.target, string(core.HealthNotApplicable), "not applicable", override.updatedAt); err != nil {
			return fmt.Errorf("force route override status %s/%s: %w", override.serviceID, override.target, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE status_history
SET health=?, message=?
WHERE service_id=? AND target=?
`, string(core.HealthNotApplicable), "not applicable", override.serviceID, override.target); err != nil {
			return fmt.Errorf("force route override history %s/%s: %w", override.serviceID, override.target, err)
		}
	}
	return nil
}
