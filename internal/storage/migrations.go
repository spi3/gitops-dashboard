package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type migrationStep struct {
	id              string
	compactOnChange bool
	alertOnly       bool
	apply           func(context.Context) (bool, error)
}

func (store *Store) migrate(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			// A later core migration can fail after alert text already exists. Scrub
			// once on that error path, but never make availability depend on it.
			_ = store.RedactPersistedSensitiveValues(context.Background())
		}
	}()
	compactNeeded := false
	alertLocked := store.alertStateLocked
	for _, step := range store.migrationSteps() {
		if step.alertOnly && alertLocked {
			continue
		}
		stepChanged, err := step.apply(ctx)
		if err != nil {
			if step.alertOnly {
				message := fmt.Sprintf("alert state locked: migration %s failed: %v", step.id, err)
				store.lockAlertState(message)
				alertLocked = true
				// Best effort only: do not let a scrub failure re-couple alert repair
				// to dashboard availability.
				_ = store.RedactPersistedSensitiveValues(context.Background())
				continue
			}
			return fmt.Errorf("migration %s: %w", step.id, err)
		}
		// Some alert repair helpers latch state without returning an error when
		// they find ambiguous tables. Do not perform any later alert mutation.
		if step.alertOnly && store.isAlertStateLocked() {
			alertLocked = true
			continue
		}
		compactNeeded = compactNeeded || (step.compactOnChange && stepChanged)
	}
	if compactNeeded {
		return store.compactRedactedPages(ctx)
	}
	return nil
}

