package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
	if changed {
		store.invalidateSummary()
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

// RouteTargetReplacement is a semantic, discovery-proven replacement. It is
// intentionally separate from URL canonicalization: a non-default port changes
// target identity and must only be migrated when discovery proves the mapping.
type RouteTargetReplacement struct {
	ServiceID string
	OldRoute  string
	NewRoute  string
}

// RouteTargetExclusion records an ambiguous old identity. It prevents monitor
// pruning from deleting observations until a later scan can prove a bijection.
type RouteTargetExclusion struct {
	ServiceID string
	OldRoute  string
}

func (store *Store) SetRouteTargetExclusions(ctx context.Context, exclusions []RouteTargetExclusion) error {
	if len(exclusions) == 0 {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setRouteTargetExclusions(ctx, tx, exclusions); err != nil {
		return err
	}
	return store.commitAndInvalidateSummary(tx)
}

func setRouteTargetExclusions(ctx context.Context, tx *sql.Tx, exclusions []RouteTargetExclusion) error {
	for _, exclusion := range exclusions {
		if exclusion.ServiceID == "" || exclusion.OldRoute == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO route_target_exclusions(service_id, old_route) VALUES(?, ?)`, exclusion.ServiceID, exclusion.OldRoute); err != nil {
			return fmt.Errorf("persist route target exclusion: %w", err)
		}
	}
	return nil
}

// RouteTargetExclusions returns unresolved identities for a repository before a
// successful scan replaces its service inventory.
func (store *Store) RouteTargetExclusions(ctx context.Context, repository string) ([]RouteTargetExclusion, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT exclusions.service_id, exclusions.old_route
FROM route_target_exclusions AS exclusions
JOIN services ON services.id=exclusions.service_id
WHERE services.repository=?
`, repository)
	if err != nil {
		return nil, fmt.Errorf("query route target exclusions: %w", err)
	}
	defer rows.Close()
	var exclusions []RouteTargetExclusion
	for rows.Next() {
		var exclusion RouteTargetExclusion
		if err := rows.Scan(&exclusion.ServiceID, &exclusion.OldRoute); err != nil {
			return nil, err
		}
		exclusions = append(exclusions, exclusion)
	}
	return exclusions, rows.Err()
}

// MigrateRouteTargetReplacements applies discovery-proven route identity
// replacements outside a scan transaction. Scan callers should prefer
// FinishScanWithRouteTargetChanges so the service replacement is atomic.
func (store *Store) MigrateRouteTargetReplacements(ctx context.Context, replacements []RouteTargetReplacement, targets []config.HTTPRouteTarget) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.migrateRouteTargetReplacements(ctx, tx, replacements, httpRouteTargetNames(targets)); err != nil {
		return err
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.observeHealthAlerts(ctx, routeReplacementServiceIDs(replacements, nil), time.Now().UTC())
	return nil
}

func routeReplacementServiceIDs(replacements []RouteTargetReplacement, retained map[string]struct{}) []string {
	ids := make([]string, 0, len(replacements))
	seen := make(map[string]struct{}, len(replacements))
	for _, replacement := range replacements {
		if replacement.ServiceID == "" || replacement.OldRoute == "" || replacement.NewRoute == "" || replacement.OldRoute == replacement.NewRoute {
			continue
		}
		if retained != nil {
			if _, ok := retained[replacement.ServiceID]; !ok {
				continue
			}
		}
		if _, ok := seen[replacement.ServiceID]; ok {
			continue
		}
		seen[replacement.ServiceID] = struct{}{}
		ids = append(ids, replacement.ServiceID)
	}
	return ids
}

func (store *Store) migrateRouteTargetReplacements(ctx context.Context, tx *sql.Tx, replacements []RouteTargetReplacement, targetNames []string) error {
	// Check both guards in this transaction before any alert query or mutation.
	// A pre-existing state lock must not hide a changed or missing key.
	alertStateErr := store.ensureAlertStateUnlocked()
	alertDedupeKeyErr := store.ensureAlertDedupeKeyCurrent(ctx, tx)
	alertMigrationReady := alertStateErr == nil && alertDedupeKeyErr == nil
	if alertMigrationReady {
		if err := store.applyDeferredAlertRouteReconciliations(ctx, tx); err != nil {
			return err
		}
	} else if err := store.deferAlertRouteReconciliations(ctx, tx, replacements, targetNames); err != nil {
		return err
	}
	for _, replacement := range replacements {
		if replacement.ServiceID == "" || replacement.OldRoute == "" || replacement.NewRoute == "" || replacement.OldRoute == replacement.NewRoute {
			continue
		}
		for _, targetName := range targetNames {
			oldTarget := routetarget.TargetForName(targetName, replacement.OldRoute)
			newTarget := routetarget.TargetForName(targetName, replacement.NewRoute)
			if err := migrateRouteOverride(ctx, tx, replacement.ServiceID, oldTarget, newTarget); err != nil {
				return err
			}
			if err := migrateRouteStatus(ctx, tx, replacement.ServiceID, oldTarget, newTarget); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE status_history SET target=? WHERE service_id=? AND target=?`, newTarget, replacement.ServiceID, oldTarget); err != nil {
				return fmt.Errorf("retarget route history: %w", err)
			}
			if alertMigrationReady {
				if _, err := store.migrateAlertRouteIdentity(ctx, tx, replacement.ServiceID, oldTarget, newTarget); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM route_target_exclusions WHERE service_id=? AND old_route=?`, replacement.ServiceID, replacement.OldRoute); err != nil {
			return fmt.Errorf("clear route target exclusion: %w", err)
		}
	}
	return nil
}

func (store *Store) deferAlertRouteReconciliations(ctx context.Context, tx *sql.Tx, replacements []RouteTargetReplacement, targetNames []string) error {
	for _, replacement := range replacements {
		if replacement.ServiceID == "" || replacement.OldRoute == "" || replacement.NewRoute == "" || replacement.OldRoute == replacement.NewRoute {
			continue
		}
		for _, targetName := range targetNames {
			oldTarget := routetarget.TargetForName(targetName, replacement.OldRoute)
			newTarget := routetarget.TargetForName(targetName, replacement.NewRoute)
			if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO deferred_alert_route_reconciliations(service_id, old_target, new_target)
VALUES(?, ?, ?)
`, replacement.ServiceID, oldTarget, newTarget); err != nil {
				return fmt.Errorf("defer alert route reconciliation: %w", err)
			}
		}
	}
	return nil
}

func deferAlertRouteReconciliation(ctx context.Context, tx *sql.Tx, serviceID, oldTarget, newTarget string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO deferred_alert_route_reconciliations(service_id, old_target, new_target)
VALUES(?, ?, ?)
`, serviceID, oldTarget, newTarget); err != nil {
		return fmt.Errorf("defer alert route reconciliation: %w", err)
	}
	return nil
}

func (store *Store) applyDeferredAlertRouteReconciliations(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, old_target, new_target
FROM deferred_alert_route_reconciliations
ORDER BY service_id, old_target, new_target
`)
	if err != nil {
		return fmt.Errorf("query deferred alert route reconciliations: %w", err)
	}
	deferred := make([]struct{ serviceID, oldTarget, newTarget string }, 0)
	for rows.Next() {
		var reconciliation struct{ serviceID, oldTarget, newTarget string }
		if err := rows.Scan(&reconciliation.serviceID, &reconciliation.oldTarget, &reconciliation.newTarget); err != nil {
			_ = rows.Close()
			return err
		}
		deferred = append(deferred, reconciliation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Deferred replacements can form a chain (A -> B -> C).  The queue's
	// lexical order is not causal order, so repeat the pass until no mutable
	// alert can advance.  Only then may an edge be discarded: deleting B -> C
	// after an early no-op would strand a later A -> B migration at B.
	settled := false
	for pass := 0; pass <= len(deferred); pass++ {
		moved := false
		for _, reconciliation := range deferred {
			matched, err := store.migrateAlertRouteIdentity(ctx, tx, reconciliation.serviceID, reconciliation.oldTarget, reconciliation.newTarget)
			if err != nil {
				return err
			}
			moved = moved || matched
		}
		if !moved {
			settled = true
			break
		}
	}
	// A replacement cycle has no fixed point. Retain its edges for a later
	// acyclic reconciliation rather than looping forever or deleting a route
	// that another edge may still traverse.
	if !settled {
		return nil
	}
	for _, reconciliation := range deferred {
		matched, err := store.mutableAlertRouteIdentityExists(ctx, tx, reconciliation.serviceID, reconciliation.oldTarget)
		if err != nil {
			return err
		}
		if matched {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM deferred_alert_route_reconciliations WHERE service_id=? AND old_target=? AND new_target=?`, reconciliation.serviceID, reconciliation.oldTarget, reconciliation.newTarget); err != nil {
			return fmt.Errorf("clear deferred alert route reconciliation: %w", err)
		}
	}
	return nil
}

func (store *Store) mutableAlertRouteIdentityExists(ctx context.Context, tx *sql.Tx, serviceID, target string) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alert_events WHERE service_id=? AND target=? AND status IN (?, ?))`, serviceID, target, AlertEventStatusPending, AlertEventStatusFailed).Scan(&found)
	return found != 0, err
}

func (store *Store) migrateAlertRouteIdentity(ctx context.Context, tx *sql.Tx, serviceID, oldTarget, newTarget string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, dedupe_key, status FROM alert_events WHERE service_id=? AND target=? AND status IN (?, ?)`, serviceID, oldTarget, AlertEventStatusPending, AlertEventStatusFailed)
	if err != nil {
		return false, fmt.Errorf("query mutable route alerts: %w", err)
	}
	defer rows.Close()
	type alertUpdate struct {
		id     int64
		key    string
		status string
	}
	var updates []alertUpdate
	for rows.Next() {
		var update alertUpdate
		if err := rows.Scan(&update.id, &update.key, &update.status); err != nil {
			return false, err
		}
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	migrated := false
	for _, update := range updates {
		key := strings.ReplaceAll(update.key, oldTarget, newTarget)
		if key == update.key {
			key = strings.ReplaceAll(key, strings.TrimPrefix(oldTarget, "routes: "), strings.TrimPrefix(newTarget, "routes: "))
		}
		newHash := store.alertDedupeHash(key)
		if update.status == AlertEventStatusPending {
			deferred, err := store.reconcilePendingAlertRouteCollision(ctx, tx, update.id, newHash)
			if err != nil {
				return false, err
			}
			if deferred {
				if err := deferAlertRouteReconciliation(ctx, tx, serviceID, oldTarget, newTarget); err != nil {
					return false, err
				}
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE alert_events SET target=?, dedupe_key=?, dedupe_hash=? WHERE id=? AND service_id=?`, newTarget, key, newHash, update.id, serviceID); err != nil {
			return false, fmt.Errorf("retarget mutable route alert: %w", err)
		}
		migrated = true
	}
	return migrated, nil
}

// reconcilePendingAlertRouteCollision makes the old-identity event the keeper.
// It merges the destination event's active sinks into the keeper without
// rewriting an in-flight claim before the keeper receives its destination
// hash. Terminal dispatches and terminal events are never rewritten.
func (store *Store) reconcilePendingAlertRouteCollision(ctx context.Context, tx *sql.Tx, keeperID int64, destinationHash string) (bool, error) {
	var duplicateID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM alert_events WHERE dedupe_hash=? AND status=? AND id<>?`, destinationHash, AlertEventStatusPending, keeperID).Scan(&duplicateID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find pending route alert collision: %w", err)
	}
	deferred, err := store.mergeRouteAlertCollisionDispatches(ctx, tx, keeperID, duplicateID)
	if err != nil {
		return false, err
	}
	return deferred, nil
}

// mergeRouteAlertCollisionDispatches preserves active dispatch rows verbatim.
// In particular, an in-flight row keeps its worker, claim, and lease; creating
// a fresh pending clone would permit a second external delivery.
func (store *Store) mergeRouteAlertCollisionDispatches(ctx context.Context, tx *sql.Tx, keeperID, duplicateID int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, sink, status FROM alert_dispatches WHERE event_id=? AND status IN (?, ?) ORDER BY id`, duplicateID, AlertDispatchStatusPending, AlertDispatchStatusInFlight)
	if err != nil {
		return false, fmt.Errorf("query route alert collision dispatches: %w", err)
	}
	type dispatch struct {
		id           int64
		sink, status string
	}
	var duplicates []dispatch
	for rows.Next() {
		var d dispatch
		if err := rows.Scan(&d.id, &d.sink, &d.status); err != nil {
			_ = rows.Close()
			return false, err
		}
		duplicates = append(duplicates, d)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	// Never rewrite or delete an in-flight claim. If either event already has
	// the same sink, convergence would require touching that claim (and may
	// otherwise violate the pending-dedupe unique index), so leave both events
	// byte-for-byte intact for deferred fixed-point reconciliation.
	for _, duplicate := range duplicates {
		if duplicate.status != AlertDispatchStatusInFlight {
			continue
		}
		_, found, err := alertDispatchForEventSink(ctx, tx, keeperID, duplicate.sink)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	for _, duplicate := range duplicates {
		keeper, found, err := alertDispatchForEventSink(ctx, tx, keeperID, duplicate.sink)
		if err != nil {
			return false, err
		}
		if !found {
			if _, err := tx.ExecContext(ctx, `UPDATE alert_dispatches SET event_id=? WHERE id=?`, keeperID, duplicate.id); err != nil {
				return false, err
			}
			continue
		}
		// A delivered keeper has already completed this sink, so the
		// destination pending row is redundant. Other keeper statuses do not
		// prove that: in particular, pending work ranks above a dead letter.
		// Leave the rows intact and defer until their active deliveries settle.
		if duplicate.status == AlertDispatchStatusPending && keeper.status == AlertDispatchStatusDelivered {
			if err := resetDuplicateAlertDispatch(ctx, tx, duplicate.id); err != nil {
				return false, err
			}
			continue
		}
		return true, nil
	}
	var inFlight int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_dispatches WHERE event_id=? AND status=?`, duplicateID, AlertDispatchStatusInFlight).Scan(&inFlight); err != nil {
		return false, err
	}
	if inFlight == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE alert_events SET status=? WHERE id=?`, AlertEventStatusReset, duplicateID); err != nil {
			return false, err
		}
	} else if err := store.syncAlertEventStatus(ctx, tx, duplicateID); err != nil {
		return false, fmt.Errorf("preserve in-flight duplicate route alert: %w", err)
	}
	return false, nil
}

func migrateRouteOverride(ctx context.Context, tx *sql.Tx, serviceID, oldTarget, newTarget string) error {
	var oldNotApplicable int
	var oldUpdatedAt string
	err := tx.QueryRowContext(ctx, `SELECT not_applicable, updated_at FROM monitor_overrides WHERE service_id=? AND target=?`, serviceID, oldTarget).Scan(&oldNotApplicable, &oldUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var newNotApplicable int
	var newUpdatedAt string
	err = tx.QueryRowContext(ctx, `SELECT not_applicable, updated_at FROM monitor_overrides WHERE service_id=? AND target=?`, serviceID, newTarget).Scan(&newNotApplicable, &newUpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `UPDATE monitor_overrides SET target=? WHERE service_id=? AND target=?`, newTarget, serviceID, oldTarget)
	case err == nil:
		if oldNotApplicable > newNotApplicable {
			newNotApplicable = oldNotApplicable
		}
		if oldUpdatedAt > newUpdatedAt {
			newUpdatedAt = oldUpdatedAt
		}
		_, err = tx.ExecContext(ctx, `UPDATE monitor_overrides SET not_applicable=?, updated_at=? WHERE service_id=? AND target=?`, newNotApplicable, newUpdatedAt, serviceID, newTarget)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM monitor_overrides WHERE service_id=? AND target=?`, serviceID, oldTarget)
		}
	}
	if err != nil {
		return fmt.Errorf("migrate route override: %w", err)
	}
	return nil
}

