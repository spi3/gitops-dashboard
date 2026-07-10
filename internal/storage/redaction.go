package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/example/gitops-dashboard/internal/sanitizer"
)

var compactRedactedPagesObserver func()

func notifyCompactRedactedPages() {
	if compactRedactedPagesObserver != nil {
		compactRedactedPagesObserver()
	}
}

func (store *Store) AddRedactionValues(values ...string) {
	store.redactionMu.Lock()
	defer store.redactionMu.Unlock()
	if store.redactionTokenSet == nil {
		store.redactionTokenSet = map[string]struct{}{}
		for _, value := range store.redactionRawValues {
			store.redactionTokenSet[value] = struct{}{}
		}
	}
	changed := false
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := store.redactionTokenSet[value]; ok {
			continue
		}
		store.redactionTokenSet[value] = struct{}{}
		store.redactionRawValues = append(store.redactionRawValues, value)
		changed = true
	}
	if changed {
		store.redactionTokens = sanitizer.New(store.redactionRawValues...).Values()
	}
}

func (store *Store) redactionValues() []string {
	store.redactionMu.RLock()
	defer store.redactionMu.RUnlock()
	values := make([]string, len(store.redactionTokens))
	copy(values, store.redactionTokens)
	return values
}

func (store *Store) redact(value string) string {
	return sanitizer.Redact(value, store.redactionValues()...)
}

func (store *Store) RedactPersistedSensitiveValues(ctx context.Context) error {
	coreChanged, err := store.redactCorePersistedSensitiveValues(ctx)
	if err != nil {
		return err
	}
	alertChanged, err := store.redactAlertPersistedSensitiveValues(ctx)
	if err != nil {
		return err
	}
	if !coreChanged && !alertChanged {
		return nil
	}
	return store.compactRedactedPages(ctx)
}

func (store *Store) redactCorePersistedSensitiveValues(ctx context.Context) (bool, error) {
	return store.redactPersistedColumns(ctx, []struct{ table, column string }{
		{"repositories", "url"}, {"repositories", "error"}, {"scans", "error"},
		{"status_results", "message"}, {"status_history", "message"},
	})
}

func (store *Store) redactAlertPersistedSensitiveValues(ctx context.Context) (bool, error) {
	return store.redactPersistedColumns(ctx, []struct{ table, column string }{
		{"alert_events", "kind"}, {"alert_events", "service_id"}, {"alert_events", "target"}, {"alert_events", "repository"}, {"alert_events", "agent"}, {"alert_events", "old_state"}, {"alert_events", "new_state"}, {"alert_events", "reason"}, {"alert_events", "dedupe_key"},
		{"alert_events_legacy", "kind"}, {"alert_events_legacy", "service_id"}, {"alert_events_legacy", "target"}, {"alert_events_legacy", "repository"}, {"alert_events_legacy", "agent"}, {"alert_events_legacy", "old_state"}, {"alert_events_legacy", "new_state"}, {"alert_events_legacy", "reason"}, {"alert_events_legacy", "dedupe_key"},
		{"alert_dispatches", "worker_id"}, {"alert_dispatches", "last_error"}, {"alert_dispatches_legacy", "worker_id"}, {"alert_dispatches_legacy", "last_error"},
	})
}

func (store *Store) redactPersistedColumns(ctx context.Context, targets []struct{ table, column string }) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, target := range targets {
		var columnChanged bool
		var err error
		if store.alertStateLocked && alertEventRedactionTable(target.table) && target.column == "dedupe_key" {
			columnChanged, err = store.redactAlertDedupeKeyColumn(ctx, tx, target.table)
		} else {
			columnChanged, err = store.redactColumn(ctx, tx, target.table, target.column)
		}
		if err != nil {
			return false, err
		}
		changed = changed || columnChanged
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func (store *Store) lockedAlertRowsNeedRawDedupeKey(ctx context.Context) (bool, error) {
	if !store.alertStateLocked {
		return false, nil
	}
	for _, table := range []string{"alert_events", "alert_events_legacy"} {
		exists, err := sqliteTableExists(ctx, store.db, table)
		if err != nil {
			return false, err
		}
		if !exists {
			continue
		}
		hasDedupeKey, err := sqliteTableHasColumn(ctx, store.db, table, "dedupe_key")
		if err != nil {
			return false, err
		}
		if !hasDedupeKey {
			continue
		}
		hasHash, err := sqliteTableHasColumn(ctx, store.db, table, "dedupe_hash")
		if err != nil {
			return false, err
		}
		var count int
		if !hasHash {
			if err := store.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE COALESCE(dedupe_key, '')<>''`, table)).Scan(&count); err != nil {
				return false, err
			}
		} else if err := store.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE COALESCE(dedupe_key, '')<>'' AND COALESCE(dedupe_hash, '')=''`, table)).Scan(&count); err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

func alertEventRedactionTable(table string) bool {
	return table == "alert_events" || table == "alert_events_legacy"
}

func (store *Store) redactAlertDedupeKeyColumn(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	exists, err := redactionColumnExists(ctx, tx, table, "dedupe_key")
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	hasHash, err := redactionColumnExists(ctx, tx, table, "dedupe_hash")
	if err != nil {
		return false, err
	}
	if !hasHash {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT rowid, COALESCE(dedupe_key, '') FROM %s WHERE COALESCE(dedupe_hash, '')<>''", table))
	if err != nil {
		return false, fmt.Errorf("query redaction target %s.dedupe_key: %w", table, err)
	}
	type rowValue struct {
		rowID int64
		value string
	}
	var updates []rowValue
	for rows.Next() {
		var current rowValue
		if err := rows.Scan(&current.rowID, &current.value); err != nil {
			_ = rows.Close()
			return false, err
		}
		redacted := store.redact(current.value)
		if redacted != current.value {
			updates = append(updates, rowValue{rowID: current.rowID, value: redacted})
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET dedupe_key=? WHERE rowid=?", table), update.value, update.rowID); err != nil {
			return false, fmt.Errorf("redact %s.dedupe_key: %w", table, err)
		}
	}
	return len(updates) > 0, nil
}

func (store *Store) redactColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	exists, err := redactionColumnExists(ctx, tx, table, column)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT rowid, COALESCE(%s, '') FROM %s", column, table))
	if err != nil {
		return false, fmt.Errorf("query redaction target %s.%s: %w", table, column, err)
	}
	type rowValue struct {
		rowID int64
		value string
	}
	var updates []rowValue
	for rows.Next() {
		var current rowValue
		if err := rows.Scan(&current.rowID, &current.value); err != nil {
			_ = rows.Close()
			return false, err
		}
		redacted := store.redact(current.value)
		if redacted != current.value {
			updates = append(updates, rowValue{rowID: current.rowID, value: redacted})
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s=? WHERE rowid=?", table, column), update.value, update.rowID); err != nil {
			return false, fmt.Errorf("redact %s.%s: %w", table, column, err)
		}
	}
	return len(updates) > 0, nil
}

func redactionColumnExists(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
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

func (store *Store) compactRedactedPages(ctx context.Context) error {
	notifyCompactRedactedPages()
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint redacted sqlite pages: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum redacted sqlite pages: %w", err)
	}
	return nil
}
