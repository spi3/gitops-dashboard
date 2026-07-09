package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/example/gitops-dashboard/internal/sanitizer"
)

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
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, target := range []struct {
		table  string
		column string
	}{
		{table: "repositories", column: "url"},
		{table: "repositories", column: "error"},
		{table: "scans", column: "error"},
		{table: "status_results", column: "message"},
		{table: "status_history", column: "message"},
	} {
		columnChanged, err := store.redactColumn(ctx, tx, target.table, target.column)
		if err != nil {
			return err
		}
		changed = changed || columnChanged
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return store.compactRedactedPages(ctx)
}

func (store *Store) redactColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
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

func (store *Store) compactRedactedPages(ctx context.Context) error {
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint redacted sqlite pages: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum redacted sqlite pages: %w", err)
	}
	return nil
}