func migrateRouteStatus(ctx context.Context, tx *sql.Tx, serviceID, oldTarget, newTarget string) error {
	var old statusResultRouteAlias
	err := tx.QueryRowContext(ctx, `SELECT service_id, target, health, message, checked_at FROM status_results WHERE service_id=? AND target=?`, serviceID, oldTarget).Scan(&old.serviceID, &old.target, &old.health, &old.message, &old.checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var newer statusResultRouteAlias
	err = tx.QueryRowContext(ctx, `SELECT service_id, target, health, message, checked_at FROM status_results WHERE service_id=? AND target=?`, serviceID, newTarget).Scan(&newer.serviceID, &newer.target, &newer.health, &newer.message, &newer.checkedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `UPDATE status_results SET target=? WHERE service_id=? AND target=?`, newTarget, serviceID, oldTarget)
	case err == nil:
		chosen := newer
		if shouldReplaceCanonicalStatus(old, newer) {
			chosen = old
		}
		_, err = tx.ExecContext(ctx, `UPDATE status_results SET health=?, message=?, checked_at=? WHERE service_id=? AND target=?`, chosen.health, chosen.message, chosen.checkedAt, serviceID, newTarget)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target=?`, serviceID, oldTarget)
		}
	}
	if err != nil {
		return fmt.Errorf("migrate route status: %w", err)
	}
	return nil
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
	}
	return nil
}
