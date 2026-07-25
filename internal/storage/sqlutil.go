package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func sqlPlaceholders(count int) string {
	parts := make([]string, 0, count)
	for i := 0; i < count; i++ {
		parts = append(parts, "?")
	}
	return strings.Join(parts, ",")
}

func dedupeStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	unique := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		unique = append(unique, item)
	}
	return unique
}

func chunkStrings(items []string, size int) [][]string {
	if size <= 0 {
		return [][]string{items}
	}
	chunks := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[start:end])
	}
	return chunks
}

func deleteByServiceAndTargets(ctx context.Context, tx *sql.Tx, table string, serviceID string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	placeholders := sqlPlaceholders(len(targets))
	args := make([]any, 0, len(targets)+1)
	args = append(args, serviceID)
	for _, target := range targets {
		args = append(args, target)
	}
	query := "DELETE FROM " + table + " WHERE service_id=? AND target IN (" + placeholders + ")"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete %s rows: %w", table, err)
	}
	return nil
}

// sqlExecutor lets a helper run both inside a transaction (*sql.Tx) and
// directly against store.db (*sql.DB).
type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func deleteStatusForService(ctx context.Context, tx *sql.Tx, serviceID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=?`, serviceID); err != nil {
		return fmt.Errorf("delete status_results rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=?`, serviceID); err != nil {
		return fmt.Errorf("delete status_history rows: %w", err)
	}
	return nil
}

func deleteStatusForServiceTargetPrefix(ctx context.Context, tx *sql.Tx, serviceID, prefix string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target LIKE ?`, serviceID, prefix); err != nil {
		return fmt.Errorf("delete status_results rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target LIKE ?`, serviceID, prefix); err != nil {
		return fmt.Errorf("delete status_history rows: %w", err)
	}
	return nil
}

func deleteStatusHistoryForServiceTarget(ctx context.Context, exec sqlExecutor, serviceID, target string) error {
	if _, err := exec.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target=?`, serviceID, target); err != nil {
		return fmt.Errorf("delete status_history rows: %w", err)
	}
	return nil
}

// deleteStatusHistoryBefore runs both inside a transaction and directly on
// store.db (age-cutoff prune), hence the sqlExecutor parameter.
func deleteStatusHistoryBefore(ctx context.Context, exec sqlExecutor, cutoff string) (sql.Result, error) {
	result, err := exec.ExecContext(ctx, `DELETE FROM status_history WHERE checked_at < ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("delete status_history rows: %w", err)
	}
	return result, nil
}