func (store *Store) migrationSteps() []migrationStep {
	return []migrationStep{
		{
			id: "001_base_schema",
			apply: func(ctx context.Context) (bool, error) {
				_, err := store.db.ExecContext(ctx, migrations)
				if err != nil {
					return false, fmt.Errorf("run schema SQL: %w", err)
				}
				return false, nil
			},
		},
		store.ensureColumnStep("002_services_config_json", "services", "config_json", "TEXT NOT NULL DEFAULT '[]'"),
		store.ensureColumnStep("003_services_compose_project", "services", "compose_project", "TEXT NOT NULL DEFAULT ''"),
		store.ensureColumnStep("004_status_results_observed_images_json", "status_results", "observed_images_json", "TEXT NOT NULL DEFAULT '[]'"),
		{
			id:        "007_repair_alert_events_schema",
			alertOnly: true,
			apply:     store.repairAlertEventsSchema,
		},
		{
			id:        "008_repair_alert_dispatches_schema",
			alertOnly: true,
			apply:     store.repairAlertDispatchesSchema,
		},
		{
			id:        "008b_backfill_alert_dedupe_hashes_before_redaction",
			alertOnly: true,
			apply: func(ctx context.Context) (bool, error) {
				return false, store.backfillAlertDedupeHashesBeforeRedaction(ctx)
			},
		},
		{
			id:        "009_repair_alert_table_constraints",
			alertOnly: true,
			apply:     store.repairAlertTableConstraints,
		},
		{
			id:        "010_reset_alert_state_after_missing_key",
			alertOnly: true,
			apply:     store.resetAlertStateAfterMissingKey,
		},
		{
			id:        "011_alert_indexes",
			alertOnly: true,
			apply:     store.ensureAlertIndexes,
		},
		{
			id:              "013_validate_and_repair_persisted_json",
			compactOnChange: true,
			apply:           store.stripPersistedRouteUserinfo,
		},
		{
			id:    "014_canonicalize_route_status_targets",
			apply: store.canonicalizeStoredRouteTargets,
		},
		{
			id: "015_route_target_exclusions",
			apply: func(ctx context.Context) (bool, error) {
				_, err := store.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS route_target_exclusions (
  service_id TEXT NOT NULL,
  old_route TEXT NOT NULL,
  PRIMARY KEY(service_id, old_route)
);
CREATE INDEX IF NOT EXISTS idx_route_target_exclusions_service_route
ON route_target_exclusions(service_id, old_route);`)
				return false, err
			},
		},
	}
}

func (store *Store) ensureColumnStep(id, table, column, definition string) migrationStep {
	return migrationStep{
		id: id,
		apply: func(ctx context.Context) (bool, error) {
			return store.ensureColumnChanged(ctx, table, column, definition)
		},
	}
}

func (store *Store) stripPersistedRouteUserinfo(ctx context.Context) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := store.scanStartupPersistedJSON(ctx, tx)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func (store *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	_, err := store.ensureColumnChanged(ctx, table, column, definition)
	return err
}

func (store *Store) redactConfiguredSensitiveValues(ctx context.Context) (bool, error) {
	if len(store.redactionValues()) == 0 {
		return false, nil
	}
	if err := store.backfillAlertDedupeHashesBeforeRedaction(ctx); err != nil {
		return false, err
	}
	return store.redactCorePersistedSensitiveValues(ctx)
}

func (store *Store) ensureColumnChanged(ctx context.Context, table, column, definition string) (bool, error) {
	rows, err := store.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	_, err = store.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err == nil, err
}

func (store *Store) repairAlertEventsSchema(ctx context.Context) (bool, error) {
	changed := false
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "kind", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "service_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "target", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "repository", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "agent", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "old_state", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "new_state", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "reason", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "dedupe_key", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "dedupe_hash", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "created_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "created_at_ns", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "status", definition: "TEXT NOT NULL DEFAULT 'pending'"},
	} {
		columnChanged, err := store.ensureColumnChanged(ctx, "alert_events", column.name, column.definition)
		if err != nil {
			return false, err
		}
		changed = changed || columnChanged
	}
	hashChanged, err := store.backfillAlertEventDedupeHashes(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || hashChanged
	timeChanged, err := store.backfillAlertEventCreatedAtNS(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || timeChanged
	return changed, nil
}

func (store *Store) repairAlertDispatchesSchema(ctx context.Context) (bool, error) {
	changed := false
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "event_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "sink", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "status", definition: "TEXT NOT NULL DEFAULT 'pending'"},
		{name: "worker_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "claim_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "lease_expires_at", definition: "TEXT"},
		{name: "lease_expires_at_ns", definition: "INTEGER"},
		{name: "next_attempt_at_ns", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "attempts", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "last_error", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "delivered_at", definition: "TEXT"},
		{name: "delivered_at_ns", definition: "INTEGER"},
		{name: "updated_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "updated_at_ns", definition: "INTEGER NOT NULL DEFAULT 0"},
	} {
		columnChanged, err := store.ensureColumnChanged(ctx, "alert_dispatches", column.name, column.definition)
		if err != nil {
			return false, err
		}
		changed = changed || columnChanged
	}
	timeChanged, err := store.backfillAlertDispatchTimestampNS(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || timeChanged
	return changed, nil
}

func (store *Store) repairAlertTableConstraints(ctx context.Context) (bool, error) {
	preserveRawDedupeKeys, err := store.lockedAlertRowsNeedRawDedupeKey(ctx)
	if err != nil {
		return false, err
	}
	if preserveRawDedupeKeys {
		return false, nil
	}
	legacyUnique, err := store.alertEventsHasUniqueDedupeKey(ctx)
	if err != nil {
		return false, err
	}
	dispatchConstraintsBroken, err := store.alertDispatchesNeedConstraintRebuild(ctx)
	if err != nil {
		return false, err
	}
	legacyEvents, err := store.tableExists(ctx, "alert_events_legacy")
	if err != nil {
		return false, err
	}
	legacyDispatches, err := store.tableExists(ctx, "alert_dispatches_legacy")
	if err != nil {
		return false, err
	}
	changed := false
	schemaBroken, err := store.alertTablesNeedSchemaRebuild(ctx)
	if err != nil {
		return false, err
	}
	if legacyUnique || dispatchConstraintsBroken || schemaBroken || legacyEvents || legacyDispatches {
		if err := store.rebuildAlertTables(ctx); err != nil {
			return false, err
		}
		changed = true
	}
	cleanupChanged, err := store.reconcileLegacyAlertEventStatuses(ctx)
	if err != nil {
		return false, err
	}
	return changed || cleanupChanged, nil
}

// alertTablesNeedSchemaRebuild catches version-skewed definitions that happen
// to have the right column names but cannot safely store the canonical rows.
func (store *Store) alertTablesNeedSchemaRebuild(ctx context.Context) (bool, error) {
	for _, table := range []string{"alert_events", "alert_dispatches"} {
		rows, err := store.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				_ = rows.Close()
				return false, err
			}
			if name == "id" {
				if strings.ToUpper(typ) != "INTEGER" || pk == 0 {
					_ = rows.Close()
					return true, nil
				}
				continue
			}
			if strings.HasSuffix(name, "_ns") || name == "event_id" || name == "attempts" {
				if strings.ToUpper(typ) != "INTEGER" {
					_ = rows.Close()
					return true, nil
				}
			} else if name != "lease_expires_at" && name != "delivered_at" {
				if strings.ToUpper(typ) != "TEXT" {
					_ = rows.Close()
					return true, nil
				}
			}
			if name != "lease_expires_at" && name != "lease_expires_at_ns" && name != "delivered_at" && name != "delivered_at_ns" && notNull == 0 {
				_ = rows.Close()
				return true, nil
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, err
		}
		_ = rows.Close()
	}
	return false, nil
}

func (store *Store) resetAlertStateAfterMissingKey(ctx context.Context) (bool, error) {
	if !store.resetAlertState {
		return false, nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireAlertDedupeKeyLockTx(ctx, tx); err != nil {
		return false, err
	}
	keyPath := filepath.Join(store.dataDir, "alert-dedupe.key")
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		return false, err
	}
	if err := writeAlertDedupeKeyFingerprintTx(ctx, tx, key); err != nil {
		return false, err
	}
	pendingValue, pending, err := storageMetadataValueTx(ctx, tx, alertDedupeResetPendingMetadata)
	if err != nil {
		return false, err
	}
	tokenFingerprint := store.alertResetTokenFP
	if pending {
		tokenFingerprint = alertDedupeResetPendingTokenFingerprint(pendingValue, store.alertResetToken)
	}
	if tokenFingerprint == "" {
		tokenFingerprint = alertDedupeResetTokenFingerprint(store.alertResetToken)
	}
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	nowNS := now.UnixNano()
	dispatchRows := int64(0)
	eventRows := int64(0)
	for _, table := range []string{"alert_dispatches", "alert_dispatches_legacy"} {
		exists, err := tableExistsTx(ctx, tx, table)
		if err != nil {
			return false, err
		}
		if !exists {
			continue
		}
		dispatches, err := tx.ExecContext(ctx, fmt.Sprintf(`
UPDATE %s
SET status=?,
    worker_id='',
    claim_id='',
    lease_expires_at=NULL,
    lease_expires_at_ns=NULL,
    next_attempt_at_ns=0,
    attempts=CASE WHEN attempts=0 THEN 1 ELSE attempts END,
    last_error=?,
    updated_at=?,
    updated_at_ns=?
WHERE status IN (?, ?)
	`, table), AlertDispatchStatusReset, "alert state reset after missing dedupe key", nowText, nowNS, AlertDispatchStatusPending, AlertDispatchStatusInFlight)
		if err != nil {
			return false, err
		}
		n, err := dispatches.RowsAffected()
		if err != nil {
			return false, err
		}
		dispatchRows += n
	}
	for _, table := range []string{"alert_events", "alert_events_legacy"} {
		exists, err := tableExistsTx(ctx, tx, table)
		if err != nil {
			return false, err
		}
		if !exists {
			continue
		}
		events, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=? WHERE status=?`, table), AlertEventStatusReset, AlertEventStatusPending)
		if err != nil {
			return false, err
		}
		n, err := events.RowsAffected()
		if err != nil {
			return false, err
		}
		eventRows += n
	}
	if err := writeAlertDedupeResetFingerprintConsumedTx(ctx, tx, tokenFingerprint, key); err != nil {
		return false, err
	}
	if err := deleteStorageMetadataTx(ctx, tx, alertDedupeResetPendingMetadata); err != nil {
		return false, err
	}
	if err := verifyAlertDedupeKeyFileAndFingerprintTx(ctx, tx, keyPath, key); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	store.alertDedupeKey = key
	store.alertResetTokenFP = tokenFingerprint
	return dispatchRows > 0 || eventRows > 0, nil
}

func (store *Store) alertDispatchesNeedConstraintRebuild(ctx context.Context) (bool, error) {
	hasUnique, err := store.alertDispatchesHasUniqueEventSink(ctx)
	if err != nil {
		return false, err
	}
	hasFK, err := store.alertDispatchesHasAlertEventsFK(ctx)
	if err != nil {
		return false, err
	}
	return !hasUnique || !hasFK, nil
}

func (store *Store) ensureAlertIndexes(ctx context.Context) (bool, error) {
	if store.alertStateLocked {
		return false, nil
	}
	changed, err := store.reconcileDuplicatePendingAlertDedupeHashes(ctx)
	if err != nil {
		return false, err
	}
	indexChanged, err := store.ensureAlertIndexDefinitions(ctx)
	return changed || indexChanged, err
}

type alertIndexSpec struct {
	name    string
	table   string
	unique  bool
	columns []string
	where   string
	sql     string
}

func alertIndexSpecs() []alertIndexSpec {
	return []alertIndexSpec{
		{name: "idx_alert_events_status_created_at", table: "alert_events", columns: []string{"status", "created_at_ns", "id"}, sql: `CREATE INDEX idx_alert_events_status_created_at ON alert_events(status, created_at_ns, id)`},
		{name: "idx_alert_events_dedupe_hash_created_at", table: "alert_events", columns: []string{"dedupe_hash", "created_at_ns", "id"}, sql: `CREATE INDEX idx_alert_events_dedupe_hash_created_at ON alert_events(dedupe_hash, created_at_ns, id)`},
		{name: "idx_alert_events_pending_dedupe_hash", table: "alert_events", unique: true, columns: []string{"dedupe_hash"}, where: "status='pending' AND dedupe_hash<>''", sql: `CREATE UNIQUE INDEX idx_alert_events_pending_dedupe_hash ON alert_events(dedupe_hash) WHERE status='pending' AND dedupe_hash<>''`},
		{name: "idx_alert_dispatches_event_sink", table: "alert_dispatches", columns: []string{"event_id", "sink"}, sql: `CREATE INDEX idx_alert_dispatches_event_sink ON alert_dispatches(event_id, sink)`},
		{name: "idx_alert_dispatches_claim", table: "alert_dispatches", columns: []string{"status", "lease_expires_at_ns", "id"}, sql: `CREATE INDEX idx_alert_dispatches_claim ON alert_dispatches(status, lease_expires_at_ns, id)`},
		{name: "idx_alert_dispatches_due", table: "alert_dispatches", columns: []string{"status", "next_attempt_at_ns", "id"}, sql: `CREATE INDEX idx_alert_dispatches_due ON alert_dispatches(status, next_attempt_at_ns, id)`},
		{name: "idx_alert_dispatches_claim_id", table: "alert_dispatches", columns: []string{"claim_id"}, sql: `CREATE INDEX idx_alert_dispatches_claim_id ON alert_dispatches(claim_id)`},
	}
}

func (store *Store) ensureAlertIndexDefinitions(ctx context.Context) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, spec := range alertIndexSpecs() {
		ok, err := alertIndexMatchesTx(ctx, tx, spec)
		if err != nil {
			return false, err
		}
		if ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s`, spec.name)); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, spec.sql); err != nil {
			return false, err
		}
		changed = true
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func alertIndexMatchesTx(ctx context.Context, tx *sql.Tx, spec alertIndexSpec) (bool, error) {
	var table string
	var rawSQL sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT tbl_name, sql FROM sqlite_master WHERE type='index' AND name=?`, spec.name).Scan(&table, &rawSQL)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if table != spec.table || !rawSQL.Valid {
		return false, nil
	}
	unique, partial, err := alertIndexFlagsTx(ctx, tx, spec.table, spec.name)
	if err != nil {
		return false, err
	}
	if unique != spec.unique || partial != (spec.where != "") {
		return false, nil
	}
	columns, err := alertIndexColumnsTx(ctx, tx, spec.name)
	if err != nil {
		return false, err
	}
	if !stringSlicesEqual(columns, spec.columns) {
		return false, nil
	}
	return normalizeAlertIndexWhere(rawSQL.String) == normalizeAlertIndexWhere(spec.where), nil
}

func alertIndexFlagsTx(ctx context.Context, tx *sql.Tx, table, name string) (bool, bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_list(%s)`, table))
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var indexName string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &indexName, &unique, &origin, &partial); err != nil {
			return false, false, err
		}
		if indexName == name {
			return unique != 0, partial != 0, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	return false, false, nil
}

func alertIndexColumnsTx(ctx context.Context, tx *sql.Tx, name string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_info(%s)`, name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var seqno, cid int
		var column string
		if err := rows.Scan(&seqno, &cid, &column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func normalizeAlertIndexWhere(sqlText string) string {
	sqlText = strings.TrimSpace(sqlText)
	upper := strings.ToUpper(sqlText)
	if idx := strings.LastIndex(upper, " WHERE "); idx >= 0 {
		sqlText = sqlText[idx+7:]
	} else if strings.HasPrefix(upper, "CREATE ") {
		sqlText = ""
	}
	sqlText = strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
	return strings.Join(strings.Fields(sqlText), " ")
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (store *Store) reconcileDuplicatePendingAlertDedupeHashes(ctx context.Context) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
SELECT dedupe_hash
FROM alert_events
WHERE status=? AND dedupe_hash<>''
GROUP BY dedupe_hash
HAVING COUNT(*) > 1
`, AlertEventStatusPending)
	if err != nil {
		return false, err
	}
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			_ = rows.Close()
			return false, err
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, hash := range hashes {
		eventRows, err := tx.QueryContext(ctx, `
SELECT id
FROM alert_events
WHERE status=? AND dedupe_hash=?
ORDER BY created_at_ns DESC, id DESC
`, AlertEventStatusPending, hash)
		if err != nil {
			return false, err
		}
		var ids []int64
		for eventRows.Next() {
			var id int64
			if err := eventRows.Scan(&id); err != nil {
				_ = eventRows.Close()
				return false, err
			}
			ids = append(ids, id)
		}
		if err := eventRows.Close(); err != nil {
			return false, err
		}
		if len(ids) < 2 {
			continue
		}
		keeperID := ids[0]
		for _, duplicateID := range ids[1:] {
			if err := store.mergeDuplicateAlertDispatches(ctx, tx, keeperID, duplicateID); err != nil {
				return false, err
			}
			if err := resetActiveAlertDispatchesForEvent(ctx, tx, duplicateID); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE alert_events SET status=? WHERE id=?`, AlertEventStatusFailed, duplicateID); err != nil {
				return false, err
			}
			changed = true
		}
		if err := store.syncAlertEventStatus(ctx, tx, keeperID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

type alertDispatchMergeRow struct {
	id               int64
	status           string
	workerID         string
	claimID          string
	leaseExpiresAt   any
	leaseExpiresAtNS any
	nextAttemptAtNS  int64
	attempts         int
	lastError        string
	deliveredAt      any
	deliveredAtNS    any
	updatedAt        string
	updatedAtNS      int64
}

func (store *Store) mergeDuplicateAlertDispatches(ctx context.Context, tx *sql.Tx, keeperID, duplicateID int64) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id, sink, status, worker_id, claim_id, lease_expires_at, lease_expires_at_ns,
       next_attempt_at_ns, attempts, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns
FROM alert_dispatches
WHERE event_id=?
ORDER BY sink ASC, id ASC
`, duplicateID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var duplicate alertDispatchMergeRow
		var sink string
		if err := rows.Scan(
			&duplicate.id,
			&sink,
			&duplicate.status,
			&duplicate.workerID,
			&duplicate.claimID,
			&duplicate.leaseExpiresAt,
			&duplicate.leaseExpiresAtNS,
			&duplicate.nextAttemptAtNS,
			&duplicate.attempts,
			&duplicate.lastError,
			&duplicate.deliveredAt,
			&duplicate.deliveredAtNS,
			&duplicate.updatedAt,
			&duplicate.updatedAtNS,
		); err != nil {
			return err
		}
		existing, ok, err := alertDispatchForEventSink(ctx, tx, keeperID, sink)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := tx.ExecContext(ctx, `UPDATE alert_dispatches SET event_id=? WHERE id=?`, keeperID, duplicate.id); err != nil {
				return err
			}
			continue
		}
		if existing.status == AlertDispatchStatusDelivered {
			if activeAlertDispatchStatus(duplicate.status) {
				if err := resetDuplicateAlertDispatch(ctx, tx, duplicate.id); err != nil {
					return err
				}
			}
			continue
		}
		if duplicate.status == AlertDispatchStatusDelivered {
			if activeAlertDispatchStatus(existing.status) {
				if err := resetDuplicateAlertDispatch(ctx, tx, existing.id); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM alert_dispatches WHERE id=?`, existing.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE alert_dispatches SET event_id=? WHERE id=?`, keeperID, duplicate.id); err != nil {
				return err
			}
			continue
		}
		if alertDispatchMergeBetter(duplicate, existing) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM alert_dispatches WHERE id=?`, existing.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE alert_dispatches SET event_id=? WHERE id=?`, keeperID, duplicate.id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM alert_dispatches WHERE id=?`, duplicate.id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func resetDuplicateAlertDispatch(ctx context.Context, tx *sql.Tx, dispatchID int64) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
UPDATE alert_dispatches
SET status=?,
    worker_id='',
    claim_id='',
    lease_expires_at=NULL,
    lease_expires_at_ns=NULL,
    next_attempt_at_ns=0,
    last_error=?,
    updated_at=?,
    updated_at_ns=?
WHERE id=?
`, AlertDispatchStatusReset, "duplicate alert dispatch reset because delivered dispatch already exists", now.Format(time.RFC3339Nano), now.UnixNano(), dispatchID)
	return err
}

func resetActiveAlertDispatchesForEvent(ctx context.Context, tx *sql.Tx, eventID int64) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
UPDATE alert_dispatches
SET status=?,
    worker_id='',
    claim_id='',
    lease_expires_at=NULL,
    lease_expires_at_ns=NULL,
    next_attempt_at_ns=0,
    last_error=CASE WHEN last_error='' THEN ? ELSE last_error END,
    updated_at=?,
    updated_at_ns=?
WHERE event_id=? AND status IN (?, ?)
`, AlertDispatchStatusReset, "duplicate alert dispatch reset after dedupe merge", now.Format(time.RFC3339Nano), now.UnixNano(), eventID, AlertDispatchStatusPending, AlertDispatchStatusInFlight)
	return err
}

func activeAlertDispatchStatus(status string) bool {
	return status == AlertDispatchStatusPending || status == AlertDispatchStatusInFlight
}

func alertDispatchForEventSink(ctx context.Context, tx *sql.Tx, eventID int64, sink string) (alertDispatchMergeRow, bool, error) {
	var row alertDispatchMergeRow
	err := tx.QueryRowContext(ctx, `
SELECT id, status, worker_id, claim_id, lease_expires_at, lease_expires_at_ns,
       next_attempt_at_ns, attempts, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns
FROM alert_dispatches
WHERE event_id=? AND sink=?
`, eventID, sink).Scan(
		&row.id,
		&row.status,
		&row.workerID,
		&row.claimID,
		&row.leaseExpiresAt,
		&row.leaseExpiresAtNS,
		&row.nextAttemptAtNS,
		&row.attempts,
		&row.lastError,
		&row.deliveredAt,
		&row.deliveredAtNS,
		&row.updatedAt,
		&row.updatedAtNS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return alertDispatchMergeRow{}, false, nil
	}
	if err != nil {
		return alertDispatchMergeRow{}, false, err
	}
	return row, true, nil
}

func alertDispatchMergeBetter(candidate, current alertDispatchMergeRow) bool {
	if current.status == AlertDispatchStatusDelivered {
		return false
	}
	if candidate.status == AlertDispatchStatusDelivered {
		return true
	}
	candidatePriority := alertDispatchMergePriority(candidate.status)
	currentPriority := alertDispatchMergePriority(current.status)
	if candidatePriority != currentPriority {
		return candidatePriority > currentPriority
	}
	if candidate.updatedAtNS != current.updatedAtNS {
		return candidate.updatedAtNS > current.updatedAtNS
	}
	return candidate.id > current.id
}

func alertDispatchMergePriority(status string) int {
	switch status {
	case AlertDispatchStatusInFlight:
		return 3
	case AlertDispatchStatusPending:
		return 2
	case AlertDispatchStatusDeadLettered:
		return 1
	default:
		return 0
	}
}

func (store *Store) backfillAlertEventDedupeHashes(ctx context.Context) (bool, error) {
	if store.alertStateLocked {
		return false, nil
	}
	rows, err := store.db.QueryContext(ctx, `SELECT id, dedupe_key FROM alert_events WHERE dedupe_hash=''`)
	if err != nil {
		return false, err
	}
	type dedupeBackfill struct {
		id  int64
		key string
	}
	var updates []dedupeBackfill
	for rows.Next() {
		var update dedupeBackfill
		if err := rows.Scan(&update.id, &update.key); err != nil {
			_ = rows.Close()
			return false, err
		}
		updates = append(updates, update)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, update := range updates {
		if _, err := store.db.ExecContext(ctx, `UPDATE alert_events SET dedupe_hash=? WHERE id=?`, store.alertDedupeHash(update.key), update.id); err != nil {
			return false, err
		}
	}
	return len(updates) > 0, nil
}

func (store *Store) backfillAlertDedupeHashesBeforeRedaction(ctx context.Context) error {
	if store.alertStateLocked || len(store.alertDedupeKey) == 0 {
		return nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"alert_events", "alert_events_legacy"} {
		exists, err := tableExistsTx(ctx, tx, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		hasDedupeKey, err := columnExistsTx(ctx, tx, table, "dedupe_key")
		if err != nil {
			return err
		}
		if !hasDedupeKey {
			continue
		}
		if _, err := ensureColumnChangedTx(ctx, tx, table, "dedupe_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT rowid, dedupe_key FROM %s WHERE dedupe_hash=''`, table))
		if err != nil {
			return err
		}
		type update struct {
			rowID int64
			key   string
		}
		var updates []update
		for rows.Next() {
			var next update
			if err := rows.Scan(&next.rowID, &next.key); err != nil {
				_ = rows.Close()
				return err
			}
			updates = append(updates, next)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, update := range updates {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET dedupe_hash=? WHERE rowid=?`, table), store.alertDedupeHash(update.key), update.rowID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (store *Store) backfillAlertEventCreatedAtNS(ctx context.Context) (bool, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT id, created_at FROM alert_events WHERE created_at_ns=0`)
	if err != nil {
		return false, err
	}
	type eventTimeBackfill struct {
		id        int64
		createdAt string
	}
	var updates []eventTimeBackfill
	for rows.Next() {
		var update eventTimeBackfill
		if err := rows.Scan(&update.id, &update.createdAt); err != nil {
			_ = rows.Close()
			return false, err
		}
		updates = append(updates, update)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, update := range updates {
		createdAtNS := parseAlertTimestampNS(update.createdAt)
		if createdAtNS == 0 {
			createdAtNS = time.Now().UTC().UnixNano()
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE alert_events SET created_at_ns=? WHERE id=?`, createdAtNS, update.id); err != nil {
			return false, err
		}
	}
	return len(updates) > 0, nil
}

func (store *Store) backfillAlertDispatchTimestampNS(ctx context.Context) (bool, error) {
	changed := false
	for _, backfill := range []struct {
		textColumn string
		nsColumn   string
		nullable   bool
	}{
		{textColumn: "lease_expires_at", nsColumn: "lease_expires_at_ns", nullable: true},
		{textColumn: "delivered_at", nsColumn: "delivered_at_ns", nullable: true},
		{textColumn: "updated_at", nsColumn: "updated_at_ns", nullable: false},
	} {
		columnChanged, err := store.backfillAlertDispatchTimestampColumnNS(ctx, backfill.textColumn, backfill.nsColumn, backfill.nullable)
		if err != nil {
			return false, err
		}
		changed = changed || columnChanged
	}
	return changed, nil
}

func (store *Store) backfillAlertDispatchTimestampColumnNS(ctx context.Context, textColumn, nsColumn string, nullable bool) (bool, error) {
	query := fmt.Sprintf(`SELECT id, %s FROM alert_dispatches WHERE COALESCE(%s, 0)=0`, textColumn, nsColumn)
	if nullable {
		query += fmt.Sprintf(` AND %s IS NOT NULL AND %s<>''`, textColumn, textColumn)
	}
	rows, err := store.db.QueryContext(ctx, query)
	if err != nil {
		return false, err
	}
	type dispatchTimeBackfill struct {
		id    int64
		value sql.NullString
	}
	var updates []dispatchTimeBackfill
	for rows.Next() {
		var update dispatchTimeBackfill
		if err := rows.Scan(&update.id, &update.value); err != nil {
			_ = rows.Close()
			return false, err
		}
		updates = append(updates, update)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, update := range updates {
		timestampNS := int64(0)
		if update.value.Valid {
			timestampNS = parseAlertTimestampNS(update.value.String)
		}
		if timestampNS == 0 && !nullable {
			timestampNS = time.Now().UTC().UnixNano()
		}
		if timestampNS == 0 {
			continue
		}
		if _, err := store.db.ExecContext(ctx, fmt.Sprintf(`UPDATE alert_dispatches SET %s=? WHERE id=?`, nsColumn), timestampNS, update.id); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func (store *Store) alertEventsHasUniqueDedupeKey(ctx context.Context) (bool, error) {
	rows, err := store.db.QueryContext(ctx, `PRAGMA index_list(alert_events)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if unique == 0 {
			continue
		}
		matches, err := store.indexColumnsMatch(ctx, name, []string{"dedupe_key"})
		if err != nil {
			return false, err
		}
		if matches {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (store *Store) alertDispatchesHasUniqueEventSink(ctx context.Context) (bool, error) {
	rows, err := store.db.QueryContext(ctx, `PRAGMA index_list(alert_dispatches)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if unique == 0 || partial != 0 {
			continue
		}
		matches, err := store.indexColumnsMatch(ctx, name, []string{"event_id", "sink"})
		if err != nil {
			return false, err
		}
		if matches {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (store *Store) alertDispatchesHasAlertEventsFK(ctx context.Context) (bool, error) {
	rows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_list(alert_dispatches)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return false, err
		}
		if table == "alert_events" && from == "event_id" && to == "id" && strings.EqualFold(onDelete, "CASCADE") {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (store *Store) indexColumnsMatch(ctx context.Context, index string, want []string) (bool, error) {
	rows, err := store.db.QueryContext(ctx, "PRAGMA index_info("+index+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var seqno, cid int
		var name string
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return false, err
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(got) != len(want) {
		return false, nil
	}
	for i := range want {
		if got[i] != want[i] {
			return false, nil
		}
	}
	return true, nil
}

func (store *Store) tableExists(ctx context.Context, table string) (bool, error) {
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (store *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := store.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (store *Store) rebuildAlertTables(ctx context.Context) (err error) {
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return err
	}
	foreignKeysDisabled := false
	var tx *sql.Tx
	defer func() {
		if tx != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, rollbackErr)
			}
		}
		if foreignKeysDisabled {
			if restoreErr := store.restoreAndVerifyAlertForeignKeys(context.Background(), conn); restoreErr != nil {
				err = errors.Join(err, restoreErr, discardSQLiteConn(conn))
			}
		}
		err = errors.Join(err, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	foreignKeysDisabled = true

	tx, err = conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	currentDispatches, err := tableExistsTx(ctx, tx, "alert_dispatches")
	if err != nil {
		return err
	}
	legacyDispatches, err := tableExistsTx(ctx, tx, "alert_dispatches_legacy")
	if err != nil {
		return err
	}
	if legacyDispatches && currentDispatches {
		legacyCount, err := tableRowCountTx(ctx, tx, "alert_dispatches_legacy")
		if err != nil {
			return err
		}
		currentCount, err := tableRowCountTx(ctx, tx, "alert_dispatches")
		if err != nil {
			return err
		}
		switch {
		case legacyCount > 0 && currentCount > 0:
			store.lockAlertState("alert state locked: alert table rebuild found both alert_dispatches and alert_dispatches_legacy with rows; manual repair required before alerting can resume")
			return nil
		case currentCount == 0:
			if _, err := tx.ExecContext(ctx, `DROP TABLE alert_dispatches`); err != nil {
				return err
			}
			currentDispatches = false
		case legacyCount == 0:
			if _, err := tx.ExecContext(ctx, `DROP TABLE alert_dispatches_legacy`); err != nil {
				return err
			}
			legacyDispatches = false
		}
	}
	if !legacyDispatches && currentDispatches {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_dispatches RENAME TO alert_dispatches_legacy`); err != nil {
			return err
		}
		legacyDispatches = true
	}

	currentEvents, err := tableExistsTx(ctx, tx, "alert_events")
	if err != nil {
		return err
	}
	legacyEvents, err := tableExistsTx(ctx, tx, "alert_events_legacy")
	if err != nil {
		return err
	}
	if legacyEvents && currentEvents {
		legacyCount, err := tableRowCountTx(ctx, tx, "alert_events_legacy")
		if err != nil {
			return err
		}
		currentCount, err := tableRowCountTx(ctx, tx, "alert_events")
		if err != nil {
			return err
		}
		switch {
		case legacyCount > 0 && currentCount > 0:
			store.lockAlertState("alert state locked: alert table rebuild found both alert_events and alert_events_legacy with rows; manual repair required before alerting can resume")
			return nil
		case currentCount == 0:
			if _, err := tx.ExecContext(ctx, `DROP TABLE alert_events`); err != nil {
				return err
			}
			currentEvents = false
		case legacyCount == 0:
			if _, err := tx.ExecContext(ctx, `DROP TABLE alert_events_legacy`); err != nil {
				return err
			}
			legacyEvents = false
		}
	}
	if !legacyEvents && currentEvents {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE alert_events RENAME TO alert_events_legacy`); err != nil {
			return err
		}
		legacyEvents = true
	}
	if !legacyEvents {
		return errors.New("alert_events table is unavailable for rebuild")
	}
	if err := store.repairAlertEventsTableInTx(ctx, tx, "alert_events_legacy"); err != nil {
		return err
	}
	if legacyDispatches {
		if err := store.repairAlertDispatchesTableInTx(ctx, tx, "alert_dispatches_legacy"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, alertEventsCreateSQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO alert_events(
  id, kind, service_id, target, repository, agent, old_state, new_state, reason,
  dedupe_key, dedupe_hash, created_at, created_at_ns, status
)
SELECT
  id, kind, service_id, target, repository, agent, old_state, new_state, reason,
  dedupe_key, dedupe_hash, created_at, created_at_ns, status
FROM alert_events_legacy
`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, alertDispatchesCreateSQL); err != nil {
		return err
	}
	if legacyDispatches {
		if err := store.copyMergedAlertDispatchesFromLegacy(ctx, tx); err != nil {
			return err
		}
		if err := store.syncAllAlertEventStatuses(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE alert_dispatches_legacy`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE alert_events_legacy`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	tx = nil
	return nil
}

func (store *Store) restoreAndVerifyAlertForeignKeys(ctx context.Context, conn *sql.Conn) error {
	if store.restoreAlertForeignKeys != nil {
		return store.restoreAlertForeignKeys(ctx, conn)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return fmt.Errorf("restore sqlite foreign keys: %w", err)
	}
	var enabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return fmt.Errorf("verify sqlite foreign keys: %w", err)
	}
	if enabled != 1 {
		return errors.New("sqlite foreign keys remain disabled after alert table rebuild")
	}
	if err := checkSQLiteForeignKeys(ctx, conn); err != nil {
		return err
	}
	return nil
}

func discardSQLiteConn(conn *sql.Conn) error {
	return conn.Raw(func(any) error { return driver.ErrBadConn })
}

func (store *Store) copyMergedAlertDispatchesFromLegacy(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT d.id, d.event_id, d.sink, d.status, d.worker_id, d.claim_id, d.lease_expires_at, d.lease_expires_at_ns,
       d.next_attempt_at_ns, d.attempts, d.last_error, d.delivered_at, d.delivered_at_ns, d.updated_at, d.updated_at_ns
FROM alert_dispatches_legacy d
WHERE EXISTS (SELECT 1 FROM alert_events e WHERE e.id=d.event_id)
ORDER BY d.event_id ASC, d.sink ASC, d.id ASC
`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type dispatchKey struct {
		eventID int64
		sink    string
	}
	type dispatchCandidate struct {
		eventID int64
		sink    string
		row     alertDispatchMergeRow
	}
	merged := map[dispatchKey]dispatchCandidate{}
	for rows.Next() {
		var candidate dispatchCandidate
		if err := rows.Scan(
			&candidate.row.id,
			&candidate.eventID,
			&candidate.sink,
			&candidate.row.status,
			&candidate.row.workerID,
			&candidate.row.claimID,
			&candidate.row.leaseExpiresAt,
			&candidate.row.leaseExpiresAtNS,
			&candidate.row.nextAttemptAtNS,
			&candidate.row.attempts,
			&candidate.row.lastError,
			&candidate.row.deliveredAt,
			&candidate.row.deliveredAtNS,
			&candidate.row.updatedAt,
			&candidate.row.updatedAtNS,
		); err != nil {
			return err
		}
		candidate.sink = strings.TrimSpace(candidate.sink)
		if candidate.sink == "" {
			return fmt.Errorf("legacy alert dispatch %d has an empty sink identity", candidate.row.id)
		}
		if err := store.validateAlertSinkIdentity(candidate.sink); err != nil {
			return fmt.Errorf("legacy alert dispatch %d: %w", candidate.row.id, err)
		}
		key := dispatchKey{eventID: candidate.eventID, sink: candidate.sink}
		existing, ok := merged[key]
		if !ok || alertDispatchMergeBetter(candidate.row, existing.row) {
			merged[key] = candidate
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, candidate := range merged {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO alert_dispatches(
  id, event_id, sink, status, worker_id, claim_id, lease_expires_at, lease_expires_at_ns,
  next_attempt_at_ns, attempts, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, candidate.row.id, candidate.eventID, candidate.sink, candidate.row.status, candidate.row.workerID, candidate.row.claimID,
			candidate.row.leaseExpiresAt, candidate.row.leaseExpiresAtNS, candidate.row.nextAttemptAtNS, candidate.row.attempts,
			candidate.row.lastError, candidate.row.deliveredAt, candidate.row.deliveredAtNS, candidate.row.updatedAt, candidate.row.updatedAtNS); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) syncAllAlertEventStatuses(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM alert_events`)
	if err != nil {
		return err
	}
	var eventIDs []int64
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return err
		}
		eventIDs = append(eventIDs, eventID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, eventID := range eventIDs {
		if err := store.syncAlertEventStatus(ctx, tx, eventID); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) repairAlertEventsTableInTx(ctx context.Context, tx *sql.Tx, table string) error {
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "kind", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "service_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "target", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "repository", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "agent", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "old_state", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "new_state", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "reason", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "dedupe_key", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "dedupe_hash", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "created_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "created_at_ns", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "status", definition: "TEXT NOT NULL DEFAULT 'pending'"},
	} {
		if _, err := ensureColumnChangedTx(ctx, tx, table, column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []string{"kind", "service_id", "target", "repository", "agent", "old_state", "new_state", "reason", "dedupe_key", "dedupe_hash", "created_at", "status"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s='' WHERE %s IS NULL`, table, column, column)); err != nil {
			return err
		}
	}
	for _, column := range []string{"created_at_ns"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s=0 WHERE %s IS NULL`, table, column, column)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=? WHERE status NOT IN (?, ?, ?, ?, ?)`, table), AlertEventStatusPending, AlertEventStatusPending, AlertEventStatusDelivered, AlertEventStatusFailed, AlertEventStatusPartial, AlertEventStatusReset); err != nil {
		return err
	}
	return store.backfillAlertEventsTableInTx(ctx, tx, table)
}

func (store *Store) repairAlertDispatchesTableInTx(ctx context.Context, tx *sql.Tx, table string) error {
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "event_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "sink", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "status", definition: "TEXT NOT NULL DEFAULT 'pending'"},
		{name: "worker_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "claim_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "lease_expires_at", definition: "TEXT"},
		{name: "lease_expires_at_ns", definition: "INTEGER"},
		{name: "next_attempt_at_ns", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "attempts", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "last_error", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "delivered_at", definition: "TEXT"},
		{name: "delivered_at_ns", definition: "INTEGER"},
		{name: "updated_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "updated_at_ns", definition: "INTEGER NOT NULL DEFAULT 0"},
	} {
		if _, err := ensureColumnChangedTx(ctx, tx, table, column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []string{"sink", "status", "worker_id", "claim_id", "last_error", "updated_at"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s='' WHERE %s IS NULL`, table, column, column)); err != nil {
			return err
		}
	}
	for _, column := range []string{"event_id", "next_attempt_at_ns", "attempts", "updated_at_ns"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s=0 WHERE %s IS NULL`, table, column, column)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=? WHERE status NOT IN (?, ?, ?, ?, ?)`, table), AlertDispatchStatusPending, AlertDispatchStatusPending, AlertDispatchStatusInFlight, AlertDispatchStatusDelivered, AlertDispatchStatusDeadLettered, AlertDispatchStatusReset); err != nil {
		return err
	}
	return backfillAlertDispatchesTableInTx(ctx, tx, table)
}

func (store *Store) backfillAlertEventsTableInTx(ctx context.Context, tx *sql.Tx, table string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id, dedupe_key, created_at FROM %s WHERE dedupe_hash='' OR created_at_ns=0`, table))
	if err != nil {
		return err
	}
	type eventBackfill struct {
		id        int64
		dedupeKey string
		createdAt string
	}
	var updates []eventBackfill
	for rows.Next() {
		var update eventBackfill
		if err := rows.Scan(&update.id, &update.dedupeKey, &update.createdAt); err != nil {
			_ = rows.Close()
			return err
		}
		updates = append(updates, update)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		createdAtNS := parseAlertTimestampNS(update.createdAt)
		if createdAtNS == 0 {
			createdAtNS = time.Now().UTC().UnixNano()
		}
		dedupeHash := ""
		if !store.alertStateLocked {
			dedupeHash = store.alertDedupeHash(update.dedupeKey)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET dedupe_hash=CASE WHEN dedupe_hash='' THEN ? ELSE dedupe_hash END, created_at_ns=CASE WHEN created_at_ns=0 THEN ? ELSE created_at_ns END WHERE id=?`, table), dedupeHash, createdAtNS, update.id); err != nil {
			return err
		}
	}
	return nil
}

func backfillAlertDispatchesTableInTx(ctx context.Context, tx *sql.Tx, table string) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id, lease_expires_at, delivered_at, updated_at FROM %s WHERE COALESCE(lease_expires_at_ns, 0)=0 OR COALESCE(delivered_at_ns, 0)=0 OR updated_at_ns=0`, table))
	if err != nil {
		return err
	}
	type dispatchBackfill struct {
		id             int64
		leaseExpiresAt sql.NullString
		deliveredAt    sql.NullString
		updatedAt      string
	}
	var updates []dispatchBackfill
	for rows.Next() {
		var update dispatchBackfill
		if err := rows.Scan(&update.id, &update.leaseExpiresAt, &update.deliveredAt, &update.updatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		updates = append(updates, update)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		var leaseExpiresAtNS any
		if update.leaseExpiresAt.Valid {
			if parsed := parseAlertTimestampNS(update.leaseExpiresAt.String); parsed != 0 {
				leaseExpiresAtNS = parsed
			}
		}
		var deliveredAtNS any
		if update.deliveredAt.Valid {
			if parsed := parseAlertTimestampNS(update.deliveredAt.String); parsed != 0 {
				deliveredAtNS = parsed
			}
		}
		updatedAtNS := parseAlertTimestampNS(update.updatedAt)
		if updatedAtNS == 0 {
			updatedAtNS = time.Now().UTC().UnixNano()
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET lease_expires_at_ns=CASE WHEN COALESCE(lease_expires_at_ns, 0)=0 THEN ? ELSE lease_expires_at_ns END, delivered_at_ns=CASE WHEN COALESCE(delivered_at_ns, 0)=0 THEN ? ELSE delivered_at_ns END, updated_at_ns=CASE WHEN updated_at_ns=0 THEN ? ELSE updated_at_ns END WHERE id=?`, table), leaseExpiresAtNS, deliveredAtNS, updatedAtNS, update.id); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) reconcileLegacyAlertEventStatuses(ctx context.Context) (bool, error) {
	reopened, err := store.db.ExecContext(ctx, `
UPDATE alert_events
SET status=?
WHERE status IN (?, ?, ?, ?)
  AND EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status IN (?, ?)
  )
`, AlertEventStatusPending, AlertEventStatusDelivered, AlertEventStatusFailed, AlertEventStatusPartial, AlertEventStatusReset, AlertDispatchStatusPending, AlertDispatchStatusInFlight)
	if err != nil {
		return false, err
	}
	reopenedRows, err := reopened.RowsAffected()
	if err != nil {
		return false, err
	}
	closed, err := store.db.ExecContext(ctx, `
UPDATE alert_events
SET status=CASE
	WHEN EXISTS (
	  SELECT 1 FROM alert_dispatches d
	  WHERE d.event_id=alert_events.id AND d.status=?
	) THEN ?
  WHEN EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) AND NOT EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) THEN ?
  WHEN EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) AND EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) THEN ?
  WHEN EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) THEN ?
  ELSE ?
END
WHERE NOT EXISTS (
  SELECT 1 FROM alert_dispatches d
  WHERE d.event_id=alert_events.id AND d.status IN (?, ?)
)
  AND status<>CASE
	WHEN EXISTS (
	  SELECT 1 FROM alert_dispatches d
	  WHERE d.event_id=alert_events.id AND d.status=?
	) THEN ?
    WHEN EXISTS (
      SELECT 1 FROM alert_dispatches d
      WHERE d.event_id=alert_events.id AND d.status=?
    ) AND NOT EXISTS (
      SELECT 1 FROM alert_dispatches d
      WHERE d.event_id=alert_events.id AND d.status=?
    ) THEN ?
    WHEN EXISTS (
      SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) AND EXISTS (
    SELECT 1 FROM alert_dispatches d
    WHERE d.event_id=alert_events.id AND d.status=?
  ) THEN ?
    WHEN EXISTS (
      SELECT 1 FROM alert_dispatches d
      WHERE d.event_id=alert_events.id AND d.status=?
    ) THEN ?
    ELSE ?
  END
	`, AlertDispatchStatusReset, AlertEventStatusReset,
		AlertDispatchStatusDelivered, AlertDispatchStatusDeadLettered, AlertEventStatusDelivered,
		AlertDispatchStatusDelivered, AlertDispatchStatusDeadLettered, AlertEventStatusPartial,
		AlertDispatchStatusReset, AlertEventStatusReset,
		AlertEventStatusFailed, AlertDispatchStatusPending, AlertDispatchStatusInFlight,
		AlertDispatchStatusReset, AlertEventStatusReset,
		AlertDispatchStatusDelivered, AlertDispatchStatusDeadLettered, AlertEventStatusDelivered,
		AlertDispatchStatusDelivered, AlertDispatchStatusDeadLettered, AlertEventStatusPartial,
		AlertDispatchStatusReset, AlertEventStatusReset,
		AlertEventStatusFailed)
	if err != nil {
		return false, err
	}
	closedRows, err := closed.RowsAffected()
	if err != nil {
		return false, err
	}
	return reopenedRows > 0 || closedRows > 0, nil
}

func tableExistsTx(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func tableRowCountTx(ctx context.Context, tx *sql.Tx, table string) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func columnExistsTx(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func ensureColumnChangedTx(ctx context.Context, tx *sql.Tx, table, column, definition string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err == nil, err
}

func checkSQLiteForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("sqlite foreign_key_check reported alert table violations")
	}
	return rows.Err()
}

func parseAlertTimestampNS(value string) int64 {
	if value == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return 0
		}
	}
	return parsed.UTC().UnixNano()
}

const alertEventsCreateSQL = `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
)`

const alertDispatchesCreateSQL = `
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
)`

const migrations = `
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS storage_metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repositories (
  name TEXT PRIMARY KEY,
  url TEXT NOT NULL,
  default_ref TEXT NOT NULL,
  last_commit TEXT,
  last_scan_at TEXT,
  status TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS scans (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repository TEXT NOT NULL,
  status TEXT NOT NULL,
  commit_sha TEXT,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS services (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  repository TEXT NOT NULL,
  source_commit TEXT NOT NULL,
  source_path TEXT NOT NULL,
  runtime TEXT NOT NULL,
  kind TEXT NOT NULL,
  namespace TEXT NOT NULL,
  resource_name TEXT NOT NULL,
  environment TEXT NOT NULL,
  health TEXT NOT NULL,
  images_json TEXT NOT NULL,
  ports_json TEXT NOT NULL,
  dependencies_json TEXT NOT NULL,
  storage_json TEXT NOT NULL,
	  exposure_json TEXT NOT NULL,
	  config_json TEXT NOT NULL DEFAULT '[]',
	  compose_project TEXT NOT NULL DEFAULT '',
	  warnings_json TEXT NOT NULL
	);

CREATE TABLE IF NOT EXISTS status_results (
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  health TEXT NOT NULL,
  message TEXT NOT NULL,
  checked_at TEXT NOT NULL,
  observed_images_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY(service_id, target)
);

CREATE TABLE IF NOT EXISTS agents (
  target TEXT PRIMARY KEY,
  last_seen_at TEXT NOT NULL,
  status_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS status_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  health TEXT NOT NULL,
  message TEXT NOT NULL,
  checked_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_status_history_lookup
ON status_history(service_id, target, checked_at);

CREATE INDEX IF NOT EXISTS idx_status_history_checked_at_lookup
ON status_history(checked_at, service_id, target);

CREATE TABLE IF NOT EXISTS monitor_overrides (
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  not_applicable INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(service_id, target)
);

CREATE TABLE IF NOT EXISTS route_target_exclusions (
  service_id TEXT NOT NULL,
  old_route TEXT NOT NULL,
  PRIMARY KEY(service_id, old_route)
);

CREATE INDEX IF NOT EXISTS idx_route_target_exclusions_service_route
ON route_target_exclusions(service_id, old_route);

-- Route scans must not be held hostage by an alert-state safety lock.  These
-- rows retain the exact alert retarget work until a later successful scan can
-- validate the alert dedupe key and apply it atomically.
CREATE TABLE IF NOT EXISTS deferred_alert_route_reconciliations (
  service_id TEXT NOT NULL,
  old_target TEXT NOT NULL,
  new_target TEXT NOT NULL,
  PRIMARY KEY(service_id, old_target, new_target)
);

CREATE TABLE IF NOT EXISTS alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);

CREATE TABLE IF NOT EXISTS alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
`
